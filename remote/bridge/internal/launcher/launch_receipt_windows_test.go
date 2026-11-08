//go:build windows

package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
)

func TestWindowsLaunchReceiptRejectsShellAsToolConfirmation(t *testing.T) {
	status := filepath.Join(t.TempDir(), "launch.status")
	if err := os.WriteFile(status, []byte(`{"state":"shell","pid":123}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if pid, err := waitWindowsLaunchReceipt(ctx, status, "工具入口进程"); err == nil || pid != 0 {
		t.Fatalf("shell PID became tool confirmation: %d %v", pid, err)
	}
	for _, raw := range []string{`{"state":"ready","pid":0}`, `{"state":"ready","pid":12,"message":"private"}`, `{"state":"ready","pid":12,"exitCode":23}`, `{"state":"failed","pid":12,"exitCode":0}`, `{"state":"ready","pid":12} {}`, strings.Repeat("x", 4097)} {
		if _, err := parseWindowsLaunchReceipt([]byte(raw)); err == nil {
			t.Errorf("invalid receipt accepted: %.80s", raw)
		}
	}
}

func TestWindowsLaunchReceiptReportsActualFailureAndPhases(t *testing.T) {
	status := filepath.Join(t.TempDir(), "launch.status")
	if err := os.WriteFile(status+".started", []byte(`{"state":"started","pid":321}`), 0600); err != nil {
		t.Fatal(err)
	}
	phase := make(chan LaunchProgress, 1)
	ctx, cancel := context.WithTimeout(WithLaunchProgress(context.Background(), func(p LaunchProgress) { phase <- p }), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := waitWindowsLaunchReceipt(ctx, status, "工具入口进程"); done <- err }()
	select {
	case progress := <-phase:
		if progress.Step != "confirm" || !strings.Contains(progress.Detail, "工具入口进程") {
			t.Fatalf("phase=%+v", progress)
		}
	case <-ctx.Done():
		t.Fatal("missing process confirmation phase")
	}
	if err := os.WriteFile(status+".tmp", []byte(`{"state":"failed","pid":321,"exitCode":23}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(status+".tmp", status); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "23") {
		t.Fatalf("lost early exit code: %v", err)
	}
	for _, raw := range []string{`{"state":"failed","nativeCode":2,"exception":"private-secret-script"}`, `{"state":"failed","hresult":-2147024891,"exception":"private-secret-script"}`, `{"state":"failed","hresult":-2146233079,"exception":"private-secret-script"}`} {
		receipt, err := parseWindowsLaunchReceipt([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		message := windowsLaunchReceiptError("启动失败", receipt).Error()
		if strings.Contains(message, "private") || !strings.Contains(message, "错误") && !strings.Contains(message, "HRESULT") {
			t.Fatalf("unsafe/uninformative error: %s", message)
		}
	}
}

func TestWindowsLaunchReceiptTimeoutAndCancellationBeforeCreation(t *testing.T) {
	status := filepath.Join(t.TempDir(), "launch.status")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	if pid, err := waitWindowsLaunchReceipt(ctx, status, "工具入口进程"); pid != 0 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout=%d %v", pid, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if pid, err := waitWindowsLaunchReceipt(cancelled, status, "工具入口进程"); pid != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait=%d %v", pid, err)
	}
	if err := os.WriteFile(status, []byte(`{"state":"ready","pid":321}`), 0600); err != nil {
		t.Fatal(err)
	}
	// Receipt publication may win the race with request cancellation, even
	// before the first polling iteration. That process must remain configured.
	if pid, err := waitWindowsLaunchReceipt(cancelled, status, "工具入口进程"); pid != 321 || err != nil {
		t.Fatalf("actual receipt=%d %v", pid, err)
	}
}

func writeWindowsLaunchFixtureReceipt(t *testing.T, path, raw string) {
	t.Helper()
	if err := os.WriteFile(path+".tmp", []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsLaunchReceiptFinishesCreatedProcessAfterRequestEnds(t *testing.T) {
	for _, boundary := range []string{"cancel", "deadline"} {
		for _, outcome := range []string{"ready", "early-exit"} {
			t.Run(boundary+"/"+outcome, func(t *testing.T) {
				status := filepath.Join(t.TempDir(), "launch.status")
				writeWindowsLaunchFixtureReceipt(t, status+".started", `{"state":"started","pid":321}`)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
				defer cancel()
				ctx = WithLaunchProgress(ctx, func(event LaunchProgress) {
					if event.Step != "confirm" || event.State == "warning" {
						return
					}
					if boundary == "cancel" {
						cancel()
					} else {
						<-ctx.Done()
					}
					raw := `{"state":"ready","pid":321}`
					if outcome == "early-exit" {
						raw = `{"state":"failed","pid":321,"exitCode":23}`
					}
					writeWindowsLaunchFixtureReceipt(t, status, raw)
				})
				pid, err := waitWindowsLaunchReceipt(ctx, status, "工具入口进程")
				if outcome == "ready" {
					if pid != 321 || err != nil {
						t.Fatalf("created process lost its receipt: %d %v", pid, err)
					}
				} else if pid != 0 || err == nil || !strings.Contains(err.Error(), "23") || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("request cancellation hid the actual early exit: %d %v", pid, err)
				}
			})
		}
	}
}

func TestWindowsLaunchReceiptPreservesCreatedProcessWhenFinalCheckIsUnavailable(t *testing.T) {
	for _, final := range []string{"missing", "malformed", "helper-error"} {
		t.Run(final, func(t *testing.T) {
			status := filepath.Join(t.TempDir(), "launch.status")
			writeWindowsLaunchFixtureReceipt(t, status+".started", `{"state":"started","pid":321}`)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			warnings := 0
			ctx = WithLaunchProgress(ctx, func(event LaunchProgress) {
				if event.State == "warning" {
					warnings++
					return
				}
				if event.Step != "confirm" {
					return
				}
				cancel()
				switch final {
				case "malformed":
					writeWindowsLaunchFixtureReceipt(t, status, `not-json`)
				case "helper-error":
					writeWindowsLaunchFixtureReceipt(t, status, `{"state":"failed","exception":"System.IO.IOException"}`)
				}
			})
			began := time.Now()
			pid, err := waitWindowsLaunchReceipt(ctx, status, "工具入口进程")
			if pid != 321 || err != nil || warnings != 1 {
				t.Fatalf("created process became rollback or lost uncertainty: %d %v warnings=%d", pid, err, warnings)
			}
			if time.Since(began) > 5*time.Second {
				t.Fatal("post-creation confirmation did not finish within its bound")
			}
		})
	}
}

func TestWindowsLaunchReceiptCancellationKeepsPreparedProfiles(t *testing.T) {
	for _, kind := range []string{"isolated", "switch", "claude-desktop"} {
		t.Run(kind, func(t *testing.T) {
			var local *Service
			var root string
			var launch func(context.Context) (Result, error)
			authorized := false
			authorize := func() error { authorized = true; return nil }
			switch kind {
			case "isolated":
				s, account, tool, workspace, _ := isolatedLaunchFixture(t, "codex", true)
				local, root = s, filepath.Join(s.Dir, "instances", account.ID, account.Kind)
				launch = func(ctx context.Context) (Result, error) {
					return s.LaunchAuthorized(ctx, account, tool.ID, workspace, nil, authorize)
				}
			case "switch":
				s, profile, _ := switchFixture(t, "codex", "codex-desktop")
				local, root = s, profile
				fixtureFile(t, root, "config.toml", "model = \"original\"\n")
				launch = func(ctx context.Context) (Result, error) {
					return s.SwitchAuthorized(ctx, account("codex", "fixture", "synthetic-key"), "codex-desktop", t.TempDir(), nil, authorize)
				}
			case "claude-desktop":
				s, profile, account, _, _ := fixtureClaude3P(t, true)
				local, root = s.Local, profile
				launch = func(ctx context.Context) (Result, error) {
					return s.SwitchAuthorized(ctx, account, nil, true, authorize)
				}
			}
			before := fixtureTree(t, root)
			var prepared map[string]string
			status := filepath.Join(t.TempDir(), "launch.status")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx = WithLaunchProgress(ctx, func(event LaunchProgress) {
				if event.Step == "confirm" && event.State != "warning" {
					cancel()
					writeWindowsLaunchFixtureReceipt(t, status, `{"state":"ready","pid":321}`)
				}
			})
			local.Start = func(ctx context.Context, _ Plan) (int, error) {
				prepared = fixtureTree(t, root)
				writeWindowsLaunchFixtureReceipt(t, status+".started", `{"state":"started","pid":321}`)
				return waitWindowsLaunchReceipt(ctx, status, "工具入口进程")
			}
			result, err := launch(ctx)
			if err != nil || result.PID != 321 || !authorized {
				t.Fatalf("created process returned rollback/revocation outcome: %+v %v authorized=%v", result, err, authorized)
			}
			if reflect.DeepEqual(before, prepared) || !reflect.DeepEqual(prepared, fixtureTree(t, root)) {
				t.Fatal("cancelled request rolled back a created process's prepared profile")
			}
		})
	}
}

// This process is only the current test binary, with an invented environment.
// It never launches Codex/Claude, reads auth files, or calls a provider.
func TestWindowsLaunchSyntheticChild(t *testing.T) {
	switch os.Getenv("SALCARA_SYNTHETIC_LAUNCH_CHILD") {
	case "exit23":
		os.Exit(23)
	case "stay":
		time.Sleep(1600 * time.Millisecond)
		os.Exit(0)
	case "claude-pin-lifetime":
		pin := os.Getenv("SALCARA_SYNTHETIC_CLAUDE_PIN")
		if _, err := os.ReadFile(pin); err != nil {
			os.Exit(24)
		}
		time.Sleep(1600 * time.Millisecond)
		if _, err := os.ReadFile(pin); err != nil {
			os.Exit(25)
		}
		if err := os.WriteFile("claude-pin-verified", []byte("alive-through-child-exit"), 0o600); err != nil {
			os.Exit(26)
		}
		os.Exit(0)
	}
}

func syntheticWindowsLaunchEnvironment(t *testing.T) []string {
	t.Helper()
	values := []string{"GORACE=atexit_sleep_ms=0"}
	for _, key := range []string{"SystemRoot", "WINDIR", "TEMP", "TMP", "PATH", "COMSPEC", "ProgramFiles", "ProgramFiles(x86)", "ProgramW6432", "ProgramData", "PROCESSOR_ARCHITECTURE"} {
		if value := os.Getenv(key); value != "" {
			values = append(values, key+"="+value)
		}
	}
	// Give Windows PowerShell a valid, isolated profile and restrict module
	// discovery to its inbox modules. Hosted runners can have many unrelated
	// modules installed; these fixtures only need Utility and Management.
	profile := t.TempDir()
	roaming := filepath.Join(profile, "AppData", "Roaming")
	local := filepath.Join(profile, "AppData", "Local")
	for _, path := range []string{roaming, local} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return append(values, "USERPROFILE="+profile, "APPDATA="+roaming, "LOCALAPPDATA="+local,
		"PSModulePath="+filepath.Join(filepath.Dir(powershell()), "Modules"))
}

func syntheticWindowsLaunchCommand(t *testing.T, ctx context.Context, script string, environment ...string) (*exec.Cmd, func() string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "synthetic-launch.ps1")
	// Production inherits the visible terminal's console handles. These tests
	// hide the synthetic child and verify only the process/receipt protocol.
	script = strings.ReplaceAll(script, "$psi.UseShellExecute = $false", "$psi.UseShellExecute = $false\n  $psi.CreateNoWindow = $true")
	script = strings.ReplaceAll(script, "$psi.CreateNoWindow = $false", "$psi.CreateNoWindow = $true")
	script = "$ProgressPreference = 'SilentlyContinue'\n[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)\n[Console]::Error.WriteLine('fixture: script entered')\n" + script
	// Use only .NET calls for markers: the first cmdlet can itself block in
	// Windows PowerShell command discovery, before Process.Start is reached.
	script = strings.ReplaceAll(script, "$child = $null", "[Console]::Error.WriteLine('fixture: CLI initialization')\n$child = $null")
	script = strings.ReplaceAll(script, "$d = Get-Content", "[Console]::Error.WriteLine('fixture: reading packaged JSON')\n  $d = Get-Content")
	script = strings.ReplaceAll(script, "$psi = New-Object System.Diagnostics.ProcessStartInfo", "[Console]::Error.WriteLine('fixture: resolving New-Object')\n  $psi = New-Object System.Diagnostics.ProcessStartInfo\n  [Console]::Error.WriteLine('fixture: process info created')")
	script = strings.ReplaceAll(script, "$child = [System.Diagnostics.Process]::Start($psi)", "[Console]::Error.WriteLine('fixture: starting child')\n  $child = [System.Diagnostics.Process]::Start($psi)\n  [Console]::Error.WriteLine('fixture: child started')")
	script = strings.ReplaceAll(script, "function Write-LaunchReceipt($value) {", "function Write-LaunchReceipt($value) {\n  [Console]::Error.WriteLine('fixture: receipt ' + $value.state)\n")
	if err := os.WriteFile(path, append([]byte{0xef, 0xbb, 0xbf}, []byte(script)...), 0600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "powershell.log")
	output, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = output.Close() })
	cmd := exec.CommandContext(ctx, powershell(), "-NoLogo", "-NoProfile", "-NonInteractive", "-OutputFormat", "Text", "-ExecutionPolicy", "Bypass", "-File", path)
	cmd.Env = append(syntheticWindowsLaunchEnvironment(t), environment...)
	// Use files instead of pipes: a synthetic child's inherited output handle
	// must not keep cmd.Wait blocked after a supervisor timeout. Reading this
	// file for diagnostics is also safe while the supervisor is still running.
	cmd.Stdout, cmd.Stderr = output, output
	configureProcess(cmd, false)
	return cmd, func() string {
		data, err := os.ReadFile(logPath)
		if err != nil {
			return err.Error()
		}
		return string(data)
	}
}

