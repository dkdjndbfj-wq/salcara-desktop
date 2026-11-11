package agents

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

type deepSeekRuntimeFixtureConfig struct {
	Mode         string `json:"mode"`
	Log          string `json:"log"`
	NodeOutput   string `json:"nodeOutput"`
	NodeStderr   string `json:"nodeStderr"`
	NodeExit     int    `json:"nodeExit"`
	NodeSleepMS  int    `json:"nodeSleepMS"`
	EntryOutput  string `json:"entryOutput"`
	EntryStderr  string `json:"entryStderr"`
	EntryExit    int    `json:"entryExit"`
	EntrySleepMS int    `json:"entrySleepMS"`
}

type deepSeekRuntimeFixtureCall struct {
	Kind string   `json:"kind"`
	Args []string `json:"args"`
}

func buildDeepSeekRuntimeFixture(t *testing.T) []byte {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "runtime-helper")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", exe, "./testdata/deepseek_runtime_helper")
	prepareCmd(cmd)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build synthetic runtime helper: %v\n%s", err, output)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeDeepSeekRuntimeFixtureConfig(t *testing.T, exe string, cfg deepSeekRuntimeFixtureConfig) {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe+".json", data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func installDeepSeekRuntimeFixture(t *testing.T, binary []byte, name string, cfg deepSeekRuntimeFixtureConfig) (string, deepSeekRuntimeFixtureConfig) {
	t.Helper()
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	exe := filepath.Join(dir, name)
	if err := os.WriteFile(exe, binary, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.Log = filepath.Join(dir, "calls.jsonl")
	writeDeepSeekRuntimeFixtureConfig(t, exe, cfg)
	// Separate setup of a fresh helper image from the runtime probe under test.
	// This does not populate production caches, print a version or touch the
	// scenario/call log. Real cancellation and the product's 3s limit remain
	// unchanged; failure to prepare the fixture is an explicit test failure.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ready := exec.CommandContext(ctx, exe, "--salcara-fixture-ready")
	prepareCmd(ready)
	ready.WaitDelay = 250 * time.Millisecond
	if output, err := ready.CombinedOutput(); err != nil || len(output) != 0 {
		t.Fatalf("synthetic runtime fixture did not become ready: %v (output bytes %d)", err, len(output))
	}
	if _, err := os.Stat(cfg.Log); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture readiness unexpectedly recorded a runtime call: %v", err)
	}
	return exe, cfg
}

func readDeepSeekRuntimeFixtureCalls(t *testing.T, log string) []deepSeekRuntimeFixtureCall {
	t.Helper()
	data, err := os.ReadFile(log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []deepSeekRuntimeFixtureCall
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var call deepSeekRuntimeFixtureCall
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, call)
	}
	return calls
}

