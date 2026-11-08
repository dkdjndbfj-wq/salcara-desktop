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
	"time"

	"salcara/bridge/internal/protocol"
)

// rpcConn speaks Codex's app-server protocol: JSON-RPC 2.0 messages, one per line, without the
// "jsonrpc" field (codex-rs/app-server-protocol/src/rpc.rs).
type rpcConn struct {
	jsonrpc string // empty for Codex, "2.0" for the DeepSeek Harness SDK
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	stderr  io.ReadCloser
	wmu     sync.Mutex

	killOnce        sync.Once
	killCh          chan struct{}
	processWaitDone chan struct{}
	treeTermination processTreeTermination

	mu                      sync.Mutex
	nextID                  int64
	pending                 map[int64]chan rpcResult
	pendingMethods          map[int64]string
	closed                  bool
	err                     error
	done                    chan struct{}
	initializeUA            string // native initialize response; never a CLI path/version probe
	latestRevertSupported   bool
	legacyRollbackSupported bool
	actionsEpoch            string             // bound to this initialized connection
	sessionActionObservers  map[int64]struct{} // includes received but not yet claimed late acknowledgements
	permissionObservers     map[string]chan codexPermissionReply

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

func (e *rpcError) ExecutionErrorCode() string {
	var data cxTurnError
	if json.Unmarshal(e.Data, &data) == nil {
		if code := data.executionCode(); code != "" {
			return code
		}
	}
	return protocol.CapacityTextCode(e.Message)
}

type rpcResult struct {
	result json.RawMessage
	err    error
}

// startRPC launches cmd and starts reading its stdout. Handlers run on the reader goroutine and must
// not block (requests are dispatched on their own goroutines by the caller).
func startRPC(cmd *exec.Cmd, onNotify func(string, json.RawMessage), onRequest func(json.RawMessage, string, json.RawMessage)) (*rpcConn, error) {
	return startRPCProtocol(cmd, "", onNotify, onRequest)
}

func startRPCProtocol(cmd *exec.Cmd, version string, onNotify func(string, json.RawMessage), onRequest func(json.RawMessage, string, json.RawMessage)) (*rpcConn, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		closeRPCStartupPipes(cmd, stdin, nil, nil)
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		closeRPCStartupPipes(cmd, stdin, stdout, nil)
		return nil, err
	}
	c := &rpcConn{cmd: cmd, jsonrpc: version, stdin: stdin, stdout: stdout, stderr: stderr, killCh: make(chan struct{}),
		processWaitDone: make(chan struct{}), pending: map[int64]chan rpcResult{}, done: make(chan struct{}),
		onNotify: onNotify, onRequest: onRequest}
	if err := cmd.Start(); err != nil {
		closeRPCStartupPipes(cmd, stdin, stdout, stderr)
		return nil, err
	}
	go c.readStderr(stderr)
	go c.readLoop(stdout)
	return c, nil
}

// Before Start, the command also owns the other (child-facing) pipe ends. A
// failed pipe setup never reaches exec.Cmd.Start's cleanup, so close both ends
// of only those pipes this function actually created; never close a caller's
// preconfigured stdout/stderr on an error.
func closeRPCStartupPipes(cmd *exec.Cmd, stdin io.WriteCloser, stdout, stderr io.ReadCloser) {
	closePair := func(parent io.Closer, child any) {
		if parent == nil {
			return
		}
		_ = parent.Close()
		if closer, ok := child.(io.Closer); ok {
			_ = closer.Close()
		}
	}
	closePair(stdin, cmd.Stdin)
	closePair(stdout, cmd.Stdout)
	closePair(stderr, cmd.Stderr)
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
var ErrRPCDeliveryTooLarge = &rpcDeliveryUncertainError{message: "Codex 返回记录超过读取上限，操作可能已执行，请核对原会话；不会自动重发"}
var errRPCDeliveryInterrupted = &rpcDeliveryUncertainError{message: "Codex 连接未完成确认，操作可能已执行，请核对原会话；不会自动重发"}

type rpcDeliveryUncertainError struct {
	message string
	cause   error
}

func (e *rpcDeliveryUncertainError) Error() string            { return e.message }
func (e *rpcDeliveryUncertainError) Unwrap() error            { return e.cause }
func (*rpcDeliveryUncertainError) ExecutionErrorCode() string { return "command_delivery_uncertain" }

var errRPCProcessExitUnconfirmed = errors.New("Codex 连接已关闭，但进程树退出尚未确认")

const rpcKillWaitTimeout = 2 * time.Second

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
		var err error = ErrRPCDeliveryTooLarge
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
	// The reader has fully stopped before done can be closed. A replaced
	// connection therefore cannot emit late buffered notifications into its
	// successor. Normal EOF continues to drain before waiting for the process.
	werr := c.waitForProcessExit()
	msg := "Codex 进程已退出"
	if errors.Is(werr, errRPCProcessExitUnconfirmed) {
		c.shutdown(werr)
		return
	}
	if werr != nil {
		msg += "：" + werr.Error()
	}
	if tail := c.stderrText(); tail != "" {
		msg += "\n" + truncTail(tail, 800)
	}
	c.shutdown(errors.New(msg))
}

