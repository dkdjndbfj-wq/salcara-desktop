package hubclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func projectDirectoryCommand(action, root, rel string) map[string]any {
	return map[string]any{"type": "workspace.projects", "controlSurface": "cli", "tool": "codex", "workspaceMode": "code", "action": action, "cwd": root, "path": rel}
}

func TestProjectDirectoryNormalNamesAndFilePolicyStaySeparate(t *testing.T) {
	c, _, root := workspaceFixture(t)
	for _, name := range []string{"config", "auth", "my-token-service"} {
		cmd := projectDirectoryCommand("mkdir", root, "")
		cmd["name"] = name
		result, err := c.Dispatch(context.Background(), cmd)
		if err != nil || result.(map[string]any)["project"].(protocol.Project).Path != filepath.Join(root, name) {
			t.Fatal(name, result, err)
		}
		if _, err = c.Dispatch(context.Background(), projectDirectoryCommand("select", root, name)); err != nil {
			t.Fatal(name, err)
		}
		if _, err = workspaceRelativePath(name + "/secret.txt"); err == nil {
			t.Fatal("file-content policy was widened", name)
		}
	}
	result, err := c.Dispatch(context.Background(), projectDirectoryCommand("list", root, ""))
	if err != nil || len(result.(map[string]any)["directory"].(workspaceFilesPage).Entries) != 3 {
		t.Fatal(result, err)
	}
	for _, name := range []string{".git", ".ssh", "CON", "LPT1", "../outside", "a/b", "trailing.", " control", "a\u200bb"} {
		cmd := projectDirectoryCommand("mkdir", root, "")
		cmd["name"] = name
		if _, err = c.Dispatch(context.Background(), cmd); err == nil {
			t.Fatal("unsafe project name accepted", name)
		}
	}
}

func TestProjectDirectoryPagesOnlyDirectories(t *testing.T) {
	c, _, root := workspaceFixture(t)
	for i := 0; i < 57; i++ {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("project-%02d", i)), 0700); err != nil {
			t.Fatal(err)
		}
		workspaceWrite(t, root, fmt.Sprintf("file-%02d.txt", i), []byte("private"))
	}
	cmd := projectDirectoryCommand("list", root, "")
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		result, err := c.Dispatch(context.Background(), cmd)
		if err != nil {
			t.Fatal(err)
		}
		page := result.(map[string]any)["directory"].(workspaceFilesPage)
		if i == 0 && len(page.Entries) != 50 {
			t.Fatal("file entries consumed the directory page", page)
		}
		for _, entry := range page.Entries {
			if entry.Kind != "directory" || seen[entry.Path] {
				t.Fatal(entry)
			}
			seen[entry.Path] = true
		}
		if page.NextCursor == "" {
			break
		}
		cmd["cursor"] = page.NextCursor
	}
	if len(seen) != 57 {
		t.Fatal("missing directories", len(seen))
	}
}

func TestProjectDirectoryRequiresCompleteSchema(t *testing.T) {
	c, _, root := workspaceFixture(t)
	for _, key := range []string{"tool", "workspaceMode", "controlSurface", "action", "cwd", "path"} {
		cmd := projectDirectoryCommand("list", root, "")
		delete(cmd, key)
		if _, err := c.Dispatch(context.Background(), cmd); err == nil {
			t.Fatal("missing field accepted", key)
		}
	}
}

func TestProjectDirectoryRevalidatesAuthorizationBeforeMutation(t *testing.T) {
	for _, change := range []string{"direct-revoked", "grant-removed", "directory-replaced"} {
		t.Run(change, func(t *testing.T) {
			c, _, root := workspaceFixture(t)
			directFixtureContext(t, c)
			parent := filepath.Join(root, "parent")
			if err := os.Mkdir(parent, 0700); err != nil {
				t.Fatal(err)
			}
			checks := 0
			ctx := WithDirectExecutionValidator(context.Background(), "project-lease", func() error {
				checks++
				if checks < 3 {
					return nil
				}
				switch change {
				case "direct-revoked":
					return errors.New("revoked")
				case "grant-removed":
					return c.o.Store.Update(func(cfg *config.Config) error { cfg.Projects = nil; return nil })
				case "directory-replaced":
					if err := os.Rename(parent, filepath.Join(root, "moved")); err != nil {
						if runtime.GOOS == "windows" && errors.Is(err, os.ErrPermission) {
							t.Skip("Windows prevents replacing this open directory handle")
						}
						t.Fatal(err)
					}
					if err := os.Mkdir(parent, 0700); err != nil {
						t.Fatal(err)
					}
				}
				return nil
			})
			cmd := projectDirectoryCommand("mkdir", root, "parent")
			cmd["name"] = "new"
			if result, err := c.Dispatch(ctx, cmd); err == nil || result != nil {
				t.Fatal("stale authorization accepted", result, err)
			}
			for _, dir := range []string{"parent", "moved"} {
				if _, err := os.Stat(filepath.Join(root, dir, "new")); !os.IsNotExist(err) {
					t.Fatal("mutation followed invalidation", err)
				}
			}
		})
	}
}

