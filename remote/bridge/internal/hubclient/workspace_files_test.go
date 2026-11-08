package hubclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

const workspaceTestKey = "codex:01a0ae56-e9f6-4933-9ad4-5e08cd7874e5"

type workspaceMetadataAgent struct {
	*fakeAgent
	metadata       protocol.SessionInfo
	metadataErr    error
	ids            []string
	directoryCalls int
}

func (a *workspaceMetadataAgent) SessionMetadata(_ context.Context, id string) (protocol.SessionInfo, error) {
	a.ids = append(a.ids, id)
	return a.metadata, a.metadataErr
}
func (a *workspaceMetadataAgent) Sessions(context.Context) ([]protocol.SessionInfo, error) {
	a.directoryCalls++
	return a.fakeAgent.sessions, nil
}

func TestWorkspaceFilesExactMetadataReadsOldSessionsWithoutHistoryOrDirectoryFallback(t *testing.T) {
	for _, condition := range []string{"ok", "unsupported", "wrong-key", "wrong-surface"} {
		t.Run(condition, func(t *testing.T) {
			c, base, root := workspaceFixture(t)
			workspaceWrite(t, root, "old.ts", []byte("synthetic old file"))
			a := &workspaceMetadataAgent{fakeAgent: base, metadata: protocol.SessionInfo{SessionKey: workspaceTestKey, Cwd: root, ControlSurface: "cli"}}
			if condition == "unsupported" {
				a.metadataErr = errors.New("unsupported")
			}
			if condition == "wrong-key" {
				a.metadata.SessionKey = "codex:other"
			}
			if condition == "wrong-surface" {
				a.metadata.ControlSurface = "desktop"
			}
			// A matching legacy record must not rescue a failed exact lookup.
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			result, err := c.Dispatch(context.Background(), workspaceCommand("read", "old.ts", root))
			if condition == "ok" {
				if err != nil || result.(workspaceFileText).Text != "synthetic old file" || len(a.ids) != 2 {
					t.Fatalf("exact read failed: %v", err)
				}
			} else if err == nil || result != nil {
				t.Fatal("invalid exact metadata accepted")
			}
			for _, id := range a.ids {
				if id != strings.TrimPrefix(workspaceTestKey, "codex:") {
					t.Fatal("metadata identity was not exact")
				}
			}
			if a.directoryCalls != 0 || len(base.opened)+len(base.started)+len(base.sent) != 0 {
				t.Fatal("metadata lookup fell back to directory/history/executor")
			}
		})
	}
}

