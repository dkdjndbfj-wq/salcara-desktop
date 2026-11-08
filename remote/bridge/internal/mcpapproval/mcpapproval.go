// Package mcpapproval is the tiny stdio MCP server Claude Code launches as its --permission-prompt-tool
// (`<bridge> mcp-approval --port N --token T --session S`). Each `approve` call is forwarded to the
// running bridge on 127.0.0.1, which asks the phone and answers.
package mcpapproval

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Timeout for one decision (the bridge itself gives up after 10 minutes).
const decisionTimeout = 11 * time.Minute

const defaultProtocolVersion = "2025-06-18"

const (
	maxMessageBytes    = 1 << 20
	maxInFlight        = 32
	maxSessionRequests = 65_536
	maxRequestIDBytes  = 256
	maxDecisionBytes   = 2 << 20
	shutdownTimeout    = time.Second
)

var errMessageTooLarge = errors.New("MCP input line exceeds the size limit")

type pendingCall struct {
	ctx    context.Context
	cancel context.CancelFunc
}

type message struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Run serves MCP on stdin/stdout until stdin closes.
func Run(port int, token, session string) error {
	return Serve(os.Stdin, os.Stdout, fmt.Sprintf("http://127.0.0.1:%d/internal/agents/claude/permission", port), token, session)
}

// Serve is Run with explicit streams and endpoint (used by tests).
func Serve(in io.Reader, out io.Writer, endpoint, token, session string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wmu sync.Mutex
	writeLocked := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		_, _ = out.Write(append(b, '\n'))
	}
	write := func(v any) {
		wmu.Lock()
		defer wmu.Unlock()
		writeLocked(v)
	}
	reply := func(id json.RawMessage, result any) {
		write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	replyErr := func(id json.RawMessage, code int, msg string) {
		write(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
	}
	client := &http.Client{Timeout: decisionTimeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		// The loopback approval endpoint is not a redirectable resource. In
		// particular a redirect must not forward the scoped permission token.
		return http.ErrUseLastResponse
	}}
	defer client.CloseIdleConnections()
	var wg sync.WaitGroup
	var mu sync.Mutex
	pending := make(map[string]*pendingCall)
	// MCP request IDs must not be reused during one connection. Keeping bounded
	// tombstones also stops a delayed cancellation targeting a newer approval.
	seen := make(map[string]struct{})
	defer func() {
		cancel()
		mu.Lock()
		for _, call := range pending {
			call.cancel()
		}
		mu.Unlock()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(shutdownTimeout):
			// A caller-supplied Writer can block forever and cannot be cancelled
			// through io.Writer. Do not let it keep the helper alive after EOF.
		}
	}()
	br := bufio.NewReaderSize(in, 64<<10)
	for {
		line, err := boundedLine(br)
		if err != nil && err != io.EOF {
			return err
		}
		if len(bytes.TrimSpace(line)) > 0 {
			var m message
			if uniqueObject(line, &m) == nil && m.JSONRPC == "2.0" {
				hasID := len(m.ID) > 0
				key, validID := requestKey(m.ID)
				if hasID {
					if !validID {
						return errors.New("invalid MCP request ID")
					}
					if _, duplicate := seen[key]; duplicate {
						// There is no unambiguous response for two requests sharing
						// an ID. Close this connection and cancel pending work.
						return errors.New("reused MCP request ID")
					}
					if len(seen) >= maxSessionRequests {
						return errors.New("MCP session request limit reached")
					}
					seen[key] = struct{}{}
				}
				if m.Method == "notifications/cancelled" && !hasID {
					var p struct {
						RequestID json.RawMessage `json:"requestId"`
					}
					if uniqueObject(m.Params, &p) == nil {
						if id, ok := requestKey(p.RequestID); ok {
							mu.Lock()
							if call := pending[id]; call != nil {
								call.cancel()
							}
							mu.Unlock()
						}
					}
					if err == io.EOF {
						return nil
					}
					continue
				}
				// tools/call without an ID is not an approval request. In
				// particular it must not cause an unanswerable side effect.
				if !hasID {
					if err != nil {
						return nil
					}
					continue
				}
				switch m.Method {
				case "initialize":
					var p struct {
						ProtocolVersion string `json:"protocolVersion"`
					}
					_ = json.Unmarshal(m.Params, &p)
					if p.ProtocolVersion == "" {
						p.ProtocolVersion = defaultProtocolVersion
					}
					reply(m.ID, map[string]any{
						"protocolVersion": p.ProtocolVersion,
						"capabilities":    map[string]any{"tools": map[string]any{}},
						"serverInfo":      map[string]any{"name": "salcara", "version": "1.0.0"},
					})
				case "ping":
					reply(m.ID, map[string]any{})
				case "tools/list":
					reply(m.ID, map[string]any{"tools": []any{toolDef()}})
				case "tools/call":
					mu.Lock()
					if len(pending) >= maxInFlight {
						mu.Unlock()
						replyErr(m.ID, -32000, "too many pending approvals")
						break
					}
					callCtx, callCancel := context.WithCancel(ctx)
					call := &pendingCall{ctx: callCtx, cancel: callCancel}
					pending[key] = call
					mu.Unlock()
					wg.Add(1)
					go func(m message, key string, call *pendingCall) {
						defer wg.Done()
						defer call.cancel()
						result := callApprove(call.ctx, client, endpoint, token, session, m.Params)
						wmu.Lock()
						defer wmu.Unlock()
						mu.Lock()
						// Completion/cancellation linearizes here; never send a
						// cancelled request's late allow into the host stream.
						live := pending[key] == call && call.ctx.Err() == nil
						delete(pending, key)
						mu.Unlock()
						if live {
							writeLocked(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
						}
					}(m, key, call)
				default:
					if hasID {
						replyErr(m.ID, -32601, "method not found: "+m.Method)
					}
					// notifications (notifications/initialized, cancelled, …) need no answer
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// Unlike ReadBytes, this never accumulates an unbounded line before rejecting it.
func boundedLine(br *bufio.Reader) ([]byte, error) {
	line := make([]byte, 0, 4096)
	for {
		part, err := br.ReadSlice('\n')
		if len(line)+len(part) > maxMessageBytes {
			return nil, errMessageTooLarge
		}
		line = append(line, part...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
		if len(line) == maxMessageBytes {
			// ReadSlice otherwise waits for another full reader buffer, EOF or
			// a delimiter. A peer sending max+1 bytes with stdin still open
			// must be rejected immediately, not hold up all cancellation input.
			if _, nextErr := br.ReadByte(); nextErr != nil {
				return line, nextErr
			}
			return nil, errMessageTooLarge
		}
	}
}

// Request IDs are strings or integers, with distinct numeric/string domains.
// No float conversion: large integer IDs must not collide through precision loss.
func requestKey(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > maxRequestIDBytes || !json.Valid(raw) {
		return "", false
	}
	if raw[0] == '"' {
		var value string
		if json.Unmarshal(raw, &value) == nil {
			return "s:" + value, true
		}
		return "", false
	}
	value := string(raw)
	if value == "-0" {
		value = "0"
	}
	for i, c := range value {
		if c < '0' || c > '9' {
			if i != 0 || c != '-' {
				return "", false
			}
		}
	}
	return "n:" + value, true
}

// Reject ambiguous envelope keys instead of letting encoding/json choose the
// last ID, method or requestId. Nested tool input remains the caller's payload.
func uniqueObject(raw []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return errors.New("expected JSON object")
	}
	keys := map[string]bool{}
	for d.More() {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		key, ok := tok.(string)
		if !ok {
			return errors.New("invalid object key")
		}
		key = strings.ToLower(key)
		if keys[key] {
			return errors.New("duplicate object key")
		}
		keys[key] = true
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return err
		}
	}
	if _, err := d.Token(); err != nil {
		return err
	}
	var tail any
	if d.Decode(&tail) != io.EOF {
		return errors.New("trailing JSON data")
	}
	return json.Unmarshal(raw, dst)
}

func toolDef() map[string]any {
	return map[string]any{
		"name":        "approve",
		"description": "Ask the Salcara phone app whether Claude Code may use a tool.",
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tool_name":   map[string]any{"type": "string"},
				"input":       map[string]any{"type": "object"},
				"tool_use_id": map[string]any{"type": "string"},
			},
			"required": []string{"tool_name", "input"},
		},
	}
}

func textResult(v any) map[string]any {
	b, _ := json.Marshal(v)
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}}
}