func (c *rpcConn) waitForProcessExit() error {
	// Exactly one goroutine reaps this owned process. On a forced shutdown an
	// OS failure must not keep callers waiting indefinitely; the reaper stays
	// alive until the OS returns, without retaining a live RPC transport.
	waited := make(chan error, 1)
	go func() {
		err := c.cmd.Wait()
		// This signal is OS wait completion, never transport retirement. It
		// publishes ProcessState and permits the owner to keep a failed-kill
		// worker fenced until its actual exit is confirmed.
		close(c.processWaitDone)
		waited <- err
	}()
	select {
	case err := <-waited:
		if !c.processExitConfirmed() {
			return errRPCProcessExitUnconfirmed
		}
		return err
	case <-c.killCh:
		timer := time.NewTimer(rpcKillWaitTimeout)
		defer timer.Stop()
		select {
		case err := <-waited:
			if !c.processExitConfirmed() {
				return errRPCProcessExitUnconfirmed
			}
			return err
		case <-timer.C:
			return errRPCProcessExitUnconfirmed
		}
	}
}

func (c *rpcConn) processExitConfirmed() bool {
	return c != nil && !c.treeTermination.unconfirmed() && c.rootProcessExitConfirmed()
}

func (c *rpcConn) rootProcessExitConfirmed() bool {
	if c == nil || c.processWaitDone == nil {
		return false
	}
	select {
	case <-c.processWaitDone:
		// Wait also reaps signal-terminated Unix processes. Exited() is false
		// for those, even though the OS has confirmed termination. The wait
		// publication and separate process-tree fence are still required.
		return c.cmd != nil && c.cmd.ProcessState != nil
	default:
		return false
	}
}

func (c *rpcConn) setRequestHandler(handler func(json.RawMessage, string, json.RawMessage)) {
	c.mu.Lock()
	c.onRequest = handler
	c.mu.Unlock()
}

func (c *rpcConn) setNotificationHandler(handler func(string, json.RawMessage)) {
	c.mu.Lock()
	c.onNotify = handler
	c.mu.Unlock()
}