func runSyntheticWindowsLaunchScript(t *testing.T, script, status, mode string) windowsLaunchReceipt {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd, diagnostics := syntheticWindowsLaunchCommand(t, ctx, script, "SALCARA_SYNTHETIC_LAUNCH_CHILD="+mode)
	if err := cmd.Run(); err != nil {
		t.Fatalf("synthetic launcher script failed: %v; context: %v\n%s", err, ctx.Err(), diagnostics())
	}
	data, err := os.ReadFile(status)
	if err != nil {
		t.Fatalf("missing launch receipt: %v\n%s", err, diagnostics())
	}
	if strings.Contains(string(data), "synthetic-private") || strings.Contains(string(data), "ProcessStartInfo") {
		t.Fatal("receipt leaked private input or source")
	}
	receipt, err := parseWindowsLaunchReceipt(data)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.State == "ready" && receipt.PID > 0 {
		if process, err := os.FindProcess(receipt.PID); err == nil {
			_, _ = process.Wait() // Only this test's short-lived synthetic child.
		}
	}
	return receipt
}

func TestWindowsLaunchScriptsProduceTargetReceiptsWithoutRealTools(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"cli", "packaged"} {
		for _, mode := range []string{"exit23", "missing", "stay"} {
			t.Run(kind+"_"+mode, func(t *testing.T) {
				dir := t.TempDir()
				status := filepath.Join(dir, "launch.status")
				p := Plan{Tool: Tool{ID: "codex", Path: executable}, Workspace: dir, Args: []string{"-test.run=^TestWindowsLaunchSyntheticChild$"}}
				if mode == "missing" {
					p.Tool.Path = filepath.Join(dir, "synthetic-private-key-no-such-executable.exe")
				}
				script := windowsCLILaunchScript(p, status)
				if kind == "packaged" {
					args := []string{}
					for _, arg := range p.Args {
						args = append(args, syscall.EscapeArg(arg))
					}
					data, _ := json.Marshal(map[string]any{"exe": p.Tool.Path, "args": strings.Join(args, " "), "cwd": dir, "env": map[string]string{"OPENAI_API_KEY": "synthetic-private-key"}})
					dataPath := filepath.Join(dir, "launch.json")
					if err := os.WriteFile(dataPath, data, 0600); err != nil {
						t.Fatal(err)
					}
					script = windowsPackagedLaunchScript(dataPath, status)
				}
				receipt := runSyntheticWindowsLaunchScript(t, script, status, mode)
				switch mode {
				case "exit23":
					if receipt.State != "failed" || receipt.ExitCode == nil || *receipt.ExitCode != 23 || receipt.PID <= 0 {
						t.Fatalf("lost target exit: %+v", receipt)
					}
				case "missing":
					if receipt.State != "failed" || receipt.NativeCode == nil || *receipt.NativeCode != 2 {
						t.Fatalf("lost native cause: %+v", receipt)
					}
					message := windowsLaunchReceiptError("创建失败", receipt).Error()
					if strings.Contains(message, "synthetic-private") || !strings.Contains(message, "2") {
						t.Fatalf("unsafe failure: %s", message)
					}
				case "stay":
					if receipt.State != "ready" || receipt.PID <= 0 {
						t.Fatalf("no target receipt: %+v", receipt)
					}
				}
			})
		}
	}
}