func deny(msg string) map[string]any {
	return textResult(map[string]any{"behavior": "deny", "message": msg})
}

func callApprove(ctx context.Context, client *http.Client, endpoint, token, session string, params json.RawMessage) map[string]any {
	var p struct {
		Name      string `json:"name"`
		Arguments struct {
			ToolName  string          `json:"tool_name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Name != "approve" {
		return deny("未知的审批请求")
	}
	input := p.Arguments.Input
	if len(input) == 0 || string(input) == "null" {
		input = json.RawMessage("{}")
	}
	body, _ := json.Marshal(map[string]any{"session": session, "tool_name": p.Arguments.ToolName,
		"input": input, "tool_use_id": p.Arguments.ToolUseID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return deny("无法联系 Salcara 桥接程序")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Salcara-Token", token)
	resp, err := client.Do(req)
	if err != nil {
		return deny("无法联系 Salcara 桥接程序：" + err.Error())
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxDecisionBytes+1))
	if readErr != nil || len(raw) > maxDecisionBytes {
		return deny("Salcara 桥接程序返回了无效的结果")
	}
	if resp.StatusCode != http.StatusOK {
		return deny(fmt.Sprintf("Salcara 桥接程序拒绝了请求（HTTP %d）", resp.StatusCode))
	}
	var dec struct {
		Behavior     string          `json:"behavior"`
		UpdatedInput json.RawMessage `json:"updatedInput"`
		Message      string          `json:"message"`
	}
	if json.Unmarshal(raw, &dec) != nil {
		return deny("Salcara 桥接程序返回了无效的结果")
	}
	if dec.Behavior == "allow" {
		upd := dec.UpdatedInput
		if len(upd) == 0 || string(upd) == "null" {
			upd = input
		}
		return textResult(map[string]any{"behavior": "allow", "updatedInput": upd})
	}
	if dec.Message == "" {
		dec.Message = "用户拒绝了这个操作"
	}
	return deny(dec.Message)
}
