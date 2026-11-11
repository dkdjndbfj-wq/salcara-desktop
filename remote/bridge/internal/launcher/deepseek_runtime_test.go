package launcher

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
	"sync"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/toolcfg"
)

type launcherDeepSeekRuntimeFixture struct {
	node, entry, shim, log string
	config                 map[string]any
}

func newLauncherDeepSeekRuntimeFixture(t *testing.T, helper []byte) *launcherDeepSeekRuntimeFixture {
	t.Helper()
	dir := t.TempDir()
	node := filepath.Join(dir, "node")
	if runtime.GOOS == "windows" {
		node += ".exe"
	}
	if err := os.WriteFile(node, helper, 0o700); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(dir, "node_modules", "@deepseek-ai", "dsh", "lib", "bin.js")
	if err := os.MkdirAll(filepath.Dir(entry), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte("// Synthetic entry: never evaluated.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(dir, "dsh.cmd")
	if err := os.WriteFile(shim, []byte("synthetic shim: never run"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &launcherDeepSeekRuntimeFixture{node: node, entry: entry, shim: shim, log: filepath.Join(dir, "calls.jsonl")}
	f.config = map[string]any{"mode": "node", "log": f.log, "nodeOutput": "v24.9.0\n", "entryOutput": "dsh 0.2.2-rc.1\n"}
	f.save(t)
	// Cold startup of a newly copied test executable is setup, not the Node
	// version behavior under test. This branch reads no fixture configuration,
	// writes no call record and does not relax the production three-second probe.
	readyCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ready := exec.CommandContext(readyCtx, node, "--salcara-fixture-ready")
	configureProcess(ready, false)
	ready.WaitDelay = 250 * time.Millisecond
	if output, err := ready.CombinedOutput(); err != nil || len(output) != 0 {
		t.Fatalf("synthetic runtime readiness: error=%v output bytes=%d", err, len(output))
	}
	if _, err := os.Stat(f.log); !os.IsNotExist(err) {
		t.Fatal("fixture readiness must not produce runtime call records", err)
	}
	// Unix discovery prefers the cached login-shell PATH over PATH. The
	// prerequisite scenarios must execute this synthetic Node, not the host's.
	t.Setenv(agents.ShellPathEnv, "1")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return f
}

func (f *launcherDeepSeekRuntimeFixture) save(t *testing.T) {
	t.Helper()
	raw, err := json.Marshal(f.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.node+".json", raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *launcherDeepSeekRuntimeFixture) assertNoEntry(t *testing.T) {
	t.Helper()
	raw, err := os.ReadFile(f.log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "entry-") {
		t.Fatal("unusable Node reached the npm entry")
	}
}

func launcherDeepSeekTool(path string) Tool {
	return Tool{ID: "deepseek-harness", Kind: "deepseek-harness", Name: "DeepSeek Harness", Path: path, Available: true}
}

func TestLauncherDeepSeekRuntime(t *testing.T) {
	fixtureExe := filepath.Join(t.TempDir(), "runtime-fixture")
	if runtime.GOOS == "windows" {
		fixtureExe += ".exe"
	}
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", fixtureExe, "../agents/testdata/deepseek_runtime_helper")
	configureProcess(build, false)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("synthetic runtime build: %v: %s", err, output)
	}
	helper, err := os.ReadFile(fixtureExe)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("discovery fails closed and keeps private diagnostic", func(t *testing.T) {
		f := newLauncherDeepSeekRuntimeFixture(t, helper)
		f.config["nodeOutput"] = "v24.8.0\n"
		f.save(t)
		paths := map[string]string{}
		for _, id := range toolOrder {
			paths[id] = filepath.Join(t.TempDir(), "missing")
		}
		paths["deepseek-harness"] = f.entry
		var got Tool
		for _, tool := range Discover(context.Background(), paths) {
			if tool.ID == "deepseek-harness" {
				got = tool
			}
		}
		if got.Available || got.Path != f.entry || got.runtimeError == nil || !strings.Contains(got.runtimeError.Error(), "24.9.0") {
			t.Fatalf("invalid discovery outcome: available=%v diagnostic=%v", got.Available, got.runtimeError)
		}
		raw, _ := json.Marshal(got)
		if strings.Contains(string(raw), "runtimeError") || strings.Contains(string(raw), "24.9.0") {
			t.Fatal("private diagnostic changed public inventory shape")
		}
		s := New(t.TempDir())
		_, err := s.Prepare(account("deepseek-harness", "fixture", "synthetic-key"), got, t.TempDir())
		if err != got.runtimeError {
			t.Fatal("prepare replaced prerequisite diagnostic with missing-tool message", err)
		}
		s.FindTools = func(context.Context, map[string]string) []Tool { return []Tool{got} }
		_, err = s.PrepareSwitch(context.Background(), account("deepseek-harness", "fixture", "synthetic-key"), got.ID, t.TempDir(), paths)
		if err != got.runtimeError {
			t.Fatal("switch replaced prerequisite diagnostic", err)
		}
		f.assertNoEntry(t)
	})

	t.Run("stale available launch restores profiles and cannot start", func(t *testing.T) {
		f := newLauncherDeepSeekRuntimeFixture(t, helper)
		f.config["nodeOutput"] = "v22.0.0\n"
		f.save(t)
		home := t.TempDir()
		t.Setenv("DSH_HOME", home)
		s := New(t.TempDir())
		tool := launcherDeepSeekTool(f.shim)
		s.FindTools = func(context.Context, map[string]string) []Tool { return []Tool{tool} }
		s.Start = func(context.Context, Plan) (int, error) { t.Fatal("started unsupported runtime"); return 0, nil }
		s.Stop = func(context.Context, Plan) error { t.Fatal("stopped a tool before prerequisite check"); return nil }
		a := account(tool.Kind, "fixture", "synthetic-key")
		for _, launch := range []func() error{
			func() error {
				_, err := s.LaunchAuthorized(context.Background(), a, tool.ID, t.TempDir(), nil, nil)
				return err
			},
			func() error {
				_, err := s.SwitchAuthorized(context.Background(), a, tool.ID, t.TempDir(), nil, nil)
				return err
			},
		} {
			if err := launch(); err == nil || !strings.Contains(err.Error(), "24.9.0") {
				t.Fatal("unsupported Node did not produce prerequisite failure", err)
			}
		}
		for _, dir := range []string{home, filepath.Join(s.Dir, "instances", a.ID, a.Kind)} {
			for _, path := range toolcfg.AdditionalPaths(tool.Kind, dir) {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("failed prerequisite left profile configuration", path, err)
				}
			}
		}
		f.assertNoEntry(t)
	})

	t.Run("remembered path does not revive failed runtime", func(t *testing.T) {
		f := newLauncherDeepSeekRuntimeFixture(t, helper)
		f.config["nodeOutput"] = "v24.8.0\n"
		f.save(t)
		s := New(t.TempDir())
		s.rememberLocations([]Tool{launcherDeepSeekTool(f.entry)})
		s.FindTools = func(context.Context, map[string]string) []Tool {
			return []Tool{{ID: "deepseek-harness", Kind: "deepseek-harness"}}
		}
		got := s.Tools(context.Background(), nil)
		if len(got) != 1 || got[0].Available || got[0].runtimeError == nil {
			t.Fatal("remembered file falsely advertised usable DeepSeek")
		}
		f.assertNoEntry(t)
	})

	t.Run("cold inventory waits for authoritative check", func(t *testing.T) {
		f := newLauncherDeepSeekRuntimeFixture(t, helper)
		for _, custom := range []bool{false, true} {
			s := New(t.TempDir())
			s.rememberLocations([]Tool{launcherDeepSeekTool(f.entry)})
			release := make(chan struct{})
			releaseProbe := sync.OnceFunc(func() { close(release) })
			defer releaseProbe()
			s.FindTools = func(context.Context, map[string]string) []Tool {
				<-release
				return []Tool{{ID: "deepseek-harness", Kind: "deepseek-harness", Path: f.entry, Custom: true}}
			}
			paths := map[string]string{}
			if custom {
				paths["deepseek-harness"] = f.entry
			}
			start := time.Now()
			got, loading := s.InventorySnapshot(paths)
			if !loading || time.Since(start) > time.Second {
				t.Fatal("inventory blocked on runtime verification")
			}
			for _, tool := range got {
				if tool.ID == "deepseek-harness" && tool.Available {
					t.Error("cold inventory treated file presence as readiness")
				}
			}
			s.inventoryMu.Lock()
			done := s.inventoryFlight.done
			s.inventoryMu.Unlock()
			releaseProbe()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("synthetic inventory failed to finish")
			}
		}
	})

	t.Run("npm local and remote select same runtime and direct entry", func(t *testing.T) {
		f := newLauncherDeepSeekRuntimeFixture(t, helper)
		for _, entry := range []string{f.entry, f.shim} {
			p := Plan{Tool: launcherDeepSeekTool(entry), Args: []string{"web"}}
			if err := normalizeCLIContext(context.Background(), &p); err != nil {
				t.Fatal(err)
			}
			remote, err := agents.DeepSeekCommand(context.Background(), entry, "web")
			if err != nil || p.Tool.Path != remote.Path || !reflect.DeepEqual(p.Args, remote.Args[1:]) || !reflect.DeepEqual(p.Args, []string{f.entry, "web"}) {
				t.Fatal("local and remote disagree on runtime/entry", err)
			}
		}
		f.assertNoEntry(t)
	})

	t.Run("native entry remains direct web", func(t *testing.T) {
		f := newLauncherDeepSeekRuntimeFixture(t, helper)
		f.config["nodeOutput"] = "v22.0.0\n"
		f.save(t)
		p := Plan{Tool: launcherDeepSeekTool(fixtureExe), Args: []string{"web"}}
		if err := normalizeCLIContext(context.Background(), &p); err != nil || p.Tool.Path != fixtureExe || !reflect.DeepEqual(p.Args, []string{"web"}) {
			t.Fatal("native launch acquired Node or history selection", err)
		}
		if _, err := os.Stat(f.log); !os.IsNotExist(err) {
			t.Fatal("native command checked unrelated Node")
		}
	})

	t.Run("caller deadline cancels local prerequisite probe", func(t *testing.T) {
		f := newLauncherDeepSeekRuntimeFixture(t, helper)
		f.config["nodeSleepMS"] = 10000
		f.save(t)
		s := New(t.TempDir())
		tool := launcherDeepSeekTool(f.entry)
		s.FindTools = func(context.Context, map[string]string) []Tool { return []Tool{tool} }
		s.Start = func(context.Context, Plan) (int, error) { t.Fatal("cancelled launch started"); return 0, nil }
		a := account(tool.Kind, "fixture", "synthetic-key")
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := s.LaunchAuthorized(ctx, a, tool.ID, t.TempDir(), nil, nil)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
			t.Fatal("launch discarded caller deadline", err)
		}
		for _, path := range toolcfg.AdditionalPaths(tool.Kind, filepath.Join(s.Dir, "instances", a.ID, a.Kind)) {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("cancelled prerequisite left profile configuration", path, err)
			}
		}
		f.assertNoEntry(t)
	})
}
