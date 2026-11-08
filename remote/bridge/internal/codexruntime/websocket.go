package codexruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"salcara/bridge/internal/apierror"

	"github.com/gorilla/websocket"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const officialCodexWebsocketURL = "wss://chatgpt.com/backend-api/codex/responses"
const maxNativeWebsocketFrame = 128 << 20
const maxNativeWebsocketTurn = 512 << 20

// A fresh connection belongs to one selected credential and one admitted turn.
// It never enters the SDK's global websocket session pool or follows redirects.
func (r *Runtime) executeWebSocket(ctx context.Context, a *coreauth.Auth, body []byte) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if a == nil || a.Provider != "codex" || strings.TrimSpace(a.ProxyURL) != "" {
		return nil, ErrUnavailable
	}
	token, _ := a.Metadata["access_token"].(string)
	accountID, _ := a.Metadata["account_id"].(string)
	if token == "" || len(token) > 32<<10 || strings.ContainsAny(token+accountID, "\r\n") || len(accountID) > 1024 {
		return nil, ErrUnavailable
	}
	var input map[string]json.RawMessage
	if len(body) > 32<<20 || json.Unmarshal(body, &input) != nil || input == nil {
		return nil, ErrUnavailable
	}
	var model string
	if json.Unmarshal(input["model"], &model) != nil || model == "" {
		return nil, ErrUnavailable
	}
	input["type"] = json.RawMessage(`"response.create"`)
	if _, ok := input["instructions"]; !ok {
		input["instructions"] = json.RawMessage(`""`)
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, ErrUnavailable
	}
	r.mu.Lock()
	closed := r.closed
	target := r.websocketURL
	if !closed && r.websocketContext == nil {
		r.websocketContext, r.websocketStop = context.WithCancel(context.Background())
	}
	ownedContext := r.websocketContext
	r.mu.Unlock()
	if closed {
		return nil, ErrUnavailable
	}
	if target == "" {
		target = officialCodexWebsocketURL
	} else {
		// Test seams cannot create an arbitrary credential destination.
		parsed, e := url.Parse(target)
		if e != nil || parsed.User != nil || parsed.Scheme != "ws" || parsed.RawQuery != "" || !isLoopbackWebsocketHost(parsed.Hostname()) {
			return nil, ErrUnavailable
		}
	}
	streamCtx, cancel := context.WithCancel(ctx)
	stopRuntime := context.AfterFunc(ownedContext, cancel)
	deferredCleanup := true
	defer func() {
		if deferredCleanup {
			stopRuntime()
			cancel()
		}
	}()
	dialer := websocket.Dialer{Proxy: nil, NetDialContext: (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext, HandshakeTimeout: 30 * time.Second}
	headers := http.Header{"Authorization": {"Bearer " + token}, "OpenAI-Beta": {"responses_websockets=2026-02-06"}, "User-Agent": {"salcara-codex-api"}}
	if accountID != "" {
		headers.Set("ChatGPT-Account-Id", accountID)
	}
	conn, handshake, err := dialer.DialContext(streamCtx, target, headers)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if handshake != nil {
			if handshake.Body != nil {
				defer handshake.Body.Close()
			}
			if handshake.StatusCode >= 400 && handshake.StatusCode <= 599 {
				var raw []byte
				if handshake.Body != nil {
					raw, _ = io.ReadAll(io.LimitReader(handshake.Body, maxProviderErrorBytes+1))
				}
				return nil, providerExecutionError{status: handshake.StatusCode, executionCode: providerErrorExecutionCode(string(raw)), retryAfter: apierror.RetryAfterDuration(handshake.Header)}
			}
		}
		return nil, ErrUnavailable
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		cancel()
		conn.Close()
		return nil, ErrUnavailable
	}
	if r.websockets == nil {
		r.websockets = make(map[*websocket.Conn]context.CancelFunc)
	}
	r.websockets[conn] = cancel
	r.mu.Unlock()
	release := func() { _ = conn.Close(); r.mu.Lock(); delete(r.websockets, conn); r.mu.Unlock() }
	stopClose := context.AfterFunc(streamCtx, func() { _ = conn.Close() })
	cleanup := func() { cancel(); stopRuntime(); stopClose(); release() }
	conn.SetReadLimit(maxNativeWebsocketFrame)
	_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if err = conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		cleanup()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrUnavailable
	}
	readEvent := func() ([]byte, bool, error) {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
		kind, raw, e := conn.ReadMessage()
		if e != nil {
			if streamCtx.Err() != nil {
				return nil, false, streamCtx.Err()
			}
			return nil, false, ErrUnavailable
		}
		if kind != websocket.TextMessage || len(raw) == 0 {
			return nil, false, ErrUnavailable
		}
		var event struct {
			Type       string          `json:"type"`
			Status     int             `json:"status"`
			StatusCode int             `json:"status_code"`
			Body       json.RawMessage `json:"body"`
		}
		if json.Unmarshal(raw, &event) != nil || !strings.HasPrefix(event.Type, "response.") && event.Type != "error" {
			return nil, false, ErrUnavailable
		}
		if event.Type == "response.done" {
			var obj map[string]json.RawMessage
			_ = json.Unmarshal(raw, &obj)
			obj["type"] = json.RawMessage(`"response.completed"`)
			raw, _ = json.Marshal(obj)
			event.Type = "response.completed"
		}
		terminal := event.Type == "response.completed" || event.Type == "response.failed" || event.Type == "response.incomplete" || event.Type == "error"
		// Error diagnostics crossing the boundary use bounded fixed messages.
		if event.Type == "error" || event.Type == "response.failed" {
			status := event.Status
			if status == 0 {
				status = event.StatusCode
			}
			if status < 400 || status > 599 {
				status = 502
			}
			code := providerErrorExecutionCode(string(raw))
			if code == "" && len(event.Body) > 0 {
				code = providerErrorExecutionCode(string(event.Body))
			}
			return nil, true, providerExecutionError{status: status, executionCode: code}
		}
		return raw, terminal, nil
	}
	first, terminal, err := readEvent()
	if err != nil {
		cleanup()
		return nil, err
	}
	reader, writer := io.Pipe()
	stopPipe := context.AfterFunc(streamCtx, func() { _ = writer.CloseWithError(streamCtx.Err()) })
	go func() {
		defer stopPipe()
		defer cleanup()
		defer writer.Close()
		total := 0
		raw, last := first, terminal
		for {
			total += len(raw)
			if total > maxNativeWebsocketTurn {
				_ = writer.CloseWithError(ErrUnavailable)
				return
			}
			framed := bytes.Join([][]byte{[]byte("data: "), raw, []byte("\n\n")}, nil)
			if _, e := writer.Write(framed); e != nil {
				return
			}
			if last {
				return
			}
			raw, last, err = readEvent()
			if err != nil {
				if safe, ok := err.(providerExecutionError); ok {
					message := "上游请求失败"
					code := "upstream_error"
					if safe.executionCode == "model_capacity" {
						message = "Selected model is at capacity. Please try a different model."
						code = "model_capacity"
					}
					event, _ := json.Marshal(map[string]any{"type": "error", "status": safe.status, "error": map[string]string{"type": "salcara_gateway_error", "code": code, "message": message}})
					_, _ = writer.Write(bytes.Join([][]byte{[]byte("data: "), event, []byte("\n\n")}, nil))
				}
				_ = writer.CloseWithError(err)
				return
			}
		}
	}()
	deferredCleanup = false
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &cancelBody{ReadCloser: reader, cancel: cancel}}, nil
}
func isLoopbackWebsocketHost(host string) bool {
	ip := net.ParseIP(host)
	return strings.EqualFold(host, "localhost") || ip != nil && ip.IsLoopback()
}
