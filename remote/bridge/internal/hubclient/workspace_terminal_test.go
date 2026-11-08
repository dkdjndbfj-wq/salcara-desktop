package hubclient

import (
	"context"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
)

func terminalCommand(root, typ string) map[string]any {
	return map[string]any{"type": "workspace.terminal." + typ, "sessionKey": workspaceTestKey, "controlSurface": "cli", "expectedCwd": root}
}

type testTerminal struct {
	terminalReply
	client          *Client
	root            string
	cursor          int64
	output          strings.Builder
	matched         int
	answeredQueries int
}

func openTestTerminal(t *testing.T, c *Client, root string) *testTerminal {
	t.Helper()
	cmd := terminalCommand(root, "open")
	cmd["operationId"] = "test-terminal-open-0001"
	cmd["cols"], cmd["rows"] = 80, 24
	r, err := c.dispatchWorkspaceTerminal(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.closeWorkspaceTerminals)
	return &testTerminal{terminalReply: r.(terminalReply), client: c, root: root}
}

func (terminal *testTerminal) waitReady(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		// Wait for the interactive host before sending typeahead. In particular,
		// PSReadLine startup on CI must finish before testing Enter or Ctrl+C.
		// ConPTY may encode the trailing prompt space as cursor movement.
		terminal.waitText(t, "PS "+filepath.Clean(terminal.root)+">")
	}
}
func readTestTerminal(t *testing.T, c *Client, root, id string, cursor int64) terminalReply {
	t.Helper()
	cmd := terminalCommand(root, "read")
	cmd["terminalId"], cmd["cursor"] = id, cursor
	r, err := c.dispatchWorkspaceTerminal(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	return r.(terminalReply)
}
func sendTestTerminal(t *testing.T, c *Client, root, id string, sequence int64, data string) {
	t.Helper()
	cmd := terminalCommand(root, "input")
	cmd["terminalId"], cmd["sequence"], cmd["data"] = id, sequence, data
	if _, err := c.dispatchWorkspaceTerminal(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceTerminalPersistentPTYAndInputDeduplication(t *testing.T) {
	c, _, root := workspaceFixture(t)
	first := openTestTerminal(t, c, root)
	again := openTestTerminal(t, c, root)
	if again.TerminalID != first.TerminalID {
		t.Fatal("open retry created another shell")
	}
	first.waitReady(t)
	assignment := "SALCARA_PTY_COUNT=$((${SALCARA_PTY_COUNT:-0}+1)); export SALCARA_PTY_TEST='persisted'; printf '\\n%s_%s\\n' 'READY' 'MARKER'\r"
	query := "printf '\\nVALUE_%s_END_%s\\n' \"$SALCARA_PTY_TEST\" \"$SALCARA_PTY_COUNT\"\r"
	if runtime.GOOS == "windows" {
		assignment = "$env:SALCARA_PTY_COUNT=[string]([int]$env:SALCARA_PTY_COUNT+1); $env:SALCARA_PTY_TEST='persisted'; Write-Output ('READY_' + 'MARKER')\r"
		query = "Write-Output ('VALUE_' + $env:SALCARA_PTY_TEST + '_END_' + $env:SALCARA_PTY_COUNT)\r"
	}
	sendTestTerminal(t, c, root, first.TerminalID, 1, assignment)
	sendTestTerminal(t, c, root, first.TerminalID, 1, assignment)
	// The marker is assembled by the shell, so input echo cannot satisfy it.
	first.waitText(t, "READY_MARKER")
	first.waitReady(t)
	sendTestTerminal(t, c, root, first.TerminalID, 2, query)
	first.waitText(t, "VALUE_persisted_END_1")
	resize := terminalCommand(root, "resize")
	resize["terminalId"], resize["cols"], resize["rows"] = first.TerminalID, 100, 30
	if _, err := c.dispatchWorkspaceTerminal(context.Background(), resize); err != nil {
		t.Fatal(err)
	}
	bad := terminalCommand(root, "input")
	bad["terminalId"], bad["sequence"], bad["data"] = first.TerminalID, 2, "echo must-not-execute\r"
	if _, err := c.dispatchWorkspaceTerminal(context.Background(), bad); err == nil {
		t.Fatal("reused sequence changed command")
	}
}

func (terminal *testTerminal) waitText(t *testing.T, want string) {
	t.Helper()
	terminal.waitTextWithin(t, want, 15*time.Second)
}

func (terminal *testTerminal) waitTextWithin(t *testing.T, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r := readTestTerminal(t, terminal.client, terminal.root, terminal.TerminalID, terminal.cursor)
		terminal.cursor = r.Cursor
		b, err := base64.StdEncoding.DecodeString(r.Data)
		if err != nil {
			t.Fatal(err)
		}
		terminal.output.Write(b)
		output := terminal.output.String()
		// Act as the renderer for cursor queries, even when a VT sequence spans
		// read chunks. Never replay an already answered query on the next wait.
		queries := strings.Count(output, "\x1b[6n")
		for terminal.answeredQueries < queries {
			terminal.client.terminalMu.Lock()
			process := terminal.client.terminals[terminal.TerminalID]
			terminal.client.terminalMu.Unlock()
			if process == nil {
				t.Fatal("terminal disappeared while answering cursor query")
			}
			process.writeMu.Lock()
			n, err := process.process.Write([]byte("\x1b[1;1R"))
			process.writeMu.Unlock()
			if err != nil || n != len("\x1b[1;1R") {
				t.Fatalf("cursor query response failed: n=%d err=%v", n, err)
			}
			terminal.answeredQueries++
		}
		if index := strings.Index(output[terminal.matched:], want); index >= 0 {
			terminal.matched += index + len(want)
			return
		}
		if r.Exited {
			t.Fatalf("PTY exited with code %d before %q; output %q", r.ExitCode, want, output)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("PTY did not emit %q; output %q", want, terminal.output.String())
}

func TestWorkspaceTerminalScopeAndRevocation(t *testing.T) {
	c, _, root := workspaceFixture(t)
	first := openTestTerminal(t, c, root)
	cmd := terminalCommand(root, "read")
	cmd["terminalId"], cmd["cursor"] = first.TerminalID, 0
	cmd["sessionKey"] = "codex:other"
	if _, err := c.dispatchWorkspaceTerminal(context.Background(), cmd); err == nil {
		t.Fatal("cross session read admitted")
	}
	if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.PhoneBindingID = "another-generation"; return nil }); err != nil {
		t.Fatal(err)
	}
	cmd["sessionKey"] = workspaceTestKey
	if _, err := c.dispatchWorkspaceTerminal(context.Background(), cmd); err == nil {
		t.Fatal("old binding terminal admitted")
	}
	if len(c.terminals) != 0 {
		t.Fatal("revoked process retained")
	}
}

func TestWorkspaceTerminalBoundedReplay(t *testing.T) {
	tm := &workspaceTerminal{nextSequence: 1}
	tm.append([]byte(strings.Repeat("a", terminalBufferLimit+200)))
	r := tm.snapshot(0)
	if !r.Truncated || r.StartCursor != 200 || r.Cursor != 200+terminalReadLimit {
		t.Fatalf("bad cursor: %+v", r)
	}
	r2 := tm.snapshot(r.StartCursor)
	if r2.Data != r.Data || r2.Cursor != r.Cursor {
		t.Fatal("output retry not replayable")
	}
	tm.exited = true
	if tm.snapshot(200).Exited || !tm.snapshot(tm.end).Exited {
		t.Fatal("exit reported before remaining buffered output")
	}
}

func TestWorkspaceTerminalInterruptForegroundProcess(t *testing.T) {
	c, _, root := workspaceFixture(t)
	first := openTestTerminal(t, c, root)
	first.waitReady(t)
	command := "PS1='SALCARA_TERMINAL_READY> '; printf '\\n%s_%s\\n' 'FOREGROUND' 'STARTED'; sleep 30\r"
	after := "printf '\\n%s_%s\\n' 'INTERRUPT' 'RECOVERED'\r"
	if runtime.GOOS == "windows" {
		command = "function prompt { 'SALCARA_TERMINAL_READY> ' }; Write-Output ('FOREGROUND_' + 'STARTED'); Start-Sleep -Seconds 30\r"
		after = "Write-Output ('INTERRUPT' + '_RECOVERED')\r"
	}
	sendTestTerminal(t, c, root, first.TerminalID, 1, command)
	first.waitText(t, "FOREGROUND_STARTED")
	sendTestTerminal(t, c, root, first.TerminalID, 2, "\x03")
	// Wait for the interrupted pipeline to release the shell before typing the
	// next command; a fixed delay can interrupt PSReadLine startup instead.
	first.waitText(t, "SALCARA_TERMINAL_READY>")
	sendTestTerminal(t, c, root, first.TerminalID, 3, after)
	first.waitText(t, "INTERRUPT_RECOVERED")
}

func TestWorkspaceTerminalEnvironmentDoesNotInheritCredentials(t *testing.T) {
	t.Setenv("SALCARA_PAIR_TOKEN", "sentinel")
	t.Setenv("OPENAI_API_KEY", "sentinel")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sentinel")
	for _, entry := range terminalEnvironment() {
		if strings.Contains(entry, "sentinel") {
			t.Fatalf("inherited credential %s", strings.SplitN(entry, "=", 2)[0])
		}
	}
	if os.Getenv("PATH") != "" {
		found := false
		for _, entry := range terminalEnvironment() {
			found = found || strings.HasPrefix(strings.ToUpper(entry), "PATH=")
		}
		if !found {
			t.Fatal("PATH missing")
		}
	}
}

type partialTerminalProcess struct{ writes int }

func (p *partialTerminalProcess) Read([]byte) (int, error)  { return 0, io.EOF }
func (p *partialTerminalProcess) Write([]byte) (int, error) { p.writes++; return 1, io.ErrShortWrite }
func (p *partialTerminalProcess) Close() error              { return nil }
func (p *partialTerminalProcess) Resize(int, int) error     { return nil }
func (p *partialTerminalProcess) Wait() (int, error)        { return 0, nil }
func TestWorkspaceTerminalPartialInputIsNotAcknowledgedOnRetry(t *testing.T) {
	process := &partialTerminalProcess{}
	tm := &workspaceTerminal{process: process, nextSequence: 1, done: make(chan struct{})}
	if err := tm.input(context.Background(), 1, []byte("echo example\r")); err == nil {
		t.Fatal("partial write accepted")
	}
	if err := tm.input(context.Background(), 1, []byte("echo example\r")); err == nil {
		t.Fatal("partial write became a successful retry")
	}
	if process.writes != 1 {
		t.Fatal("partial command replayed")
	}
}
