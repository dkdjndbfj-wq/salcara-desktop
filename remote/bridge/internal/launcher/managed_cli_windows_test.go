//go:build windows

package launcher

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"salcara/bridge/internal/config"
)

// All processes in these tests are this test binary in an isolated directory.
// No installed Agent, real configuration or user task is opened or stopped.
func TestManagedCLIFixture(t *testing.T) {
	role := os.Getenv("SALCARA_OWNED_CLI_FIXTURE")
	if role == "" {
		return
	}
	if role == "worker" {
		for {
			time.Sleep(time.Second)
		}
	}
	if role != "parent" {
		os.Exit(2)
	}
	exe, _ := os.Executable()
	child := exec.Command(exe, "-test.run=^TestManagedCLIFixture$")
	child.Env = append(os.Environ(), "SALCARA_OWNED_CLI_FIXTURE=worker")
	configureProcess(child, false)
	if child.Start() != nil {
		os.Exit(3)
	}
	if os.WriteFile(os.Getenv("SALCARA_OWNED_CLI_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0600) != nil {
		os.Exit(4)
	}
	if release := os.Getenv("SALCARA_OWNED_CLI_FAIL"); release != "" {
		until := time.Now().Add(5 * time.Second)
		for time.Now().Before(until) {
			if _, err := os.Stat(release); err == nil {
				os.Exit(23)
			}
			time.Sleep(5 * time.Millisecond)
		}
		os.Exit(24)
	}
	// The root exits while its descendant keeps running inside the Job.
	os.Exit(0)
}

func managedFixture(t *testing.T, root, target string) (Plan, string, string, managedCLIReceipt) {
	t.Helper()
	dir := filepath.Join(root, "launch-scratch", target, "managed-cli-v1")
	if err := managedCLIPrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	host, boot, owner, err := managedCLIIdentity()
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	id := config.RandomToken(32)
	pidFile := filepath.Join(dir, id+".pid")
	p := Plan{Tool: Tool{ID: target, Kind: target, Path: exe}, Shared: true, RuntimeDir: filepath.Dir(dir), ProfileDir: filepath.Join(root, "profile"), Workspace: root, Args: []string{"-test.run=^TestManagedCLIFixture$"}, Environment: append(os.Environ(), "SALCARA_OWNED_CLI_FIXTURE=parent", "SALCARA_OWNED_CLI_PID="+pidFile)}
	r := managedCLIReceipt{Protocol: managedCLIProtocol, ID: id, Host: host, Boot: boot, Owner: owner, Target: target, Family: config.ToolFamily(target), Shared: true, State: "pending"}
	path := filepath.Join(dir, r.ID+".json")
	if err := writeManagedCLI(path, r); err != nil {
		t.Fatal(err)
	}
	return p, path, pidFile, r
}

func managedTestRoot(t *testing.T) string {
	t.Helper()
	// PowerShell 5/.NET Framework still has legacy path-length limits. Do not
	// prefix the full test name (t.TempDir) before a 256-bit receipt filename.
	root, err := os.MkdirTemp("", "scli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

func runManagedFixture(t *testing.T, p Plan, path string, r managedCLIReceipt) *exec.Cmd {
	t.Helper()
	script := filepath.Join(filepath.Dir(path), r.ID+".ps1")
	if err := os.WriteFile(script, append([]byte{0xef, 0xbb, 0xbf}, []byte(managedCLIScript(p, path, path+".status", r.ID))...), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(powershell(), "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script)
	cmd.Env = p.Environment
	configureProcess(cmd, false)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = stopManagedCLIReceipt(ctx, path, r.Host, r.Boot, r.Owner)
		select {
		case err := <-done:
			if err != nil {
				t.Logf("synthetic supervisor: %v %s", err, output.String())
			}
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			t.Error("synthetic supervisor failed to exit")
		}
	})
	return cmd
}

func waitManagedWorker(t *testing.T, path string) windows.Handle {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			pid, e := strconv.Atoi(string(b))
			if e != nil {
				t.Fatal(e)
			}
			h, e := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
			if e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { windows.CloseHandle(h) })
			return h
		}
		time.Sleep(30 * time.Millisecond)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".status") || strings.HasSuffix(e.Name(), ".json") {
			b, _ := os.ReadFile(filepath.Join(filepath.Dir(path), e.Name()))
			t.Logf("synthetic receipt %s: %s", e.Name(), b)
		}
	}
	t.Fatal("synthetic descendant did not start")
	return 0
}