func workspaceFixture(t *testing.T) (*Client, *fakeAgent, string) {
	t.Helper()
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("final handle implementation intentionally unavailable")
	}
	c, a := surfaceClient(t)
	root := t.TempDir()
	if err := c.o.Store.Update(func(cfg *config.Config) error {
		cfg.Projects = []protocol.Project{{Path: root, Name: "synthetic"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a.sessions = []protocol.SessionInfo{{SessionKey: workspaceTestKey, Cwd: root, ControlSurface: "cli"}}
	return c, a, root
}

func workspaceCommand(typ, rel, expectedCwd string) map[string]any {
	return map[string]any{"type": "workspace.files." + typ, "sessionKey": workspaceTestKey, "controlSurface": "cli", "path": rel, "expectedCwd": expectedCwd}
}

func workspaceWrite(t *testing.T, root, name string, data []byte) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceFilesReadOnlyAllowlistedTextAndList(t *testing.T) {
	c, a, root := workspaceFixture(t)
	for _, name := range []string{"README.md", "src/app.ts", "auth.json", "config.json", ".env", ".git/HEAD", "node_modules/a.js", "photo.png", "archive.zip", "secrets.ts"} {
		workspaceWrite(t, root, name, []byte("synthetic\n"))
	}
	result, err := c.Dispatch(context.Background(), workspaceCommand("list", "", root))
	if err != nil {
		t.Fatal(err)
	}
	page := result.(workspaceFilesPage)
	if page.Path != "" || len(page.Entries) != 2 {
		t.Fatalf("unexpected entries: %+v", page)
	}
	names := map[string]bool{}
	for _, e := range page.Entries {
		names[e.Name] = true
		if filepath.IsAbs(e.Path) {
			t.Fatal("absolute path exposed")
		}
	}
	if !names["README.md"] || !names["src"] {
		t.Fatal("allowlisted files missing")
	}
	workspaceWrite(t, root, "src/app.ts", []byte("你好\nconst n = 1;\n"))
	result, err = c.Dispatch(context.Background(), workspaceCommand("read", "src/app.ts", root))
	if err != nil {
		t.Fatal(err)
	}
	text := result.(workspaceFileText)
	if text.Path != "src/app.ts" || text.Text != "你好\nconst n = 1;\n" || text.Size != int64(len(text.Text)) {
		t.Fatalf("text=%+v", text)
	}
	if len(a.opened)+len(a.started)+len(a.sent)+len(a.stopped) != 0 {
		t.Fatal("read-only file operation executed/opened a turn")
	}
}

func TestWorkspaceFilesRejectUnsafePathAndPayload(t *testing.T) {
	c, _, root := workspaceFixture(t)
	workspaceWrite(t, root, "ok.ts", []byte("ok"))
	for _, rel := range []string{"../ok.ts", "/ok.ts", "src/../ok.ts", `src\ok.ts`, "C:/ok.ts", "ok.ts:secret", "ok.ts ", "src//ok.ts", ".env", ".codex/auth.json", ".ssh/id_rsa", "auth.json", "config.json", "config.production.json", "auth_saved.json", "appsettings.json", "env.production.json", "service-account.json", "key.json", "token.ts", "credentials.json", "image.png", "file.pdf", "src/\x01bad.ts", "src/\u202ebad.ts", "src/\u0085bad.ts", "NUL.ts", "COM1.any.ts", "AUX.ts"} {
		if _, err := c.Dispatch(context.Background(), workspaceCommand("read", rel, root)); err == nil {
			t.Fatalf("accepted %q", rel)
		}
	}
	for _, mutate := range []func(map[string]any){
		func(cmd map[string]any) { cmd["root"] = root },
		func(cmd map[string]any) { cmd["cwd"] = root },
		func(cmd map[string]any) { cmd["accountId"] = "handle" },
		func(cmd map[string]any) { cmd["controlSurface"] = "read-only" },
		func(cmd map[string]any) { delete(cmd, "controlSurface") },
		func(cmd map[string]any) { cmd["path"] = nil },
		func(cmd map[string]any) { cmd["cursor"] = "x" },
		func(cmd map[string]any) { cmd["sessionKey"] = "codex:missing" },
	} {
		cmd := workspaceCommand("read", "ok.ts", root)
		mutate(cmd)
		if _, err := c.Dispatch(context.Background(), cmd); err == nil {
			t.Fatalf("accepted %v", cmd)
		} else if strings.Contains(err.Error(), root) {
			t.Fatal("filesystem path reflected")
		}
	}
}

func TestWorkspaceFilesRequireExactConfiguredSessionProject(t *testing.T) {
	c, a, root := workspaceFixture(t)
	workspaceWrite(t, root, "sub/app.ts", []byte("synthetic"))
	for _, mode := range []string{"removed", "subfolder", "foreign-surface"} {
		t.Run(mode, func(t *testing.T) {
			if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.Projects = []protocol.Project{{Path: root}}; return nil }); err != nil {
				t.Fatal(err)
			}
			a.sessions[0].Cwd, a.sessions[0].ControlSurface = root, "cli"
			switch mode {
			case "removed":
				if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.Projects = nil; return nil }); err != nil {
					t.Fatal(err)
				}
			case "subfolder":
				a.sessions[0].Cwd = filepath.Join(root, "sub")
			case "foreign-surface":
				a.sessions[0].ControlSurface = "desktop"
			}
			if _, err := c.Dispatch(context.Background(), workspaceCommand("list", "", root)); err == nil {
				t.Fatal("unconfigured/inexact project accepted")
			}
		})
	}
}