func TestWindowsLaunchScriptPreservesInteractiveHandlesAndNoRawErrors(t *testing.T) {
	p := Plan{Tool: Tool{Path: `C:\synthetic\tool.exe`}, Workspace: `C:\synthetic`, Args: []string{"name'with quote", "model;not-a-command"}}
	script := windowsCLILaunchScript(p, `C:\synthetic\private.status`)
	for _, required := range []string{"$psi.UseShellExecute = $false", "$psi.RedirectStandardInput = $false", "$psi.RedirectStandardOutput = $false", "$psi.RedirectStandardError = $false", "$child.WaitForExit(1200)", "exitCode=[int]$child.ExitCode", "name''with quote"} {
		if !strings.Contains(script, required) {
			t.Errorf("missing process boundary %q", required)
		}
	}
	for _, forbidden := range []string{".Message", "Format-List", "Out-String", "Write-Output $child.Id"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("unsafe raw diagnostic: %q", forbidden)
		}
	}
	err := safeWindowsLaunchError("创建失败", &os.PathError{Op: "open", Path: "synthetic-private-key", Err: syscall.Errno(5)})
	if strings.Contains(err.Error(), "synthetic-private") || !strings.Contains(err.Error(), "5") {
		t.Fatalf("unsafe system error: %v", err)
	}
}

