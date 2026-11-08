package hubclient

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"salcara/bridge/internal/config"
)

const terminalBufferLimit = 512 << 10
const terminalReadLimit = 48 << 10

var errTerminalScope = errors.New("terminal_scope_changed: 终端所属电脑、会话或项目已变化，请重新打开")

type terminalProcess interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
	Wait() (int, error)
}

type workspaceTerminal struct {
	mu                sync.Mutex
	writeMu           sync.Mutex
	id, openID        string
	scope             workspaceFileScope
	rootInfo          os.FileInfo
	process           terminalProcess
	buffer            []byte
	start, end        int64
	nextSequence      int64
	lastInput         [32]byte
	lastInputAccepted bool
	lastTouch         time.Time
	exited            bool
	exitCode          int
	closed            bool
	done              chan struct{}
}

type terminalReply struct {
	TerminalID   string `json:"terminalId"`
	Cursor       int64  `json:"cursor"`
	StartCursor  int64  `json:"startCursor"`
	Data         string `json:"dataBase64"`
	Truncated    bool   `json:"truncated"`
	Exited       bool   `json:"exited"`
	ExitCode     int    `json:"exitCode"`
	NextSequence int64  `json:"nextSequence"`
}

// Terminals are independent PTYs, scoped to an authenticated phone generation
// and an existing session in a desktop-authorized project. They never execute
// inside an Agent worker or inherit its API/pairing environment.
func (c *Client) dispatchWorkspaceTerminal(ctx context.Context, cmd map[string]any) (any, error) {
	typ := str(cmd, "type")
	allowed := map[string]bool{"type": true, "sessionKey": true, "controlSurface": true, "expectedCwd": true}
	switch typ {
	case "workspace.terminal.open":
		allowed["operationId"], allowed["cols"], allowed["rows"] = true, true, true
	case "workspace.terminal.read":
		allowed["terminalId"], allowed["cursor"] = true, true
	case "workspace.terminal.input":
		allowed["terminalId"], allowed["sequence"], allowed["data"] = true, true, true
	case "workspace.terminal.resize":
		allowed["terminalId"], allowed["cols"], allowed["rows"] = true, true, true
	case "workspace.terminal.close":
		allowed["terminalId"] = true
	default:
		return nil, errors.New("terminal_command_invalid")
	}
	for k := range cmd {
		if !allowed[k] {
			return nil, errors.New("terminal_command_invalid")
		}
	}
	key, surface, expected := str(cmd, "sessionKey"), str(cmd, "controlSurface"), str(cmd, "expectedCwd")
	if key == "" || len(key) > 256 || (surface != "cli" && surface != "desktop") || expected == "" || len(expected) > 4096 || !utf8.ValidString(expected) || c.o.Store == nil {
		return nil, errTerminalScope
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cwd, err := c.workspaceSessionCwd(ctx, key, surface)
	if err != nil || !workspaceExpectedCwdMatches(expected, cwd) {
		return nil, errTerminalScope
	}
	c.taskConfigMu.Lock()
	defer c.taskConfigMu.Unlock()
	if err := c.checkUpdateCommand(cmd); err != nil {
		return nil, err
	}
	if err = c.validateCommandGuard(ctx, typ); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	cfg := c.o.Store.Get()
	root, err := workspaceConfiguredRoot(cfg, cwd)
	if err != nil {
		return nil, errTerminalScope
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return nil, errTerminalScope
	}
	scope := workspaceFileScope{key, surface, root, config.RemoteIdentity(cfg), cfg.DeviceID, cfg.PhoneBindingID, cfg.PhoneHash}
	c.terminalMu.Lock()
	defer c.terminalMu.Unlock()
	if c.terminals == nil {
		c.terminals = make(map[string]*workspaceTerminal)
	}
	// Revoke old processes before admitting work for a replacement binding.
	for id, t := range c.terminals {
		if t.scope.identity != scope.identity || t.scope.device != scope.device || t.scope.binding != scope.binding || t.scope.phone != scope.phone {
			t.stop()
			delete(c.terminals, id)
		}
	}
	if typ == "workspace.terminal.open" {
		openID := str(cmd, "operationId")
		cols, ok := terminalInteger(cmd["cols"], 20, 400)
		if !ok {
			return nil, errors.New("terminal_size_invalid")
		}
		rows, ok := terminalInteger(cmd["rows"], 5, 200)
		if !ok || !terminalIdentifier(openID) {
			return nil, errors.New("terminal_open_invalid")
		}
		for _, t := range c.terminals {
			if t.openID == openID {
				if t.scope != scope {
					return nil, errTerminalScope
				}
				return t.snapshot(0), nil
			}
		}
		if len(c.terminals) >= 4 {
			return nil, errors.New("terminal_limit: 请先关闭一个终端")
		}
		process, err := startTerminalProcess(root, int(cols), int(rows), terminalEnvironment())
		if err != nil {
			return nil, errors.New("terminal_start_failed: 无法启动电脑终端：" + err.Error())
		}
		bytes := make([]byte, 24)
		if _, err = rand.Read(bytes); err != nil {
			process.Close()
			return nil, err
		}
		t := &workspaceTerminal{id: base64.RawURLEncoding.EncodeToString(bytes), openID: openID, scope: scope, rootInfo: rootInfo, process: process, nextSequence: 1, lastTouch: time.Now(), done: make(chan struct{})}
		c.terminals[t.id] = t
		go t.collect()
		go c.watchTerminal(t)
		return t.snapshot(0), nil
	}
	t := c.terminals[str(cmd, "terminalId")]
	if t == nil || t.scope != scope || !os.SameFile(rootInfo, t.rootInfo) {
		return nil, errTerminalScope
	}
	switch typ {
	case "workspace.terminal.read":
		cursor, ok := terminalInteger(cmd["cursor"], 0, 1<<52)
		if !ok {
			return nil, errors.New("terminal_cursor_invalid")
		}
		t.mu.Lock()
		future := cursor > t.end
		t.mu.Unlock()
		if future {
			return nil, errors.New("terminal_cursor_invalid")
		}
		return t.snapshot(cursor), nil
	case "workspace.terminal.input":
		data, ok := cmd["data"].(string)
		if !ok || len(data) == 0 || len(data) > 16384 || !utf8.ValidString(data) {
			return nil, errors.New("terminal_input_invalid")
		}
		seq, ok := terminalInteger(cmd["sequence"], 1, 1<<52)
		if !ok {
			return nil, errors.New("terminal_sequence_invalid")
		}
		if err = t.input(ctx, seq, []byte(data)); err != nil {
			return nil, err
		}
		return map[string]any{"terminalId": t.id, "sequence": seq}, nil
	case "workspace.terminal.resize":
		cols, ok := terminalInteger(cmd["cols"], 20, 400)
		if !ok {
			return nil, errors.New("terminal_size_invalid")
		}
		rows, ok := terminalInteger(cmd["rows"], 5, 200)
		if !ok {
			return nil, errors.New("terminal_size_invalid")
		}
		t.mu.Lock()
		t.lastTouch = time.Now()
		closed := t.closed || t.exited
		t.mu.Unlock()
		if closed {
			return nil, errors.New("terminal_exited")
		}
		if err = t.process.Resize(int(cols), int(rows)); err != nil {
			return nil, err
		}
		return map[string]any{"terminalId": t.id}, nil
	case "workspace.terminal.close":
		t.stop()
		delete(c.terminals, t.id)
		return map[string]any{"terminalId": t.id, "closed": true}, nil
	}
	return nil, errors.New("terminal_command_invalid")
}

func terminalInteger(value any, min, max int64) (int64, bool) {
	var n float64
	switch v := value.(type) {
	case float64:
		n = v
	case int:
		n = float64(v)
	case int64:
		n = float64(v)
	default:
		return 0, false
	}
	return int64(n), !math.IsNaN(n) && !math.IsInf(n, 0) && n == math.Trunc(n) && n >= float64(min) && n <= float64(max)
}
func terminalIdentifier(value string) bool {
	if len(value) < 12 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func (t *workspaceTerminal) collect() {
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 8192)
		for {
			n, err := t.process.Read(buf)
			if n > 0 {
				t.append(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	code, _ := t.process.Wait()
	// ClosePseudoConsole must run while its output is still being drained.
	t.process.Close()
	<-readDone
	t.mu.Lock()
	t.exited = true
	t.exitCode = code
	t.mu.Unlock()
}
func (t *workspaceTerminal) append(data []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buffer = append(t.buffer, data...)
	t.end += int64(len(data))
	if len(t.buffer) > terminalBufferLimit {
		t.buffer = append([]byte(nil), t.buffer[len(t.buffer)-terminalBufferLimit:]...)
	}
	t.start = t.end - int64(len(t.buffer))
}
func (t *workspaceTerminal) snapshot(cursor int64) terminalReply {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastTouch = time.Now()
	truncated := cursor < t.start
	if cursor < t.start {
		cursor = t.start
	}
	if cursor > t.end {
		cursor = t.end
	}
	end := cursor + terminalReadLimit
	if end > t.end {
		end = t.end
	}
	// Report exit only with the last buffered chunk so a fast command cannot
	// truncate its final output when the renderer sees process completion.
	return terminalReply{TerminalID: t.id, Cursor: end, StartCursor: cursor, Data: base64.StdEncoding.EncodeToString(t.buffer[cursor-t.start : end-t.start]), Truncated: truncated, Exited: t.exited && end == t.end, ExitCode: t.exitCode, NextSequence: t.nextSequence}
}
func (t *workspaceTerminal) input(ctx context.Context, sequence int64, data []byte) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	hash := sha256.Sum256(data)
	t.mu.Lock()
	if sequence == t.nextSequence-1 && hash == t.lastInput && t.lastInputAccepted {
		t.mu.Unlock()
		return nil
	}
	if t.closed || t.exited || sequence != t.nextSequence {
		t.mu.Unlock()
		return errors.New("terminal_input_sequence: 输入状态已变化，请重新连接终端")
	}
	t.nextSequence++
	t.lastInput = hash
	t.lastInputAccepted = false
	t.lastTouch = time.Now()
	t.mu.Unlock()
	// Consume the sequence before writing: an ambiguous network failure can
	// never replay a command. A partial OS write ends this terminal instead.
	result := make(chan error, 1)
	go func() {
		n, err := t.process.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
		result <- err
	}()
	var err error
	select {
	case err = <-result:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		t.stop()
		return errors.New("terminal_input_failed: 终端输入中断，未自动重试")
	}
	t.mu.Lock()
	t.lastInputAccepted = true
	t.mu.Unlock()
	return nil
}
func (t *workspaceTerminal) stop() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	close(t.done)
	t.mu.Unlock()
	t.process.Close()
}

func (c *Client) watchTerminal(t *workspaceTerminal) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-t.done:
			return
		case <-ticker.C:
			cfg := c.o.Store.Get()
			t.mu.Lock()
			idle := time.Since(t.lastTouch) > 90*time.Second
			t.mu.Unlock()
			root, err := workspaceConfiguredRoot(cfg, t.scope.root)
			info, statErr := os.Lstat(root)
			invalid := idle || err != nil || statErr != nil || !os.SameFile(info, t.rootInfo) || config.RemoteIdentity(cfg) != t.scope.identity || cfg.DeviceID != t.scope.device || cfg.PhoneBindingID != t.scope.binding || cfg.PhoneHash != t.scope.phone || t.scope.phone != "" && !c.canUpload(cfg)
			if !invalid {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				cwd, readErr := c.workspaceSessionCwd(ctx, t.scope.key, t.scope.surface)
				cancel()
				invalid = readErr != nil || !workspaceSamePath(cwd, t.scope.root)
			}
			if invalid {
				c.terminalMu.Lock()
				if c.terminals[t.id] == t {
					delete(c.terminals, t.id)
				}
				c.terminalMu.Unlock()
				t.stop()
				return
			}
		}
	}
}

// Called when the Bridge stops. Per-terminal idle/identity watchers also
// release abandoned terminals after loss of the phone transport.
func (c *Client) closeWorkspaceTerminals() {
	c.terminalMu.Lock()
	defer c.terminalMu.Unlock()
	for id, t := range c.terminals {
		t.stop()
		delete(c.terminals, id)
	}
}

func terminalEnvironment() []string {
	allowed := map[string]bool{"PATH": true, "PATHEXT": true, "SYSTEMROOT": true, "WINDIR": true, "COMSPEC": true, "TEMP": true, "TMP": true, "HOME": true, "USERPROFILE": true, "HOMEDRIVE": true, "HOMEPATH": true, "APPDATA": true, "LOCALAPPDATA": true, "PROGRAMFILES": true, "PROGRAMFILES(X86)": true, "PROGRAMDATA": true, "USERNAME": true, "USER": true, "LANG": true, "LC_ALL": true, "SHELL": true}
	env := make([]string, 0, 32)
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok && allowed[strings.ToUpper(key)] {
			env = append(env, entry)
		}
	}
	return append(env, "TERM=xterm-256color", "COLORTERM=truecolor")
}