func TestProjectDirectoryOverHubCommandStream(t *testing.T) {
	hub, server := newFakeHub(t)
	root := t.TempDir()
	c, st := newTestClient(t, server.URL, "sk-good", nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("client did not stop")
		}
	})
	waitFor(t, "project command stream", func() bool { return c.Status().State == StateConnected })
	call := func(id string, cmd map[string]any) protocol.Reply {
		t.Helper()
		hub.commands <- protocol.CommandEnvelope{CommandID: id, Command: cmd}
		select {
		case reply := <-hub.replies:
			if reply.CommandID != id {
				t.Fatal(reply)
			}
			return reply
		case <-time.After(5 * time.Second):
			t.Fatal("project reply timeout")
			return protocol.Reply{}
		}
	}
	empty := call("roots-empty", projectDirectoryCommand("list", "", ""))
	if !empty.OK || len(empty.Result.(map[string]any)["roots"].([]any)) != 0 {
		t.Fatal(empty)
	}
	if err := st.Update(func(cfg *config.Config) error {
		cfg.Projects = []protocol.Project{{Path: root, Name: "allowed"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cmd := projectDirectoryCommand("mkdir", root, "")
	cmd["name"] = "config"
	created := call("mkdir", cmd)
	if !created.OK {
		t.Fatal(created)
	}
	cwd := created.Result.(map[string]any)["project"].(map[string]any)["path"].(string)
	if cwd != filepath.Join(root, "config") {
		t.Fatal(cwd)
	}
	started := call("start", map[string]any{"type": "session.start", "tool": "codex", "controlSurface": "cli", "workspaceMode": "code", "cwd": cwd, "prompt": "synthetic project flow"})
	if !started.OK || started.Result.(map[string]any)["sessionKey"] != "codex:s1" {
		t.Fatal(started)
	}
}

func TestProjectDirectoryFirstUseAndExactSelection(t *testing.T) {
	c, a, root := workspaceFixture(t)
	if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.Projects = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	result, err := c.Dispatch(context.Background(), projectDirectoryCommand("list", "", ""))
	if err != nil || len(result.(map[string]any)["roots"].([]protocol.Project)) != 0 {
		t.Fatal(result, err)
	}
	// Local settings authorizes one root. The phone cannot grant its own root.
	if _, err = c.Dispatch(context.Background(), projectDirectoryCommand("select", root, "")); err == nil {
		t.Fatal("ungranted root accepted")
	}
	if err := c.o.Store.Update(func(cfg *config.Config) error {
		cfg.Projects = []protocol.Project{{Path: root, Name: "allowed"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(root, "existing"), 0700); err != nil {
		t.Fatal(err)
	}
	workspaceWrite(t, root, "private.txt", []byte("not returned"))
	result, err = c.Dispatch(context.Background(), projectDirectoryCommand("list", root, ""))
	if err != nil {
		t.Fatal(err)
	}
	page := result.(map[string]any)["directory"].(workspaceFilesPage)
	if len(page.Entries) != 1 || page.Entries[0].Name != "existing" {
		t.Fatal(page)
	}
	result, err = c.Dispatch(context.Background(), projectDirectoryCommand("select", root, "existing"))
	if err != nil {
		t.Fatal(err)
	}
	selected := result.(map[string]any)["project"].(protocol.Project)
	if selected.Path != filepath.Join(root, "existing") {
		t.Fatal(selected)
	}
	if _, err = workspaceConfiguredRoot(c.o.Store.Get(), selected.Path); err != nil {
		t.Fatal("selected project was not saved", err)
	}
	if len(a.started)+len(a.opened)+len(a.sent) != 0 {
		t.Fatal("folder selection started a session")
	}
	if _, err = CheckCwd(selected.Path, c.o.Store.Get().Projects); err != nil {
		t.Fatal(err)
	}
}

func TestProjectDirectoryCreateEmptyAndConflict(t *testing.T) {
	c, _, root := workspaceFixture(t)
	cmd := projectDirectoryCommand("mkdir", root, "")
	cmd["name"] = "new-project"
	result, err := c.Dispatch(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	p := result.(map[string]any)["project"].(protocol.Project)
	if p.Path != filepath.Join(root, "new-project") {
		t.Fatal(p)
	}
	if entries, err := os.ReadDir(p.Path); err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
	if _, err = c.Dispatch(context.Background(), cmd); err == nil || !strings.Contains(err.Error(), "project_exists") {
		t.Fatal("existing directory was replaced", err)
	}
}

func TestProjectDirectoryRejectsInvalidAuthorityAndPaths(t *testing.T) {
	for _, variant := range []string{"unknown-root", "traversal", "absolute", "hidden", "separator", "unknown-tool", "surface", "mode", "extra", "symlink", "cancelled"} {
		t.Run(variant, func(t *testing.T) {
			c, _, root := workspaceFixture(t)
			cmd := projectDirectoryCommand("mkdir", root, "")
			cmd["name"] = "new"
			ctx := context.Background()
			switch variant {
			case "unknown-root":
				cmd["cwd"] = t.TempDir()
			case "traversal":
				cmd["path"] = "../outside"
			case "absolute":
				cmd["path"] = root
			case "hidden":
				cmd["name"] = ".ssh"
			case "separator":
				cmd["name"] = "a/b"
			case "unknown-tool":
				cmd["tool"] = "not-installed"
			case "surface":
				cmd["controlSurface"] = "desktop"
			case "mode":
				cmd["workspaceMode"] = "another"
			case "extra":
				cmd["accountId"] = "not-allowed"
			case "symlink":
				if err := os.Symlink(t.TempDir(), filepath.Join(root, "link")); err != nil {
					t.Skip(err)
				}
				cmd["path"] = "link"
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if got, err := c.Dispatch(ctx, cmd); err == nil || got != nil {
				t.Fatal("invalid request accepted", got, err)
			}
			if _, err := os.Stat(filepath.Join(root, "new")); !os.IsNotExist(err) {
				t.Fatal("mutation preceded validation", err)
			}
		})
	}
}