func TestManagedCLIStopsWholeTreeAfterRootExitAndPreservesOtherAgent(t *testing.T) {
	root := managedTestRoot(t)
	p, path, pidFile, r := managedFixture(t, root, "hermes")
	runManagedFixture(t, p, path, r)
	worker := waitManagedWorker(t, pidFile)
	other, otherPath, otherPID, otherR := managedFixture(t, root, "codex")
	runManagedFixture(t, other, otherPath, otherR)
	unrelated := waitManagedWorker(t, otherPID)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	paths, err := managedCLIFamilyReceipts(p)
	if err != nil || len(paths) != 1 || paths[0] != path {
		t.Fatalf("family isolation: %v %v", paths, err)
	}
	if err := stopManagedCLIReceipt(ctx, path, r.Host, r.Boot, r.Owner); err != nil {
		t.Fatal(err)
	}
	if status, err := windows.WaitForSingleObject(worker, 2000); err != nil || status != windows.WAIT_OBJECT_0 {
		t.Fatalf("owned descendant still running: %d %v", status, err)
	}
	if status, err := windows.WaitForSingleObject(unrelated, 0); err != nil || status != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("unrelated Agent was stopped: %d %v", status, err)
	}
	final, err := readManagedCLI(path)
	if err != nil || !final.Zero || final.State != "stopped" || !final.Cancelled {
		t.Fatalf("missing tree-zero proof: %+v %v", final, err)
	}
	if err := stopManagedCLIReceipt(ctx, path, r.Host, r.Boot, r.Owner); err != nil {
		t.Fatalf("repeat stop: %v", err)
	}
}

func TestManagedCLICancelledPendingIntentPreventsLateHelper(t *testing.T) {
	p, path, pidFile, r := managedFixture(t, managedTestRoot(t), "hermes")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := stopManagedCLIReceipt(ctx, path, r.Host, r.Boot, r.Owner); err != nil {
		t.Fatal(err)
	}
	runManagedFixture(t, p, path, r)
	if _, err := waitWindowsLaunchReceipt(ctx, path+".status", "synthetic fixture"); err == nil {
		t.Fatal("revoked helper reported success")
	}
	if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
		t.Fatalf("late revoked helper launched a child: %v", err)
	}
	final, err := readManagedCLI(path)
	if err != nil || !final.Cancelled || !final.Zero {
		t.Fatalf("lost tombstone: %+v %v", final, err)
	}
}