func TestWindowsClaudeAPIPinLifetimeAndExactCleanup(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"claude-pin-lifetime", "exit23", "missing", "receipt-failure", "unrelated-file"} {
		t.Run(mode, func(t *testing.T) {
			pin, err := agents.WriteClaudeRuntimeSettings(`{"env":{"ANTHROPIC_AUTH_TOKEN":"synthetic-private-key"}}`)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { agents.RemoveClaudeRuntimeSettings(pin) })
			dir := t.TempDir()
			status := filepath.Join(dir, "launch.status")
			p := Plan{Tool: Tool{ID: "claude", Path: executable}, Workspace: dir, Args: []string{"-test.run=^TestWindowsLaunchSyntheticChild$"}, ClaudeSettingsPin: pin}
			if mode == "missing" {
				p.Tool.Path = filepath.Join(dir, "missing.exe")
			}
			other := ""
			if mode == "unrelated-file" {
				other = filepath.Join(filepath.Dir(pin), "preserve.txt")
				if err := os.WriteFile(other, []byte("preserved"), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Remove(other); _ = os.Remove(filepath.Dir(pin)) })
			}
			script := windowsCLILaunchScript(p, status)
			if strings.Contains(script, "synthetic-private-key") || strings.Contains(script, "-Recurse") || !strings.Contains(script, "Remove-Item -LiteralPath") {
				t.Fatal("unsafe credential or cleanup script")
			}
			script = strings.Replace(script, "$child = [System.Diagnostics.Process]::Start($psi)", "$psi.EnvironmentVariables['SALCARA_SYNTHETIC_CLAUDE_PIN'] = "+psLiteral(pin)+"\n  $child = [System.Diagnostics.Process]::Start($psi)", 1)
			childMode := mode
			if mode == "unrelated-file" || mode == "receipt-failure" {
				childMode = "claude-pin-lifetime"
			}
			if mode == "receipt-failure" {
				// Simulate receipt I/O failure after creating a live child. The
				// supervisor must still wait before deleting its API settings.
				script = strings.Replace(script, "Write-LaunchReceipt @{state='started';pid=$child.Id}", "throw [IO.IOException]::new('synthetic receipt failure')", 1)
			}
			receipt := runSyntheticWindowsLaunchScript(t, script, status, childMode)
			if childMode == "claude-pin-lifetime" {
				if data, err := os.ReadFile(filepath.Join(dir, "claude-pin-verified")); err != nil || string(data) != "alive-through-child-exit" {
					t.Fatal("API settings disappeared while the child was still running")
				}
			}
			if mode == "claude-pin-lifetime" || mode == "unrelated-file" {
				if receipt.State != "ready" {
					t.Fatalf("pin unavailable before child exit: %+v", receipt)
				}
			} else if receipt.State != "failed" {
				t.Fatalf("missing launch failure: %+v", receipt)
			}
			if _, err := os.Stat(pin); !os.IsNotExist(err) {
				t.Fatal("pin survived child completion")
			}
			if other != "" {
				if data, err := os.ReadFile(other); err != nil || string(data) != "preserved" {
					t.Fatal("cleanup deleted unrelated content")
				}
			} else if _, err := os.Stat(filepath.Dir(pin)); !os.IsNotExist(err) {
				t.Fatal("private empty directory survived")
			}
		})
	}
}
