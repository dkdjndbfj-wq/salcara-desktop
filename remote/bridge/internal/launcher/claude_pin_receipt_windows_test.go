//go:build windows

package launcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
)

func TestClaudePinWaiterSyntheticChild(t *testing.T) {
	if os.Getenv("SALCARA_PIN_WAITER_CHILD") != "1" {
		return
	}
	pin := os.Getenv("SALCARA_PIN_WAITER_PATH")
	if _, err := os.ReadFile(pin); err != nil {
		os.Exit(24)
	}
	if err := os.WriteFile("child-alive", []byte("ready"), 0600); err != nil {
		os.Exit(25)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat("release-child"); err == nil {
			if _, err := os.ReadFile(pin); err != nil {
				os.Exit(26)
			}
			_ = os.WriteFile("pin-retained", []byte("verified"), 0600)
			os.Exit(0)
		}
		time.Sleep(20 * time.Millisecond)
	}
	os.Exit(27)
}

func TestClaudePinSurvivesWaiterReceiptFailuresUntilChildExit(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"first-started-write", "all-receipts-lost"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			status := filepath.Join(dir, "launch.status")
			var pin string
			var childDone chan struct{}
			var supervisorErr error
			var diagnostics func() string
			warnings := 0
			ctx := WithLaunchProgress(context.Background(), func(progress LaunchProgress) {
				if progress.State == "warning" {
					warnings++
				}
			})
			s := &Service{Start: func(_ context.Context, plan Plan) (int, error) {
				pin = plan.ClaudeSettingsPin
				plan.Tool.Path = executable
				plan.Args = []string{"-test.run=^TestClaudePinWaiterSyntheticChild$"}
				plan.Workspace = dir
				script := windowsCLILaunchScript(plan, status)
				if mode == "first-started-write" {
					script = strings.Replace(script, "Write-LaunchReceipt @{state='started';pid=$child.Id}", "throw [IO.IOException]::new('synthetic first receipt failure')", 1)
				} else {
					script = strings.Replace(script, "function Write-LaunchReceipt($value) {", "function Write-LaunchReceipt($value) { throw [IO.IOException]::new('synthetic all receipt failure')", 1)
				}
				supervisorCtx, cancelSupervisor := context.WithCancel(context.Background())
				cmd, output := syntheticWindowsLaunchCommand(t, supervisorCtx, script, "SALCARA_PIN_WAITER_CHILD=1", "SALCARA_PIN_WAITER_PATH="+pin)
				diagnostics = output
				t.Cleanup(cancelSupervisor)
				if err := cmd.Start(); err != nil {
					return 0, err
				}
				childDone = make(chan struct{})
				go func() { supervisorErr = cmd.Wait(); close(childDone) }()
				// Register after command resources so they stay available until
				// the owned supervisor and its bounded synthetic worker finish.
				t.Cleanup(func() {
					_ = os.WriteFile(filepath.Join(dir, "release-child"), nil, 0600)
					select {
					case <-childDone:
					case <-time.After(20 * time.Second):
						cancelSupervisor()
						<-childDone
						t.Errorf("synthetic supervisor did not exit\n%s", diagnostics())
					}
				})
				deadline := time.Now().Add(10 * time.Second)
				for {
					if _, err := os.Stat(filepath.Join(dir, "child-alive")); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("synthetic worker did not start\n%s", diagnostics())
					}
					select {
					case <-childDone:
						t.Fatalf("synthetic supervisor exited before worker started: %v\n%s", supervisorErr, diagnostics())
					default:
					}
					time.Sleep(20 * time.Millisecond)
				}
				waitCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
				defer cancel()
				return waitWindowsLaunchReceiptWithPin(waitCtx, status, "工具入口进程", true)
			}}
			t.Cleanup(func() { agents.RemoveClaudeRuntimeSettings(pin) })
			pid, err := s.startWithClaudeAPIPin(ctx, Plan{Tool: Tool{ID: "claude", Kind: "claude"}}, account("claude", "native", "synthetic-key"))
			if err != nil || warnings != 1 || mode == "first-started-write" && pid <= 0 || mode == "all-receipts-lost" && pid != 0 {
				t.Fatalf("receipt failure became rollback or false PID: pid=%d err=%v warnings=%d", pid, err, warnings)
			}
			if _, err := os.ReadFile(pin); err != nil {
				t.Fatal("caller deleted the live worker's API pin")
			}
			if err := os.WriteFile(filepath.Join(dir, "release-child"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			select {
			case <-childDone:
			case <-time.After(5 * time.Second):
				t.Fatalf("supervisor did not finish\n%s", diagnostics())
			}
			if _, err := os.Stat(filepath.Join(dir, "pin-retained")); err != nil {
				t.Fatal("child did not retain settings until exit")
			}
			if _, err := os.Stat(pin); !os.IsNotExist(err) {
				t.Fatal("supervisor did not clean its pin after child exit")
			}
		})
	}
}

func TestClaudePinWaiterStillRejectsConfirmedLaunchFailures(t *testing.T) {
	for _, raw := range []string{`{"state":"failed","nativeCode":2}`, `{"state":"failed","pid":123,"exitCode":23}`} {
		status := filepath.Join(t.TempDir(), "launch.status")
		if err := os.WriteFile(status, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if pid, err := waitWindowsLaunchReceiptWithPin(context.Background(), status, "工具入口进程", true); pid != 0 || err == nil {
			t.Fatalf("confirmed failure was preserved: pid=%d err=%v", pid, err)
		}
	}
}