func TestWorkspaceFilesTextBoundsAndBinaryDenial(t *testing.T) {
	c, _, root := workspaceFixture(t)
	for name, data := range map[string][]byte{"large.ts": []byte(strings.Repeat("a", workspaceFileBytes+1)), "nul.ts": {'a', 0, 'b'}, "bad.ts": {0xff, 0xfe}, "escape.ts": {'a', 27, 'b'}} {
		workspaceWrite(t, root, name, data)
		if _, err := c.Dispatch(context.Background(), workspaceCommand("read", name, root)); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
	workspaceWrite(t, root, "limit.ts", []byte(strings.Repeat("a", workspaceFileBytes)))
	if _, err := c.Dispatch(context.Background(), workspaceCommand("read", "limit.ts", root)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Dispatch(context.Background(), workspaceCommand("read", "", root)); err == nil {
		t.Fatal("root read accepted")
	}
	if _, err := c.Dispatch(context.Background(), workspaceCommand("list", "limit.ts", root)); err == nil {
		t.Fatal("file listed as directory")
	}
}

func TestWorkspaceFilesPagedDirectoryAndBoundCursor(t *testing.T) {
	c, _, root := workspaceFixture(t)
	for i := 0; i < 125; i++ {
		workspaceWrite(t, root, fmt.Sprintf("f%03d.ts", i), []byte("x"))
	}
	seen := map[string]bool{}
	cursor, original := "", ""
	for _, want := range []int{50, 50, 25} {
		cmd := workspaceCommand("list", "", root)
		if cursor != "" {
			cmd["cursor"] = cursor
		}
		result, err := c.Dispatch(context.Background(), cmd)
		if err != nil {
			t.Fatal(err)
		}
		page := result.(workspaceFilesPage)
		if len(page.Entries) != want {
			t.Fatalf("entries=%d wanted %d", len(page.Entries), want)
		}
		for _, e := range page.Entries {
			if seen[e.Path] {
				t.Fatal("duplicate page entry")
			}
			seen[e.Path] = true
		}
		cursor = page.NextCursor
		if original == "" {
			original = cursor
		}
	}
	if cursor != "" || len(seen) != 125 {
		t.Fatal("incorrect directory exhaustion")
	}
	now := time.Now().Add(time.Hour)
	if err := os.Chtimes(root, now, now); err != nil {
		t.Fatal(err)
	}
	cmd := workspaceCommand("list", "", root)
	cmd["cursor"] = original
	if _, err := c.Dispatch(context.Background(), cmd); err == nil {
		t.Fatal("stale directory cursor accepted")
	}
	for _, bad := range []string{"bad", strings.Repeat("A", 1000), "e30"} {
		cmd["cursor"] = bad
		if _, err := c.Dispatch(context.Background(), cmd); err == nil {
			t.Fatal("malformed cursor accepted")
		}
	}
}

func TestWorkspaceFilesCursorRequiresExactFields(t *testing.T) {
	c, _, root := workspaceFixture(t)
	workspaceWrite(t, root, "one.ts", []byte("one"))
	workspaceWrite(t, root, "two.ts", []byte("two"))
	revision := strings.Repeat("a", 64)
	valid := fmt.Sprintf(`{"v":1,"o":1,"r":%q}`, revision)
	if _, err := workspaceDecodeCursor(base64.RawURLEncoding.EncodeToString([]byte(valid)), revision); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"o":1}`, fmt.Sprintf(`{"o":1,"r":%q}`, revision), `{"v":1,"o":1}`,
		fmt.Sprintf(`{"v":1,"v":1,"o":1,"r":%q}`, revision),
		fmt.Sprintf(`{"v":1,"o":1,"r":%q,"unknown":1}`, revision),
		fmt.Sprintf(`{"v":null,"o":1,"r":%q}`, revision),
		fmt.Sprintf(`{"v":1,"o":null,"r":%q}`, revision),
		`{"v":1,"o":1,"r":null}`, valid + `{}`, `[]`,
	} {
		encoded := base64.RawURLEncoding.EncodeToString([]byte(raw))
		if _, err := workspaceDecodeCursor(encoded, revision); err == nil {
			t.Fatalf("partial/ambiguous cursor accepted: %s", raw)
		}
		cmd := workspaceCommand("list", "", root)
		cmd["cursor"] = encoded
		if result, err := c.Dispatch(context.Background(), cmd); err == nil || result != nil {
			t.Fatalf("malformed cursor reached listing: %s", raw)
		}
	}
}

func TestWorkspaceFilesIgnoredNamesStillConsumeBoundedScan(t *testing.T) {
	c, _, root := workspaceFixture(t)
	for i := 0; i <= workspaceListScan; i++ {
		workspaceWrite(t, root, fmt.Sprintf(".private%04d", i), []byte("x"))
	}
	result, err := c.Dispatch(context.Background(), workspaceCommand("list", "", root))
	if err != nil {
		t.Fatal(err)
	}
	page := result.(workspaceFilesPage)
	if len(page.Entries) != 0 || !page.Truncated || page.NextCursor != "" {
		t.Fatalf("scan bound result=%+v", page)
	}
}

func TestWorkspaceFilesHardlinkAndSymlinkAliasesDenied(t *testing.T) {
	c, _, root := workspaceFixture(t)
	workspaceWrite(t, root, "auth.json", []byte("synthetic secret"))
	if err := os.Link(filepath.Join(root, "auth.json"), filepath.Join(root, "alias.ts")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Dispatch(context.Background(), workspaceCommand("read", "alias.ts", root)); err == nil {
		t.Fatal("hardlink secret alias accepted")
	}
	outside := t.TempDir()
	workspaceWrite(t, outside, "outside.ts", []byte("synthetic outside"))
	for name, target := range map[string]string{"inside.ts": "auth.json", "outside.ts": filepath.Join(outside, "outside.ts"), "junction": outside} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Logf("symlink creation unavailable (%s); hardlink denial verified", name)
			continue
		}
		rel := name
		if name == "junction" {
			rel += "/outside.ts"
		}
		if _, err := c.Dispatch(context.Background(), workspaceCommand("read", rel, root)); err == nil {
			t.Fatalf("symlink alias accepted %s", name)
		}
	}
}

func TestWorkspaceFilesFinalHandleRejectsRenamedFile(t *testing.T) {
	_, _, root := workspaceFixture(t)
	workspaceWrite(t, root, "app.ts", []byte("synthetic"))
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	rh, err := r.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer rh.Close()
	f, err := workspaceOpen(r, rh, root, "app.ts", true)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Rename(filepath.Join(root, "app.ts"), filepath.Join(root, ".env")); err != nil {
		t.Fatal(err)
	}
	if workspaceVerifyHandle(rh, f, root, "app.ts", true) == nil {
		t.Fatal("renamed secret handle accepted")
	}
}

type workspaceChangingAgent struct {
	*fakeAgent
	calls  int
	change func()
}

func (a *workspaceChangingAgent) Sessions(ctx context.Context) ([]protocol.SessionInfo, error) {
	a.calls++
	if a.calls == 2 {
		a.change()
	}
	return a.fakeAgent.Sessions(ctx)
}

func TestWorkspaceFilesDiscardSnapshotOnBindingOrProjectRemoval(t *testing.T) {
	for _, mode := range []string{"binding", "project"} {
		t.Run(mode, func(t *testing.T) {
			c, base, root := workspaceFixture(t)
			workspaceWrite(t, root, "app.ts", []byte("private synthetic"))
			a := &workspaceChangingAgent{fakeAgent: base, change: func() {
				if err := c.o.Store.Update(func(cfg *config.Config) error {
					if mode == "binding" {
						cfg.PhoneHash = "changed"
					} else {
						cfg.Projects = nil
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}}
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			if result, err := c.Dispatch(context.Background(), workspaceCommand("read", "app.ts", root)); err == nil || result != nil {
				t.Fatal("private late snapshot released")
			}
		})
	}
}

func TestWorkspaceFilesDiscardSnapshotOnLateRootReplacement(t *testing.T) {
	for _, operation := range []string{"read", "list"} {
		t.Run(operation, func(t *testing.T) {
			c, base, root := workspaceFixture(t)
			workspaceWrite(t, root, "app.ts", []byte("old private snapshot"))
			retired := filepath.Join(t.TempDir(), "retired-root")
			a := &workspaceChangingAgent{fakeAgent: base, change: func() {
				if err := os.Rename(root, retired); err != nil {
					if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(32)) {
						t.Skip("Windows anchored directory handle prevents root rename (sharing violation)")
					}
					t.Fatal(err)
				}
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				workspaceWrite(t, root, "app.ts", []byte("new project generation"))
			}}
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			rel := ""
			if operation == "read" {
				rel = "app.ts"
			}
			if result, err := c.Dispatch(context.Background(), workspaceCommand(operation, rel, root)); err == nil || result != nil {
				t.Fatal("old root snapshot released after same-path replacement")
			}
		})
	}
}

func TestWorkspaceFilesNativeLeaseScopeNeverFallsBackToCLI(t *testing.T) {
	c, cli, root := workspaceFixture(t)
	workspaceWrite(t, root, "app.ts", []byte("native synthetic"))
	d := &pagedRouteDesktop{routeDesktop: routeDesktop{st: liveDesktop(workspaceTestKey)}, list: []protocol.SessionInfo{{SessionKey: workspaceTestKey, Cwd: root, ControlSurface: "desktop", Controllable: true, ControlExpiresAt: time.Now().Add(time.Hour).UnixMilli()}}}
	c.o.Desktop = d
	cmd := workspaceCommand("read", "app.ts", root)
	cmd["controlSurface"] = "desktop"
	if _, err := c.Dispatch(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	if d.statusChecks != 2 {
		t.Fatal("native lease not rechecked")
	}
	for _, mode := range []string{"no-read", "expired", "wrong-scope", "no-desktop", "unknown-cwd"} {
		d.st = liveDesktop(workspaceTestKey)
		c.o.Desktop = d
		d.list[0].Cwd = root
		switch mode {
		case "no-read":
			d.st.Capabilities.Read = false
		case "expired":
			d.st.ExpiresAt = nowMS() - 1
		case "wrong-scope":
			d.st.SessionKeys = []string{"codex:11111111-2222-4333-8444-555555555555"}
		case "no-desktop":
			c.o.Desktop = nil
		case "unknown-cwd":
			d.list[0].Cwd = ""
		}
		if _, err := c.Dispatch(context.Background(), cmd); err == nil {
			t.Fatalf("native scope accepted %s", mode)
		}
	}
	if len(cli.opened)+len(cli.started)+len(cli.sent) != 0 {
		t.Fatal("native fallback executed CLI")
	}
}

func TestWorkspaceFilesBroadRootAndCapability(t *testing.T) {
	if !workspaceBroadRoot(filepath.VolumeName(t.TempDir()) + string(filepath.Separator)) {
		t.Fatal("drive root allowed")
	}
	if home, err := os.UserHomeDir(); err == nil && !workspaceBroadRoot(home) {
		t.Fatal("home root allowed")
	}
	c, _, root := workspaceFixture(t)
	raw, err := json.Marshal(c.Device(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "workspace.files.v1") {
		t.Fatal("capability missing")
	}
	if !c.workspaceFilesMu.TryLock() {
		t.Fatal("unexpected file operation lock")
	}
	if _, err := c.Dispatch(context.Background(), workspaceCommand("list", "", root)); err == nil {
		t.Fatal("unbounded parallel file operation")
	}
	c.workspaceFilesMu.Unlock()
}

func TestWorkspaceFilesRejectNetworkNamespaceBeforeFilesystemAccess(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows namespace/drive check")
	}
	for _, root := range []string{`\\synthetic.invalid\share\project`, `\\?\C:\synthetic`, `\\.\C:\synthetic`} {
		if workspaceLocalRoot(root) {
			t.Fatalf("network/device namespace allowed: %s", root)
		}
		if _, err := workspaceConfiguredRoot(config.Config{Projects: []protocol.Project{{Path: root}}}, root); err == nil {
			t.Fatal("namespace reached workspace")
		}
	}
}

func TestWorkspaceFilesExpectedCwdIsMandatoryBoundedAssertion(t *testing.T) {
	c, _, root := workspaceFixture(t)
	workspaceWrite(t, root, "app.ts", []byte("synthetic"))
	for _, value := range []any{nil, true, 1, "", strings.Repeat("a", 4097), "relative/project", root + "\x00", root + "\u202e"} {
		cmd := workspaceCommand("read", "app.ts", root)
		cmd["expectedCwd"] = value
		if result, err := c.Dispatch(context.Background(), cmd); err == nil || result != nil {
			t.Fatalf("invalid cwd assertion accepted: %T", value)
		}
	}
	cmd := workspaceCommand("read", "app.ts", root)
	delete(cmd, "expectedCwd")
	if result, err := c.Dispatch(context.Background(), cmd); err == nil || result != nil {
		t.Fatal("missing cwd assertion accepted")
	}
	// The phone's arbitrary network path is compared as a string only. It
	// cannot select the root or turn into an SMB stat/EvalSymlinks operation.
	cmd["expectedCwd"] = `\\synthetic.invalid\share\project`
	if result, err := c.Dispatch(context.Background(), cmd); !errors.Is(err, errWorkspaceChanged) || result != nil {
		t.Fatalf("foreign assertion touched root selection: %v", err)
	}
}

func TestWorkspaceFilesExpectedCwdRejectsRootMoveBeforeAdmission(t *testing.T) {
	for _, surface := range []string{"cli", "desktop"} {
		t.Run(surface, func(t *testing.T) {
			c, a, oldRoot := workspaceFixture(t)
			newRoot := t.TempDir()
			workspaceWrite(t, oldRoot, "app.ts", []byte("project A"))
			workspaceWrite(t, newRoot, "app.ts", []byte("project B"))
			if err := c.o.Store.Update(func(cfg *config.Config) error {
				cfg.Projects = append(cfg.Projects, protocol.Project{Path: newRoot})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			a.sessions[0].Cwd = newRoot
			if surface == "desktop" {
				c.o.Desktop = &pagedRouteDesktop{routeDesktop: routeDesktop{st: liveDesktop(workspaceTestKey)}, list: []protocol.SessionInfo{{SessionKey: workspaceTestKey, Cwd: newRoot, ControlSurface: "desktop", Controllable: true, ControlExpiresAt: time.Now().Add(time.Hour).UnixMilli()}}}
			}
			for _, operation := range []string{"list", "read"} {
				rel := ""
				if operation == "read" {
					rel = "app.ts"
				}
				cmd := workspaceCommand(operation, rel, oldRoot)
				cmd["controlSurface"] = surface
				if result, err := c.Dispatch(context.Background(), cmd); !errors.Is(err, errWorkspaceChanged) || result != nil {
					t.Fatalf("stale A assertion read project B: %v", err)
				}
				cmd["expectedCwd"] = newRoot
				if _, err := c.Dispatch(context.Background(), cmd); err != nil {
					t.Fatalf("fresh B assertion rejected: %v", err)
				}
			}
		})
	}
}

func TestWorkspaceFilesExpectedCwdRejectsBeforeProjectFilesystemChecks(t *testing.T) {
	c, a, root := workspaceFixture(t)
	// Verified metadata now names a missing directory. A stale phone assertion
	// must fail as changed before checking whether that host directory exists.
	a.sessions[0].Cwd = filepath.Join(root, "does-not-exist")
	if err := c.o.Store.Update(func(cfg *config.Config) error {
		cfg.Projects = []protocol.Project{{Path: a.sessions[0].Cwd}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if result, err := c.Dispatch(context.Background(), workspaceCommand("list", "", root)); !errors.Is(err, errWorkspaceChanged) || result != nil {
		t.Fatalf("project filesystem checks preceded expectedcwd: %v", err)
	}
}

func TestWorkspaceFilesExpectedCwdComparisonDoesNotFoldDirectoryCase(t *testing.T) {
	if runtime.GOOS == "windows" {
		if !workspaceExpectedCwdMatches(`c:/Project/src/`, `C:\Project\src`) {
			t.Fatal("drive letter or separator normalization rejected")
		}
		if workspaceExpectedCwdMatches(`C:\project\src`, `C:\Project\src`) {
			t.Fatal("case-sensitive project was guessed equal")
		}
	} else {
		if !workspaceExpectedCwdMatches("/home/project/src/", "/home/project/src") {
			t.Fatal("path cleanup rejected")
		}
		if workspaceExpectedCwdMatches("/home/Project/src", "/home/project/src") {
			t.Fatal("directory case folded")
		}
	}
}