func TestManagedCLIMissingJobUnknownAndIdentityFailClosed(t *testing.T) {
	for _, boundary := range []string{"missing-job", "foreign-host", "foreign-owner", "malformed", "cancelled"} {
		t.Run(boundary, func(t *testing.T) {
			_, path, _, r := managedFixture(t, managedTestRoot(t), "hermes")
			r.State = "running"
			r.PID = 123
			r.Created = "123456789"
			if boundary == "foreign-host" {
				r.Host = strings.Repeat("a", 64)
			}
			if boundary == "foreign-owner" {
				r.Owner = "S-1-5-999"
			}
			if err := writeManagedCLI(path, r); err != nil {
				t.Fatal(err)
			}
			if boundary == "malformed" {
				if err := os.WriteFile(path, []byte(`{"state":"running"}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			host, boot, owner, err := managedCLIIdentity()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if boundary == "cancelled" {
				cancel()
			}
			err = stopManagedCLIReceipt(ctx, path, host, boot, owner)
			if err == nil {
				t.Fatal("unproven stop accepted")
			}
			if boundary == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if boundary != "malformed" {
				after, e := readManagedCLI(path)
				if e != nil || after.Zero {
					t.Fatalf("invented zero proof: %+v %v", after, e)
				}
			}
		})
	}
}

func TestManagedCLISameHostBootChangeProvesOldTreeEnded(t *testing.T) {
	_, path, _, r := managedFixture(t, managedTestRoot(t), "hermes")
	r.State = "running"
	r.Boot = strings.Repeat("a", 64)
	if err := writeManagedCLI(path, r); err != nil {
		t.Fatal(err)
	}
	_, boot, _, err := managedCLIIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := stopManagedCLIReceipt(context.Background(), path, r.Host, boot, r.Owner); err != nil {
		t.Fatal(err)
	}
	final, err := readManagedCLI(path)
	if err != nil || !final.Zero {
		t.Fatalf("restart proof not persisted: %+v %v", final, err)
	}
}

func TestManagedCLIEarlyFailureCannotPermitRollbackWhileDescendantLives(t *testing.T) {
	root := managedTestRoot(t)
	p, path, pidFile, r := managedFixture(t, root, "hermes")
	release := filepath.Join(root, "release-failure")
	p.Environment = append(p.Environment, "SALCARA_OWNED_CLI_FAIL="+release)
	runManagedFixture(t, p, path, r)
	worker := waitManagedWorker(t, pidFile)
	if err := os.WriteFile(release, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := waitWindowsLaunchReceiptWithPin(ctx, path+".status", "synthetic fixture", true); err == nil || !strings.Contains(err.Error(), "23") {
		t.Fatalf("early failure lost: %v", err)
	}
	if status, err := windows.WaitForSingleObject(worker, 0); err != nil || status != windows.WAIT_OBJECT_0 {
		t.Fatalf("rollback-capable failure left a descendant alive: %d %v", status, err)
	}
}

func TestManagedCLIPrivateInstanceExemptionRequiresKernelJobMembership(t *testing.T) {
	root := managedTestRoot(t)
	p, path, pidFile, r := managedFixture(t, root, "hermes")
	p.Shared = false
	r.Shared = false
	if err := writeManagedCLI(path, r); err != nil {
		t.Fatal(err)
	}
	runManagedFixture(t, p, path, r)
	worker := waitManagedWorker(t, pidFile)
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(b))
	if err != nil {
		t.Fatal(err)
	}
	private, err := managedCLIPrivateProcess(uint32(pid), []string{path}, r.Host, r.Boot, r.Owner)
	if err != nil || !private {
		t.Fatalf("owned private tree not recognized: %v %v", private, err)
	}
	private, err = managedCLIPrivateProcess(uint32(os.Getpid()), []string{path}, r.Host, r.Boot, r.Owner)
	if err != nil || private {
		t.Fatalf("unrelated PID received exemption: %v %v", private, err)
	}
	if status, err := windows.WaitForSingleObject(worker, 0); err != nil || status != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("private instance was stopped: %d %v", status, err)
	}
}

func TestManagedCLIExecutableInventoryDoesNotClaimEveryNodeProcess(t *testing.T) {
	p := Plan{Tool: Tool{ID: "codex", Kind: "codex", Path: `C:\runtime\node.exe`}, Args: []string{`C:\npm\node_modules\@openai\codex\bin\codex.js`}}
	for _, tc := range []struct {
		name, image, command string
		match                bool
	}{
		{"selected entry", "node.exe", `node.exe C:\npm\node_modules\@openai\codex\bin\codex.js`, true},
		{"unrelated node", "node.exe", `node.exe E:\synthetic\preview-server.mjs`, false},
		{"other agent node", "node.exe", `node.exe C:\npm\node_modules\@anthropic-ai\claude-code\cli.js`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedCLIProcessMatches(managedCLIProcess{Name: tc.image, ExecutablePath: p.Tool.Path, CommandLine: tc.command}, p); got != tc.match {
				t.Fatalf("match=%v want=%v", got, tc.match)
			}
		})
	}
	p.Tool.Path = `C:\synthetic\renamed-agent.exe`
	p.Args = nil
	if !managedCLIProcessMatches(managedCLIProcess{Name: "renamed-agent.exe", ExecutablePath: p.Tool.Path, CommandLine: "synthetic"}, p) {
		t.Fatal("custom executable was omitted")
	}
}

func TestManagedCLIRealLaunchPlanUsesSameRegistryAsSharedSwitch(t *testing.T) {
	root := managedTestRoot(t)
	t.Setenv("CODEX_HOME", filepath.Join(root, "original-codex"))
	if err := os.MkdirAll(os.Getenv("CODEX_HOME"), 0700); err != nil {
		t.Fatal(err)
	}
	s := New(filepath.Join(root, "bridge"))
	tool := stubTool(t, "codex")
	s.FindTools = func(context.Context, map[string]string) []Tool { return []Tool{tool} }
	var isolated Plan
	s.Start = func(_ context.Context, p Plan) (int, error) { isolated = p; return 321, nil }
	a := account("codex", "synthetic-isolated", "synthetic-key")
	if _, err := s.LaunchAuthorized(context.Background(), a, "codex", root, nil, nil); err != nil {
		t.Fatal(err)
	}
	shared, err := s.PrepareSwitch(context.Background(), a, "codex", root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if isolated.Shared || !shared.Shared || isolated.RuntimeDir == "" || isolated.RuntimeDir != shared.RuntimeDir || isolated.ProfileDir == shared.ProfileDir {
		t.Fatalf("isolated receipt would be undiscoverable by shared safe stop: isolated=%q shared=%q", isolated.RuntimeDir, shared.RuntimeDir)
	}
}
