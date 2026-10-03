package agents

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// rpcConn speaks Codex's app-server protocol: JSON-RPC 2.0 messages, one per line, without the
// "jsonrpc" field (codex-rs/app-server-protocol/src/rpc.rs).
type rpcConn struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	wmu   sync.Mutex

	mu             sync.Mutex
	nextID         int64
	pending        map[int64]chan rpcResult
	pendingMethods map[int64]string
	closed         bool
	err            error
	done           chan struct{}

	stderrMu   sync.Mutex
	stderrTail []byte

	onNotify  func(method string, params json.RawMessage)
	onRequest func(id json.RawMessage, method string, params json.RawMessage)
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

type rpcResult struct {
	result json.RawMessage
	err    error
}

// startRPC launches cmd and starts reading its stdout. Handlers run on the reader goroutine and must
// not block (requests are dispatched on their own goroutines by the caller).
func startRPC(cmd *exec.Cmd, onNotify func(string, json.RawMessage), onRequest func(json.RawMessage, string, json.RawMessage)) (*rpcConn, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	c := &rpcConn{cmd: cmd, stdin: stdin, pending: map[int64]chan rpcResult{}, done: make(chan struct{}),
		onNotify: onNotify, onRequest: onRequest}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go c.readStderr(stderr)
	go c.readLoop(stdout)
	return c, nil
}

func (c *rpcConn) readStderr(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			c.stderrMu.Lock()
			c.stderrTail = append(c.stderrTail, buf[:n]...)
			if len(c.stderrTail) > 4096 {
				c.stderrTail = c.stderrTail[len(c.stderrTail)-4096:]
			}
			c.stderrMu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (c *rpcConn) stderrText() string {
	c.stderrMu.Lock()
	defer c.stderrMu.Unlock()
	return strings.TrimSpace(string(c.stderrTail))
}

var ErrRPCResponseTooLarge = errors.New("Codex 返回记录超过 64 MiB 读取上限，请在电脑查看")
var ErrRPCDeliveryTooLarge = errors.New("command_delivery_uncertain: Codex 返回记录超过读取上限，操作可能已执行，请核对原会话；不会自动重发")

// An oversized frame has no safely decoded ID. Settle only the calls already
// waiting when it was read, without cancelling work or closing the connection.
// Mutating calls cannot be reported as definitely unsent.
func (c *rpcConn) takeOversizedPending() (map[int64]chan rpcResult, map[int64]string) {
	c.mu.Lock()
	pending, methods := c.pending, c.pendingMethods
	c.pending = map[int64]chan rpcResult{}
	c.pendingMethods = map[int64]string{}
	c.mu.Unlock()
	return pending, methods
}

func failOversizedPending(pending map[int64]chan rpcResult, methods map[int64]string) {
	for id, ch := range pending {
		err := ErrRPCDeliveryTooLarge
		switch methods[id] {
		case "thread/read", "thread/list", "thread/loaded/list", "model/list", "account/read", "account/rateLimits/read":
			err = ErrRPCResponseTooLarge
		}
		ch <- rpcResult{err: err}
	}
}

func (c *rpcConn) readMessages(r io.Reader, limit int) error {
	readerSize := 1 << 20
	if limit > 0 && limit < readerSize {
		readerSize = limit
	}
	br := bufio.NewReaderSize(r, readerSize)
	for {
		var oversized map[int64]chan rpcResult
		var methods map[int64]string
		line, err := readBoundedLine(br, limit, func() { oversized, methods = c.takeOversizedPending() })
		if errors.Is(err, ErrLineTooLarge) {
			failOversizedPending(oversized, methods)
			continue
		}
		if len(line) > 0 {
			c.handleLine(line)
		}
		if err != nil {
			return err
		}
	}
}

func (c *rpcConn) readLoop(r io.Reader) {
	_ = c.readMessages(r, MaxAgentLineBytes)
	werr := c.cmd.Wait()
	msg := "Codex 进程已退出"
	if werr != nil {
		msg += "：" + werr.Error()
	}
	if tail := c.stderrText(); tail != "" {
		msg += "\n" + truncTail(tail, 800)
	}
	c.shutdown(errors.New(msg))
}

func (c *rpcConn) handleLine(line []byte) {
	var m rpcMessage
	if err := json.Unmarshal(line, &m); err != nil {
		return
	}
	hasID := len(m.ID) > 0 && string(m.ID) != "null"
	switch {
	case m.Method != "" && hasID:
		if c.onRequest != nil {
			c.onRequest(m.ID, m.Method, m.Params)
		}
	case m.Method != "":
		if c.onNotify != nil {
			c.onNotify(m.Method, m.Params)
		}
	case hasID:
		id, err := strconv.ParseInt(strings.Trim(string(m.ID), `"`), 10, 64)
		if err != nil {
			return
		}
		c.mu.Lock()
		ch := c.pending[id]
		delete(c.pending, id)
		delete(c.pendingMethods, id)
		c.mu.Unlock()
		if ch == nil {
			return
		}
		if m.Error != nil {
			ch <- rpcResult{err: m.Error}
		} else {
			ch <- rpcResult{result: m.Result}
		}
	}
}

func (c *rpcConn) shutdown(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.err = err
	pend := c.pending
	c.pending = map[int64]chan rpcResult{}
	c.pendingMethods = map[int64]string{}
	c.mu.Unlock()
	for _, ch := range pend {
		ch <- rpcResult{err: err}
	}
	close(c.done)
}

func (c *rpcConn) alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed
}

func (c *rpcConn) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.stdin.Write(b)
	return err
}

// Call sends a request and waits for its response (decoded into out when non-nil).
func (c *rpcConn) Call(ctx context.Context, method string, params any, out any) error {
	c.mu.Lock()
	if c.closed {
		err := c.err
		c.mu.Unlock()
		return err
	}
	c.nextID++
	id := c.nextID
	ch := make(chan rpcResult, 1)
	c.pending[id] = ch
	if c.pendingMethods == nil {
		c.pendingMethods = map[int64]string{}
	}
	c.pendingMethods[id] = method
	c.mu.Unlock()

	req := map[string]any{"id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	if err := c.write(req); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		delete(c.pendingMethods, id)
		c.mu.Unlock()
		return err
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		if out != nil && len(r.result) > 0 {
			return json.Unmarshal(r.result, out)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		delete(c.pendingMethods, id)
		c.mu.Unlock()
		return ctx.Err()
	}
}

func (c *rpcConn) Notify(method string, params any) error {
	m := map[string]any{"method": method}
	if params != nil {
		m["params"] = params
	}
	return c.write(m)
}

func (c *rpcConn) Reply(id json.RawMessage, result any) error {
	return c.write(map[string]any{"id": id, "result": result})
}

func (c *rpcConn) ReplyError(id json.RawMessage, code int64, msg string) error {
	return c.write(map[string]any{"id": id, "error": map[string]any{"code": code, "message": msg}})
}

// Kill stops the process tree.
func (c *rpcConn) Kill() {
	_ = c.stdin.Close()
	killTree(c.cmd)
}
