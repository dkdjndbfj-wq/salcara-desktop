package agents

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

func buildDeepSeekStartupFixture(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "dsh-fixture")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", exe, "./testdata/deepseek_sdk_helper")
	prepareCmd(build)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatal("fixture build", err, string(output))
	}
	return exe
}

// Scale the caller's deadline down while preserving the production startup,
// shutdown/tree-stop, persistence and retry paths. The real official SDK tests
// separately cover protocol compatibility; this fixture deterministically holds
// initialize beyond the caller deadline without sending any provider request.
func TestDeepSeekStartupDeadlineStopsBeforePromptAndAllowsExplicitRetry(t *testing.T) {
	exe := buildDeepSeekStartupFixture(t)
	t.Setenv("SALCARA_DSH_FIXTURE_INITIALIZE_DELAY", "15s")
	state := t.TempDir()
	settings := Settings{DeepSeekPath: exe, Approval: "ask", Extensions: map[string]ExtensionSettings{deepSeekID: {Root: "http://127.0.0.1:1", Key: "synthetic-fixture-key", Model: "fixture-model"}}}
	a := newDeepSeekAgent(func() Settings { return settings }, state)
	defer a.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	returnedID, err := a.Start(ctx, t.TempDir(), "must never be delivered", "", "")
	if !errors.Is(err, context.DeadlineExceeded) || returnedID != "" {
		t.Fatalf("startup deadline response: id=%q error=%v", returnedID, err)
	}
	a.mu.Lock()
	var id string
	var session *deepSeekSession
	for key, s := range a.sessions {
		id, session = key, s
	}
	count := len(a.sessions)
	a.mu.Unlock()
	if count != 1 || session == nil {
		t.Fatalf("failed startup record not preserved: %d", count)
	}
	native := filepath.Join(state, deepSeekID, id, "native")
	if _, err := os.Stat(filepath.Join(native, "initialize.started")); err != nil {
		t.Fatal("deadline did not exercise an initializing child", err)
	}
	if session.Started || session.busy || session.RuntimePending || session.Unconfirmed || session.Info.Status != "failed" || !session.Info.Controllable || session.conn == nil || !session.conn.rootProcessExitConfirmed() || a.ActiveRemoteTurns() {
		t.Fatalf("confirmed startup stop left incorrect state: %+v", session.Info)
	}
	if _, err := os.Stat(filepath.Join(native, id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled initialize reached session/prompt", err)
	}
	for _, event := range session.Events {
		if event.Role == "user" {
			t.Fatal("cancelled startup projected a user message")
		}
	}
	// Verify the next Bridge reconstructs an ordinary failed attempt rather
	// than an unknown worker; loading does not itself retry anything.
	restored := newDeepSeekAgent(func() Settings { return settings }, state)
	if restored.ActiveRemoteTurns() || restored.sessions[id].Started || restored.sessions[id].Unconfirmed || restored.sessions[id].RuntimePending {
		t.Fatal("confirmed cancellation was not durably recorded")
	}
	restored.Close()
	t.Setenv("SALCARA_DSH_FIXTURE_INITIALIZE_DELAY", "")
	retryCtx, retryCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer retryCancel()
	if err := a.Send(retryCtx, id, "explicit retry only"); err != nil {
		t.Fatal("explicit retry failed", err)
	}
	for retryCtx.Err() == nil {
		info, events, err := a.Open(retryCtx, id)
		if err != nil {
			t.Fatal(err)
		}
		if info.Status == "idle" {
			users := 0
			for _, event := range events {
				if event.Role == "user" {
					users++
					if event.Text != "explicit retry only" {
						t.Fatal("startup cancellation replayed the old prompt")
					}
				}
			}
			if users != 1 {
				t.Fatalf("explicit retry did not produce exactly one user message: %d", users)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(retryCtx.Err())
}

func TestDeepSeekMissingPromptReceiptPreservesUncertainIdentity(t *testing.T) {
	exe := buildDeepSeekStartupFixture(t)
	state, logPath := t.TempDir(), filepath.Join(t.TempDir(), "prompts.jsonl")
	t.Setenv("SALCARA_DSH_FIXTURE_EMPTY_RECEIPT", "1")
	t.Setenv("SALCARA_DSH_FIXTURE_PROMPT_LOG", logPath)
	settings := Settings{DeepSeekPath: exe, Approval: "ask", Extensions: map[string]ExtensionSettings{deepSeekID: {Root: "http://127.0.0.1:1", Key: "synthetic-fixture-key", Model: "fixture-model"}}}
	a := newDeepSeekAgent(func() Settings { return settings }, state)
	closeFirst := sync.OnceFunc(a.Close)
	defer closeFirst()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id, err := a.Start(ctx, t.TempDir(), "first attempt", "", "")
	if id == "" || protocol.ExecutionErrorCode(err) != "command_delivery_uncertain" {
		t.Fatalf("missing receipt was treated as safely unsent: id=%q error=%v", id, err)
	}
	if len(a.sessions) != 1 || !a.sessions[id].Started {
		t.Fatal("native session identity was discarded")
	}
	data, err := os.ReadFile(logPath)
	if err != nil || strings.Count(string(data), "\n") != 1 {
		t.Fatal("uncertain start replayed a prompt", err, string(data))
	}
	closeFirst()
	// After checking the uncertain result, an explicit continuation must use
	// the same persisted identity and native resume, not create a second task.
	t.Setenv("SALCARA_DSH_FIXTURE_EMPTY_RECEIPT", "")
	b := newDeepSeekAgent(func() Settings { return settings }, state)
	defer b.Close()
	if !b.sessions[id].Started {
		t.Fatal("uncertain identity was not durable")
	}
	if err := b.Send(ctx, id, "explicit continuation"); err != nil {
		t.Fatal("same-identity continuation failed", err)
	}
	resumed, err := os.ReadFile(filepath.Join(state, deepSeekID, id, "native", "resume.called"))
	if err != nil || string(resumed) != id {
		t.Fatal("uncertain native session was recreated instead of resumed", err)
	}
	data, err = os.ReadFile(logPath)
	if err != nil || strings.Count(string(data), "\n") != 2 || strings.Count(string(data), "first attempt") != 1 || strings.Count(string(data), "explicit continuation") != 1 {
		t.Fatal("continuation duplicated the earlier prompt", err, string(data))
	}
}
