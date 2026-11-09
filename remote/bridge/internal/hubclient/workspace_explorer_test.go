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

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func explorerCommand(action, root, rel string) map[string]any {
	return map[string]any{"type": "workspace.explorer", "controlSurface": "cli", "tool": "codex", "workspaceMode": "code", "action": action, "cwd": root, "path": rel}
}

func explorerTestRoot(value string) (string, string) {
	root := filepath.VolumeName(value) + string(filepath.Separator)
	rel, _ := filepath.Rel(root, value)
	return root, filepath.ToSlash(rel)
}

func TestExplorerListsRealRootsAndHiddenNamesWithoutGrantingThem(t *testing.T) {
	c, a, parent := workspaceFixture(t)
	if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.Projects = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	root, rel := explorerTestRoot(parent)
	result, err := c.Dispatch(context.Background(), explorerCommand("list", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range result.(map[string]any)["roots"].([]protocol.Project) {
		if workspaceExpectedCwdMatches(candidate.Path, root) {
			found = true
		}
	}
	if !found {
		t.Fatal("real local root missing", root, result)
	}
	for _, name := range []string{".hidden", "config", "auth", "system", "my-token-service"} {
		if err = os.Mkdir(filepath.Join(parent, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	workspaceWrite(t, parent, "credentials.txt", []byte("never read"))
	result, err = c.Dispatch(context.Background(), explorerCommand("list", root, rel))
	if err != nil {
		t.Fatal(err)
	}
	page := result.(map[string]any)["directory"].(explorerPage)
	if len(page.Entries) != 5 {
		t.Fatal("hidden/system names lost or a file exposed", page)
	}
	for _, entry := range page.Entries {
		if entry.Kind != "directory" || entry.Name == "credentials.txt" {
			t.Fatal(entry)
		}
	}
	if len(c.o.Store.Get().Projects) != 0 || len(c.o.Store.Get().ExplorerGrants) != 0 || len(a.started)+len(a.opened)+len(a.sent) != 0 {
		t.Fatal("browsing granted or started something")
	}
	if _, err = CheckCwd(root, c.o.Store.Get().Projects); err == nil {
		t.Fatal("disk browsing authorized the disk")
	}
	old, err := c.Dispatch(context.Background(), projectDirectoryCommand("list", "", ""))
	if err != nil || len(old.(map[string]any)["roots"].([]protocol.Project)) != 0 {
		t.Fatal("legacy authorized-root picker was widened", old, err)
	}
}

func TestExplorerCreatesExactUsablePersistentProjectUnderRestrictedParent(t *testing.T) {
	c, a, ctx := projectlessFixture(t)
	parent := filepath.Join(t.TempDir(), ".hidden-system")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ProgramData", parent) // same broad-root rule on Windows/Linux.
	root, rel := explorerTestRoot(parent)
	cmd := explorerCommand("mkdir", root, rel)
	cmd["tool"], cmd["name"] = "hermes", "created"
	result, err := c.Dispatch(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	project := result.(map[string]any)["project"].(protocol.Project)
	if project.Path != filepath.Join(parent, "created") || !workspaceBroadRoot(project.Path) || len(a.started) != 0 {
		t.Fatal("wrong project or implicit start", project)
	}
	if _, err = workspaceConfiguredRoot(c.o.Store.Get(), project.Path); err != nil {
		t.Fatal("success returned an unusable project", err)
	}
	for _, denied := range []string{root, parent, filepath.Join(parent, "sibling")} {
		if _, err = workspaceConfiguredRoot(c.o.Store.Get(), denied); err == nil {
			t.Fatal("grant widened", denied)
		}
	}
	workspaceWrite(t, project.Path, "note.md", []byte("persistent project"))
	if retry, e := c.Dispatch(ctx, cmd); !errors.Is(e, errExplorerExists) || retry != nil {
		t.Fatal("mkdir replay adopted/overwrote existing content", retry, e)
	}
	start := projectlessCommand()
	delete(start, "projectless")
	start["cwd"] = project.Path
	started, err := c.Dispatch(ctx, start)
	if err != nil {
		t.Fatal("created project cannot start", err)
	}
	key := started.(map[string]any)["sessionKey"].(string)
	store, err := config.Open(c.o.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	restarted := New(Options{Store: store})
	restarted.SetManager(&fakeManager{list: []agents.Agent{a}})
	read := map[string]any{"type": "workspace.files.read", "controlSurface": "cli", "sessionKey": key, "expectedCwd": project.Path, "path": "note.md"}
	got, err := restarted.Dispatch(ctx, read)
	if err != nil || got.(workspaceFileText).Text != "persistent project" {
		t.Fatal("grant did not survive restart", got, err)
	}
	write := map[string]any{"type": "workspace.files.write", "controlSurface": "cli", "sessionKey": key, "expectedCwd": project.Path, "path": "note.md", "text": "saved after restart", "expectedRevision": got.(workspaceFileText).Revision}
	if _, err = restarted.Dispatch(ctx, write); err != nil {
		t.Fatal("created project cannot save files", err)
	}
	if content, err := os.ReadFile(filepath.Join(project.Path, "note.md")); err != nil || string(content) != "saved after restart" {
		t.Fatal(string(content), err)
	}
	saved := store.Get()
	if err = store.Update(func(cfg *config.Config) error { cfg.Projects = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	send := map[string]any{"type": "session.send", "controlSurface": "cli", "sessionKey": key, "text": "must not execute"}
	for _, command := range []map[string]any{read, write, start, send} {
		if result, err = restarted.Dispatch(ctx, command); err == nil || result != nil {
			t.Fatal("revoked project fell back to native history", command["type"], result, err)
		}
	}
	if err = store.Update(func(cfg *config.Config) error { cfg.Projects = saved.Projects; return nil }); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(project.Path, project.Path+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(project.Path, 0700); err != nil {
		t.Fatal(err)
	}
	workspaceWrite(t, project.Path, "note.md", []byte("replacement"))
	for _, command := range []map[string]any{read, write, start, send} {
		if result, err = restarted.Dispatch(ctx, command); err == nil || result != nil {
			t.Fatal("replaced project identity accepted", command["type"], result, err)
		}
	}
	if len(a.started) != 1 || len(a.sent) != 0 {
		t.Fatal("stale grant dispatched work")
	}
}

func TestExplorerStrictSchemaAndNetworkPaths(t *testing.T) {
	c, _, parent := workspaceFixture(t)
	root, rel := explorerTestRoot(parent)
	for _, field := range []string{"tool", "workspaceMode", "controlSurface", "action", "cwd", "path"} {
		cmd := explorerCommand("list", root, rel)
		delete(cmd, field)
		if _, err := c.Dispatch(context.Background(), cmd); err == nil {
			t.Fatal("missing field", field)
		}
	}
	for field, value := range map[string]any{"action": "select", "controlSurface": "desktop", "workspaceMode": "unknown", "name": "extra", "cursor": 4, "extra": "unexpected", "projectless": true} {
		cmd := explorerCommand("list", root, rel)
		cmd[field] = value
		if _, err := c.Dispatch(context.Background(), cmd); err == nil {
			t.Fatal("invalid schema accepted", field, value)
		}
	}
	rootCommand := explorerCommand("list", "", "")
	rootCommand["cursor"] = ""
	if _, err := c.Dispatch(context.Background(), rootCommand); err == nil {
		t.Fatal("root enumeration accepted a cursor")
	}
	for _, bad := range []string{"../outside", "a/../b", "/absolute", `a\b`, "a//b", "CON", "LPT1", "trailing.", " control", "a\u200bb"} {
		cmd := explorerCommand("mkdir", root, rel)
		cmd["name"] = bad
		if _, err := c.Dispatch(context.Background(), cmd); err == nil {
			t.Fatal("invalid name accepted", bad)
		}
	}
	for _, bad := range []string{parent, `\\server\share\`, `\\?\C:\`, `\\.\C:\`, "C:", "//server/share/"} {
		if _, err := c.Dispatch(context.Background(), explorerCommand("list", bad, "")); err == nil {
			t.Fatal("non-root/network/device path accepted", bad)
		}
	}
}

func TestExplorerPagingBindsOwnerModeAndDirectory(t *testing.T) {
	c, _, parent := workspaceFixture(t)
	root, rel := explorerTestRoot(parent)
	for i := 0; i < 57; i++ {
		if err := os.Mkdir(filepath.Join(parent, fmt.Sprintf(".folder-%02d", i)), 0700); err != nil {
			t.Fatal(err)
		}
		workspaceWrite(t, parent, fmt.Sprintf("file-%02d.txt", i), []byte("not listed"))
	}
	cmd := explorerCommand("list", root, rel)
	result, err := c.Dispatch(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	first := result.(map[string]any)["directory"].(explorerPage)
	if len(first.Entries) != 50 || first.NextCursor == "" {
		t.Fatal(first)
	}
	cmd["cursor"] = first.NextCursor
	result, err = c.Dispatch(context.Background(), cmd)
	if err != nil || len(result.(map[string]any)["directory"].(explorerPage).Entries) != 7 {
		t.Fatal(result, err)
	}
	cmd["workspaceMode"] = "work"
	if _, err = c.Dispatch(context.Background(), cmd); err == nil {
		t.Fatal("cursor crossed Code/Work")
	}
	cmd["workspaceMode"] = "code"
	if err = c.o.Store.Update(func(cfg *config.Config) error { cfg.DeviceID += "-changed"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Dispatch(context.Background(), cmd); err == nil {
		t.Fatal("cursor crossed owner")
	}
}

func TestExplorerRevocationCancellationAndLateReplacement(t *testing.T) {
	for _, change := range []string{"revoked-before", "revoked-after", "cancelled", "replaced"} {
		t.Run(change, func(t *testing.T) {
			c, _, parent := workspaceFixture(t)
			directFixtureContext(t, c)
			root, rel := explorerTestRoot(parent)
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			checks := 0
			ctx := WithDirectExecutionValidator(base, "explorer-lease", func() error {
				checks++
				if checks < 4 {
					return nil
				}
				switch change {
				case "revoked-before":
					return errors.New("revoked")
				case "revoked-after":
					if checks >= 5 {
						return errors.New("revoked")
					}
				case "cancelled":
					cancel()
				case "replaced":
					if checks == 4 {
						if err := os.Rename(parent, parent+"-moved"); err != nil {
							if runtime.GOOS == "windows" && errors.Is(err, os.ErrPermission) {
								t.Skip("Windows prevents replacing this pinned directory")
							}
							t.Fatal(err)
						}
						if err := os.Mkdir(parent, 0700); err != nil {
							t.Fatal(err)
						}
					}
				}
				return nil
			})
			cmd := explorerCommand("mkdir", root, rel)
			cmd["name"] = "created"
			if result, err := c.Dispatch(ctx, cmd); err == nil || result != nil {
				t.Fatal("changed authority or directory accepted", result, err)
			}
			if len(c.o.Store.Get().ExplorerGrants) != 0 {
				t.Fatal("unconfirmed creation was granted")
			}
			if change == "revoked-after" {
				if _, err := os.Stat(filepath.Join(parent, "created")); err != nil {
					t.Fatal("after-create boundary was not exercised", err)
				}
			} else {
				for _, current := range []string{parent, parent + "-moved"} {
					if _, err := os.Stat(filepath.Join(current, "created")); !os.IsNotExist(err) {
						t.Fatal("mutation after invalidation", err)
					}
				}
			}
		})
	}
}

func TestExplorerLinksVisibleButNeverFollowed(t *testing.T) {
	c, _, parent := workspaceFixture(t)
	out := t.TempDir()
	link := filepath.Join(parent, ".linked-folder")
	if err := os.Symlink(out, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("Windows test token cannot create symbolic links")
		}
		t.Fatal(err)
	}
	root, rel := explorerTestRoot(parent)
	result, err := c.Dispatch(context.Background(), explorerCommand("list", root, rel))
	if err != nil {
		t.Fatal(err)
	}
	page := result.(map[string]any)["directory"].(explorerPage)
	if len(page.Entries) != 1 || page.Entries[0].BlockedReason == "" {
		t.Fatal("link hidden or offered as traversable", page)
	}
	if _, err = c.Dispatch(context.Background(), explorerCommand("list", root, rel+"/.linked-folder")); !errors.Is(err, errExplorerLink) {
		t.Fatal("link traversed", err)
	}
}

func TestExplorerPermissionDeniedDoesNotCreateOrGrant(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() == 0 {
		t.Skip("run as an unprivileged Linux user to exercise OS permission denial")
	}
	c, _, parent := workspaceFixture(t)
	if err := os.Chmod(parent, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0700) })
	root, rel := explorerTestRoot(parent)
	cmd := explorerCommand("mkdir", root, rel)
	cmd["name"] = "denied"
	result, err := c.Dispatch(context.Background(), cmd)
	if !errors.Is(err, errExplorerPermission) || result != nil || len(c.o.Store.Get().ExplorerGrants) != 0 {
		t.Fatal(result, err)
	}
	if _, err = os.Stat(filepath.Join(parent, "denied")); !os.IsNotExist(err) {
		t.Fatal("permission error created a directory", err)
	}
}

func TestExplorerOverHubCommandStream(t *testing.T) {
	hub, server := newFakeHub(t)
	parent := t.TempDir()
	root, rel := explorerTestRoot(parent)
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
	waitFor(t, "explorer stream", func() bool { return c.Status().State == StateConnected })
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
			t.Fatal("explorer timeout")
			return protocol.Reply{}
		}
	}
	if reply := call("roots", explorerCommand("list", "", "")); !reply.OK || len(reply.Result.(map[string]any)["roots"].([]any)) == 0 {
		t.Fatal(reply)
	}
	cmd := explorerCommand("mkdir", root, rel)
	cmd["name"] = ".new-project"
	reply := call("mkdir", cmd)
	if !reply.OK || len(st.Get().ExplorerGrants) != 1 {
		t.Fatal(reply)
	}
	project := reply.Result.(map[string]any)["project"].(map[string]any)["path"].(string)
	if project != filepath.Join(parent, ".new-project") {
		t.Fatal(project)
	}
	if retry := call("new-request-same-name", cmd); retry.OK || !strings.Contains(retry.Error, "explorer_exists") {
		t.Fatal("new mkdir request adopted old directory", retry)
	}
}