func deepSeekRuntimeEntry(t *testing.T, extension string) string {
	t.Helper()
	entry := filepath.Join(t.TempDir(), "entry with spaces"+extension)
	if err := os.WriteFile(entry, []byte("#!/usr/bin/env node\n// synthetic entry; must never execute\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return entry
}

func useDeepSeekRuntimeFixtureNode(t *testing.T, node string) {
	t.Helper()
	// On Unix the login-shell PATH precedes the process PATH. Keep host Node
	// installations (including an already-cached shell PATH) out of this fixture.
	t.Setenv(ShellPathEnv, "1")
	t.Setenv("PATH", filepath.Dir(node)+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func assertDeepSeekNoEntryExecuted(t *testing.T, cfg deepSeekRuntimeFixtureConfig) {
	t.Helper()
	for _, call := range readDeepSeekRuntimeFixtureCalls(t, cfg.Log) {
		if call.Kind != "node-version" {
			t.Fatalf("prerequisite check executed npm entry: %+v", call)
		}
	}
}

func TestDeepSeekRuntimePrerequisites(t *testing.T) {
	binary := buildDeepSeekRuntimeFixture(t)
	const secret = "synthetic-private-runtime-diagnostic"
	t.Run("node-version-boundary-and-safe-errors", func(t *testing.T) {
		for _, tc := range []struct {
			name, output, stderr string
			exit                 int
			wantOK               bool
		}{
			{name: "older-major", output: "v22.18.0\n"},
			{name: "older-minor", output: "v24.8.99\n"},
			{name: "minimum", output: "v24.9.0\n", wantOK: true},
			{name: "newer-minor", output: "v24.16.0\r\n", wantOK: true},
			{name: "newer-major", output: "v25.0.0\n", wantOK: true},
			{name: "nonzero-exit", output: "v25.0.0\n", stderr: secret, exit: 7},
			{name: "stderr-only", stderr: "v25.0.0 " + secret},
			{name: "empty"},
			{name: "malformed", output: secret + " v25.0.0\n"},
			{name: "multiple-lines", output: "v25.0.0\n" + secret},
			{name: "oversized", output: "v25.0.0\n" + strings.Repeat("x", 128<<10)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				node, cfg := installDeepSeekRuntimeFixture(t, binary, "node", deepSeekRuntimeFixtureConfig{Mode: "node", NodeOutput: tc.output, NodeStderr: tc.stderr, NodeExit: tc.exit})
				useDeepSeekRuntimeFixtureNode(t, node)
				entry := deepSeekRuntimeEntry(t, ".js")
				args := []string{"--profile", "sdk", "--patch", "literal & patch.yml"}
				cmd, err := DeepSeekCommand(context.Background(), entry, args...)
				if tc.wantOK {
					if err != nil || cmd == nil {
						t.Fatalf("supported runtime rejected: %v", err)
					}
					if !strings.EqualFold(filepath.Clean(cmd.Path), filepath.Clean(node)) || !reflect.DeepEqual(cmd.Args[1:], append([]string{entry}, args...)) {
						t.Fatalf("selected runtime or literal arguments changed: %+v", cmd.Args)
					}
				} else {
					if err == nil || cmd != nil {
						t.Fatal("invalid runtime returned a launchable command")
					}
					if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("version case did not reach its intended decision: %v", err)
					}
					if strings.Contains(err.Error(), secret) || len(err.Error()) > 1000 {
						t.Fatalf("unsafe diagnostic exposed: %q", err.Error())
					}
				}
				assertDeepSeekNoEntryExecuted(t, cfg)
				if calls := readDeepSeekRuntimeFixtureCalls(t, cfg.Log); len(calls) != 1 {
					t.Fatalf("version case must execute exactly one synthetic Node probe: %+v", calls)
				}
			})
		}
	})

	t.Run("node-probe-failure-can-recover", func(t *testing.T) {
		node, cfg := installDeepSeekRuntimeFixture(t, binary, "node", deepSeekRuntimeFixtureConfig{Mode: "node", NodeOutput: "v25.0.0\n", NodeExit: 7, EntryOutput: "dsh 0.2.2-rc.1\n"})
		useDeepSeekRuntimeFixtureNode(t, node)
		entry := deepSeekRuntimeEntry(t, ".js")
		if err := CheckDeepSeekRuntime(context.Background(), entry); err == nil {
			t.Fatal("failed node probe accepted")
		}
		assertDeepSeekNoEntryExecuted(t, cfg)
		cfg.NodeExit = 0
		writeDeepSeekRuntimeFixtureConfig(t, node, cfg)
		if err := CheckDeepSeekRuntime(context.Background(), entry); err != nil {
			t.Fatalf("failed node probe was cached permanently: %v", err)
		}
		entryProbes := 0
		for _, call := range readDeepSeekRuntimeFixtureCalls(t, cfg.Log) {
			if call.Kind == "entry-version" {
				entryProbes++
			} else if call.Kind != "node-version" {
				t.Fatalf("availability check started SDK execution: %+v", call)
			}
		}
		if entryProbes != 1 {
			t.Fatalf("recovered runtime should check DSH exactly once: %d", entryProbes)
		}
	})

	t.Run("start-rejects-old-node-before-sdk-or-prompt", func(t *testing.T) {
		node, cfg := installDeepSeekRuntimeFixture(t, binary, "node", deepSeekRuntimeFixtureConfig{Mode: "node", NodeOutput: "v24.8.0\n", EntryOutput: "dsh 0.2.2-rc.1\n"})
		useDeepSeekRuntimeFixtureNode(t, node)
		entry := deepSeekRuntimeEntry(t, ".js")
		state := t.TempDir()
		settings := Settings{DeepSeekPath: entry, Approval: "ask", Extensions: map[string]ExtensionSettings{deepSeekID: {Root: "http://127.0.0.1:1", Key: secret, Model: "synthetic-model"}}}
		a := newDeepSeekAgent(func() Settings { return settings }, state)
		defer a.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		id, err := a.Start(ctx, t.TempDir(), "must not reach SDK or provider", "", "")
		if id != "" || err == nil || !strings.Contains(err.Error(), "24.9.0") || strings.Contains(err.Error(), secret) {
			t.Fatalf("old-Node startup response: id=%q error=%v", id, err)
		}
		assertDeepSeekNoEntryExecuted(t, cfg)
		if calls := readDeepSeekRuntimeFixtureCalls(t, cfg.Log); len(calls) != 1 {
			t.Fatalf("startup must stop after one Node version check: %+v", calls)
		}
		a.mu.Lock()
		var savedID string
		var session *deepSeekSession
		for k, s := range a.sessions {
			savedID, session = k, s
		}
		count := len(a.sessions)
		a.mu.Unlock()
		if count != 1 || session == nil {
			t.Fatalf("failed preflight did not preserve one controllable attempt: %d", count)
		}
		if session.Started || session.RuntimePending || session.Unconfirmed || session.busy || session.conn != nil || session.Info.Status != "failed" || !session.Info.Controllable || a.ActiveRemoteTurns() {
			t.Fatalf("runtime preflight failure left execution markers: started=%v pending=%v unconfirmed=%v busy=%v info=%+v", session.Started, session.RuntimePending, session.Unconfirmed, session.busy, session.Info)
		}
		for _, event := range session.Events {
			if event.Role == "user" {
				t.Fatal("rejected startup projected a delivered user message")
			}
		}
		data, err := os.ReadFile(filepath.Join(a.stateDir, savedID, "session.json"))
		var persisted deepSeekSession
		if err != nil || json.Unmarshal(data, &persisted) != nil {
			t.Fatalf("failed attempt checkpoint unreadable: %v", err)
		}
		if persisted.Started || persisted.RuntimePending || persisted.Unconfirmed || persisted.Info.Status != "failed" || !persisted.Info.Controllable {
			t.Fatal("preflight rejection persisted an uncertain or started runtime")
		}
		restored := newDeepSeekAgentWithBoot(func() Settings { return settings }, state, a.boot)
		defer restored.Close()
		loaded := restored.sessions[savedID]
		if loaded == nil || loaded.Started || loaded.RuntimePending || loaded.Unconfirmed || !loaded.Info.Controllable || restored.ActiveRemoteTurns() {
			t.Fatal("preflight rejection became an unknown worker after reconstruction")
		}
		assertDeepSeekNoEntryExecuted(t, cfg)
	})

	t.Run("dsh-failure-is-unavailable-and-not-cached", func(t *testing.T) {
		node, cfg := installDeepSeekRuntimeFixture(t, binary, "node", deepSeekRuntimeFixtureConfig{Mode: "node", NodeOutput: "v24.9.0\n", EntryOutput: "dsh 0.2.2-rc.1\n", EntryStderr: secret, EntryExit: 9})
		useDeepSeekRuntimeFixtureNode(t, node)
		entry := deepSeekRuntimeEntry(t, ".js")
		a := newDeepSeekAgent(func() Settings { return Settings{DeepSeekPath: entry} }, t.TempDir())
		defer a.Close()
		if tool := a.Detect(context.Background()); tool.Available {
			t.Fatalf("failed dsh version probe advertised available: %+v", tool)
		}
		cfg.EntryExit, cfg.EntryStderr = 0, ""
		writeDeepSeekRuntimeFixtureConfig(t, node, cfg)
		if tool := a.Detect(context.Background()); !tool.Available || tool.Version != "0.2.2-rc.1" {
			t.Fatalf("repaired dsh remained unavailable: %+v", tool)
		}
		for _, call := range readDeepSeekRuntimeFixtureCalls(t, cfg.Log) {
			if call.Kind == "entry-start" {
				t.Fatal("detection started an SDK session")
			}
		}
	})

	t.Run("cached-dsh-still-rechecks-selected-node", func(t *testing.T) {
		node, cfg := installDeepSeekRuntimeFixture(t, binary, "node", deepSeekRuntimeFixtureConfig{Mode: "node", NodeOutput: "v24.9.0\n", EntryOutput: "dsh 0.2.2-rc.1\n"})
		useDeepSeekRuntimeFixtureNode(t, node)
		entry := deepSeekRuntimeEntry(t, ".js")
		if v, ok := deepSeekVersion(context.Background(), entry); !ok || v != "0.2.2-rc.1" {
			t.Fatalf("initial probe: %q, %v", v, ok)
		}
		cfg.NodeOutput = "v24.8.0\n"
		writeDeepSeekRuntimeFixtureConfig(t, node, cfg)
		if _, ok := deepSeekVersion(context.Background(), entry); ok {
			t.Fatal("cached DSH version bypassed changed Node prerequisite")
		}
		entryCalls := 0
		for _, call := range readDeepSeekRuntimeFixtureCalls(t, cfg.Log) {
			if call.Kind == "entry-version" {
				entryCalls++
			}
		}
		if entryCalls != 1 {
			t.Fatalf("rejected runtime executed npm entry: %d probes", entryCalls)
		}
	})

	t.Run("cached-dsh-is-scoped-to-node-path", func(t *testing.T) {
		first, _ := installDeepSeekRuntimeFixture(t, binary, "node", deepSeekRuntimeFixtureConfig{Mode: "node", NodeOutput: "v24.9.0\n", EntryOutput: "dsh 0.2.2-rc.1\n"})
		useDeepSeekRuntimeFixtureNode(t, first)
		entry := deepSeekRuntimeEntry(t, ".mjs")
		if _, ok := deepSeekVersion(context.Background(), entry); !ok {
			t.Fatal("first runtime unavailable")
		}
		second, cfg := installDeepSeekRuntimeFixture(t, binary, "node", deepSeekRuntimeFixtureConfig{Mode: "node", NodeOutput: "v25.0.0\n", EntryOutput: "dsh 0.2.3\n"})
		useDeepSeekRuntimeFixtureNode(t, second)
		if v, ok := deepSeekVersion(context.Background(), entry); !ok || v != "0.2.3" {
			t.Fatalf("new Node reused unrelated DSH probe: %q, %v", v, ok)
		}
		found := false
		for _, call := range readDeepSeekRuntimeFixtureCalls(t, cfg.Log) {
			found = found || call.Kind == "entry-version"
		}
		if !found {
			t.Fatal("new Node's entry was never verified")
		}
	})

	t.Run("cancelled-and-deadline-probes", func(t *testing.T) {
		node, cfg := installDeepSeekRuntimeFixture(t, binary, "node", deepSeekRuntimeFixtureConfig{Mode: "node", NodeOutput: "v25.0.0\n", NodeSleepMS: 5000})
		useDeepSeekRuntimeFixtureNode(t, node)
		entry := deepSeekRuntimeEntry(t, ".js")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if cmd, err := DeepSeekCommand(ctx, entry); cmd != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled request ignored: %v", err)
		}
		ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		started := time.Now()
		if err := CheckDeepSeekRuntime(ctx, entry); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("parent deadline not preserved: %v", err)
		}
		if elapsed := time.Since(started); elapsed >= 3*time.Second {
			t.Fatalf("node probe ignored shorter caller budget: %v", elapsed)
		}
		assertDeepSeekNoEntryExecuted(t, cfg)
	})

	t.Run("prepared-command-outlives-preflight-context", func(t *testing.T) {
		node, cfg := installDeepSeekRuntimeFixture(t, binary, "node", deepSeekRuntimeFixtureConfig{Mode: "node", NodeOutput: "v24.9.0\n"})
		useDeepSeekRuntimeFixtureNode(t, node)
		entry := deepSeekRuntimeEntry(t, ".js")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cmd, err := DeepSeekCommand(ctx, entry, "--profile", "sdk")
		if err != nil || cmd == nil {
			t.Fatalf("synthetic command preparation failed: %v", err)
		}
		assertDeepSeekNoEntryExecuted(t, cfg)
		cancel()
		// Only the synthetic helper runs here: it records argv and exits without
		// interpreting the entry or starting an SDK/provider. A request context
		// must not become the lifetime owner of an already prepared SDK command.
		prepareCmd(cmd)
		if output, err := cmd.CombinedOutput(); err != nil || len(output) != 0 {
			t.Fatalf("preflight cancellation affected prepared command: %v (output bytes %d)", err, len(output))
		}
		calls := readDeepSeekRuntimeFixtureCalls(t, cfg.Log)
		if len(calls) != 2 || calls[0].Kind != "node-version" || calls[1].Kind != "entry-start" || !reflect.DeepEqual(calls[1].Args, []string{entry, "--profile", "sdk"}) {
			t.Fatalf("prepared command changed execution or replayed work: %+v", calls)
		}
	})

	t.Run("native-needs-no-node", func(t *testing.T) {
		node, cfg := installDeepSeekRuntimeFixture(t, binary, "node", deepSeekRuntimeFixtureConfig{Mode: "node", NodeOutput: "v1.0.0\n", NodeExit: 8})
		useDeepSeekRuntimeFixtureNode(t, node)
		native, nativeCfg := installDeepSeekRuntimeFixture(t, binary, "dsh-native", deepSeekRuntimeFixtureConfig{Mode: "native", EntryOutput: "dsh 0.2.2-rc.1\n"})
		if err := CheckDeepSeekRuntime(context.Background(), native); err != nil {
			t.Fatalf("native required Node: %v", err)
		}
		cmd, err := DeepSeekCommand(context.Background(), native, "--profile", "sdk")
		if err != nil || cmd == nil || cmd.Path != native || !reflect.DeepEqual(cmd.Args[1:], []string{"--profile", "sdk"}) {
			t.Fatalf("native command changed: %v", err)
		}
		if v, ok := deepSeekVersion(context.Background(), native); !ok || v != "0.2.2-rc.1" {
			t.Fatalf("native version probe: %q, %v", v, ok)
		}
		if calls := readDeepSeekRuntimeFixtureCalls(t, cfg.Log); len(calls) != 0 {
			t.Fatalf("native unexpectedly probed Node: %+v", calls)
		}
		if calls := readDeepSeekRuntimeFixtureCalls(t, nativeCfg.Log); len(calls) != 1 || calls[0].Kind != "native-version" {
			t.Fatalf("native preflight executed runtime: %+v", calls)
		}
	})

	t.Run("npm-entry-forms", func(t *testing.T) {
		for _, form := range []string{"js", "mjs", "cmd", "bat", "symlink", "shebang"} {
			t.Run(form, func(t *testing.T) {
				node, cfg := installDeepSeekRuntimeFixture(t, binary, "node", deepSeekRuntimeFixtureConfig{Mode: "node", NodeOutput: "v24.9.0\n"})
				useDeepSeekRuntimeFixtureNode(t, node)
				entry := deepSeekRuntimeEntry(t, ".js")
				exe := entry
				switch form {
				case "mjs":
					entry = deepSeekRuntimeEntry(t, ".mjs")
					exe = entry
				case "cmd", "bat":
					dir := t.TempDir()
					exe = filepath.Join(dir, "dsh."+form)
					entry = filepath.Join(dir, "node_modules", "@deepseek-ai", "dsh", "lib", "bin.js")
					if err := os.MkdirAll(filepath.Dir(entry), 0o700); err != nil {
						t.Fatal(err)
					}
					for _, p := range []string{exe, entry} {
						if err := os.WriteFile(p, []byte("synthetic fixture only\n"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
				case "symlink":
					exe = filepath.Join(t.TempDir(), "dsh")
					if err := os.Symlink(entry, exe); err != nil {
						t.Skipf("host cannot create symlink: %v", err)
					}
				case "shebang":
					entry = deepSeekRuntimeEntry(t, "")
					exe = entry
				}
				cmd, err := DeepSeekCommand(context.Background(), exe, "--profile", "sdk")
				if err != nil || cmd == nil {
					t.Fatalf("supported npm entry rejected: %v", err)
				}
				if !strings.EqualFold(filepath.Clean(cmd.Path), filepath.Clean(node)) || !reflect.DeepEqual(cmd.Args[1:], []string{entry, "--profile", "sdk"}) {
					t.Fatalf("entry did not use verified Node/literal args: %+v", cmd.Args)
				}
				assertDeepSeekNoEntryExecuted(t, cfg)
			})
		}
	})
}