func (c *rpcConn) handleLine(line []byte) {
	var m rpcMessage
	if err := json.Unmarshal(line, &m); err != nil {
		return
	}
	hasID := len(m.ID) > 0 && string(m.ID) != "null"
	switch {
	case m.Method != "" && hasID:
		c.mu.Lock()
		handler := c.onRequest
		c.mu.Unlock()
		if handler != nil {
			handler(m.ID, m.Method, m.Params)
		}
	case m.Method != "":
		if m.Method == "thread/settings/updated" {
			c.observeCodexPermission(m.Params)
		}
		c.mu.Lock()
		handler := c.onNotify
		c.mu.Unlock()
		if handler != nil {
			handler(m.Method, m.Params)
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
	// Serialize this final error publication with Kill's pending marker.
	// done makes err immutable for callers, even if Wait computed an older
	// root-only result before forced tree cleanup began.
	if c.treeTermination.unconfirmed() {
		err = errRPCProcessExitUnconfirmed
	}
	c.closed = true
	c.err = err
	pend := c.pending
	methods := c.pendingMethods
	c.pending = map[int64]chan rpcResult{}
	c.pendingMethods = map[int64]string{}
	c.sessionActionObservers = nil
	c.mu.Unlock()
	for id, ch := range pend {
		pendingErr := err
		if !rpcShutdownReadOnly(methods[id]) {
			pendingErr = errRPCDeliveryInterrupted
		}
		ch <- rpcResult{err: pendingErr}
	}
	close(c.done)
}

func rpcShutdownReadOnly(method string) bool {
	switch method {
	case "initialize", "config/read", "thread/read", "thread/list", "thread/loaded/list", "thread/turns/list", "thread/goal/get", "model/list", "account/read", "account/rateLimits/read":
		return true
	default:
		return false
	}
}

func (c *rpcConn) alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed && !c.transportRetired()
}

func (c *rpcConn) transportRetired() bool {
	if c.treeTermination.unconfirmed() {
		return true
	}
	select {
	case <-c.killCh:
		return true
	default:
		return false
	}
}

func (c *rpcConn) write(v any) error {
	_, err := c.writeContext(context.Background(), v)
	return err
}

func (c *rpcConn) writeContext(ctx context.Context, v any) (attempted bool, err error) {
	return c.writeGuarded(ctx, v, nil)
}

func (c *rpcConn) writeGuarded(ctx context.Context, v any, beforeWrite func() error) (attempted bool, err error) {
	if c.jsonrpc != "" {
		if frame, ok := v.(map[string]any); ok {
			frame["jsonrpc"] = c.jsonrpc
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return false, err
	}
	b = append(b, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	// A queued caller can expire while another pipe write holds wmu. It must
	// not dispatch its task after its caller has already abandoned it.
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if beforeWrite != nil {
		if err := beforeWrite(); err != nil {
			return false, err
		}
	}
	if c.transportRetired() {
		return false, errRPCProcessExitUnconfirmed
	}
	n, err := c.stdin.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	return true, err
}

// Call sends a request and waits for its response (decoded into out when non-nil).
func (c *rpcConn) Call(ctx context.Context, method string, params any, out any) error {
	return c.callGuarded(ctx, method, params, out, nil)
}

// beforeWrite must not issue RPCs: it runs with the pipe write lock held, after
// any earlier writer has finished. An unsent guard failure is safe to retry.
func (c *rpcConn) callGuarded(ctx context.Context, method string, params any, out any, beforeWrite func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
	if attempted, err := c.writeGuarded(ctx, req, beforeWrite); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		delete(c.pendingMethods, id)
		c.mu.Unlock()
		// Closing an owned pipe can interrupt a write after the server received
		// some or all of it. Do not turn that shutdown race into a safe replay.
		if attempted && !rpcShutdownReadOnly(method) {
			return errRPCDeliveryInterrupted
		}
		return err
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		if out != nil && len(r.result) > 0 {
			err := json.Unmarshal(r.result, out)
			if err != nil && !rpcShutdownReadOnly(method) {
				return errRPCDeliveryInterrupted
			}
			return err
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		delete(c.pendingMethods, id)
		c.mu.Unlock()
		if !rpcShutdownReadOnly(method) {
			return &rpcDeliveryUncertainError{message: errRPCDeliveryInterrupted.message, cause: ctx.Err()}
		}
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

// Kill stops only this owned process tree and retires its transport. In
// particular, an already-exited parent cannot leave its inherited stdout open
// forever through an orphaned child. Readers are closed only on explicit Kill,
// never as a shortcut while normal task output is still being drained.
func (c *rpcConn) Kill() {
	c.killWithTreeKiller(killTreeResult)
}

func (c *rpcConn) killWithTreeKiller(terminate func(*exec.Cmd) error) {
	c.killOnce.Do(func() {
		c.mu.Lock()
		terminateNeeded := !c.rootProcessExitConfirmed()
		if terminateNeeded {
			c.treeTermination.begin()
		}
		c.mu.Unlock()
		if c.killCh != nil {
			close(c.killCh)
		}
		// Kill before stdin EOF can cause the parent to exit and hide its tree.
		// A normally reaped parent no longer owns its numeric PID. Do not send
		// taskkill to a stale PID when cleanup is called after normal exit.
		if terminateNeeded {
			c.treeTermination.finish(terminate(c.cmd))
		}
		c.closeOwnedPipes()
	})
}

func (c *rpcConn) closeOwnedPipes() {
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.stdout != nil {
		_ = c.stdout.Close()
	}
	if c.stderr != nil {
		_ = c.stderr.Close()
	}
}
