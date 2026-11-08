package hubclient

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func gitFixture(t *testing.T) (*Client, string) {
	t.Helper()
	c, _, root := workspaceFixture(t)
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.name", "Fixture"}, {"config", "user.email", "fixture@example.invalid"}, {"config", "commit.gpgsign", "false"}, {"config", "core.autocrlf", "false"}, {"config", "core.hooksPath", t.TempDir()}} {
		if _, err := workspaceGitRun(context.Background(), root, "git", args...); err != nil {
			t.Fatal(err)
		}
	}
	workspaceWrite(t, root, "main.txt", []byte("original\n"))
	for _, args := range [][]string{{"add", "main.txt"}, {"commit", "-m", "initial"}} {
		if _, err := workspaceGitRun(context.Background(), root, "git", args...); err != nil {
			t.Fatal(err)
		}
	}
	return c, root
}
func gitCommand(root, typ, action string) map[string]any {
	return map[string]any{"type": "workspace.git." + typ, "sessionKey": workspaceTestKey, "controlSurface": "cli", "expectedCwd": root, "action": action}
}
func gitState(t *testing.T, c *Client, root string) workspaceGitState {
	t.Helper()
	result, err := c.Dispatch(context.Background(), gitCommand(root, "read", "status"))
	if err != nil {
		t.Fatal(err)
	}
	return result.(workspaceGitState)
}
func gitMutate(t *testing.T, c *Client, root, action string, fields map[string]any) workspaceGitState {
	t.Helper()
	cmd := gitCommand(root, "mutate", action)
	cmd["expectedRevision"] = gitState(t, c, root).Revision
	for key, value := range fields {
		cmd[key] = value
	}
	result, err := c.Dispatch(context.Background(), cmd)
	if err != nil {
		t.Fatal(action, err)
	}
	return result.(map[string]any)["state"].(workspaceGitState)
}

func TestWorkspaceGitRealStagingCommitAndStaleGuard(t *testing.T) {
	c, root := gitFixture(t)
	workspaceWrite(t, root, "main.txt", []byte("edited\n"))
	state := gitState(t, c, root)
	if len(state.Files) != 1 || state.Files[0].Worktree != "M" {
		t.Fatal(state)
	}
	cmd := gitCommand(root, "read", "diff")
	cmd["path"] = "main.txt"
	result, err := c.Dispatch(context.Background(), cmd)
	if err != nil || !strings.Contains(result.(map[string]any)["diff"].(string), "+edited") {
		t.Fatalf("diff %v", err)
	}
	cmd = gitCommand(root, "mutate", "stage")
	cmd["path"] = "main.txt"
	cmd["expectedRevision"] = state.Revision
	workspaceWrite(t, root, "main.txt", []byte("new external edit\n"))
	if _, err = c.Dispatch(context.Background(), cmd); err == nil || !strings.Contains(err.Error(), "git_conflict") {
		t.Fatal("stale stage allowed", err)
	}
	state = gitMutate(t, c, root, "stage", map[string]any{"path": "main.txt"})
	if state.Files[0].Index != "M" {
		t.Fatal("not staged")
	}
	state = gitMutate(t, c, root, "unstage", map[string]any{"path": "main.txt"})
	if state.Files[0].Index != " " {
		t.Fatal("not unstaged")
	}
	gitMutate(t, c, root, "stage", map[string]any{"path": "main.txt"})
	state = gitMutate(t, c, root, "commit", map[string]any{"message": "phone change"})
	if len(state.Files) != 0 {
		t.Fatal("not clean")
	}
}
func TestWorkspaceGitRealBranchAndWorktreeLifecycle(t *testing.T) {
	c, root := gitFixture(t)
	gitMutate(t, c, root, "branch", map[string]any{"branch": "feature"})
	workspaceWrite(t, root, "main.txt", []byte("dirty\n"))
	cmd := gitCommand(root, "mutate", "checkout")
	cmd["branch"] = "feature"
	cmd["expectedRevision"] = gitState(t, c, root).Revision
	if _, err := c.Dispatch(context.Background(), cmd); err == nil {
		t.Fatal("dirty checkout allowed")
	}
	workspaceWrite(t, root, "main.txt", []byte("original\n"))
	state := gitMutate(t, c, root, "checkout", map[string]any{"branch": "feature"})
	if state.Branch != "feature" {
		t.Fatal(state.Branch)
	}
	state = gitMutate(t, c, root, "worktree-create", map[string]any{"branch": "isolated", "name": "isolated"})
	path := filepath.Join(root, ".salcara-worktrees", "isolated")
	if len(state.Worktrees) != 2 {
		t.Fatal(state.Worktrees)
	}
	workspaceWrite(t, path, "dirty.txt", []byte("keep me"))
	cmd = gitCommand(root, "mutate", "worktree-remove")
	cmd["path"] = path
	cmd["expectedRevision"] = state.Revision
	if _, err := c.Dispatch(context.Background(), cmd); err == nil {
		t.Fatal("dirty worktree removed")
	}
	if err := os.Remove(filepath.Join(path, "dirty.txt")); err != nil {
		t.Fatal(err)
	}
	state = gitMutate(t, c, root, "worktree-remove", map[string]any{"path": path})
	if len(state.Worktrees) != 1 {
		t.Fatal(state.Worktrees)
	}
}
func TestWorkspaceGitRejectsParentRepositoryAndUnsafePaths(t *testing.T) {
	c, root := gitFixture(t)
	cmd := gitCommand(root, "read", "diff")
	cmd["path"] = "../outside"
	if _, err := c.Dispatch(context.Background(), cmd); err == nil {
		t.Fatal("traversal allowed")
	}
	_, a, _ := workspaceFixture(t)
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	a.sessions = []protocol.SessionInfo{{SessionKey: workspaceTestKey, Cwd: child, ControlSurface: "cli"}}
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	if err := c.o.Store.Update(func(cfg *config.Config) error {
		cfg.Projects = []protocol.Project{{Path: child, Name: "child"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Dispatch(context.Background(), gitCommand(child, "read", "status")); err == nil || !strings.Contains(err.Error(), "git_root_mismatch") && !strings.Contains(err.Error(), "git_not_repository") {
		t.Fatal("parent repo accessed", err)
	}
}

func TestWorkspaceGitPushesOnlyReviewedBranchToLocalRemote(t *testing.T) {
	c, root := gitFixture(t)
	remote := t.TempDir()
	for _, operation := range []struct {
		root string
		args []string
	}{{remote, []string{"init", "--bare"}}, {root, []string{"remote", "add", "fixture", remote}}, {root, []string{"push", "--set-upstream", "fixture", "main"}}} {
		if _, err := workspaceGitRun(context.Background(), operation.root, "git", operation.args...); err != nil {
			t.Fatal(err)
		}
	}
	workspaceWrite(t, root, "main.txt", []byte("push fixture\n"))
	gitMutate(t, c, root, "stage", map[string]any{"path": "main.txt"})
	state := gitMutate(t, c, root, "commit", map[string]any{"message": "local push verification"})
	gitMutate(t, c, root, "push", nil)
	head, err := workspaceGitRun(context.Background(), remote, "git", "rev-parse", "refs/heads/main")
	if err != nil || strings.TrimSpace(head) != state.Head {
		t.Fatal("local push not verified", err)
	}
}

func TestGitOutputBoundDoesNotUseUnboundedReaderFrom(t *testing.T) {
	var output gitOutput
	if _, ok := any(&output).(interface {
		ReadFrom(io.Reader) (int64, error)
	}); ok {
		t.Fatal("unbounded embedded buffer reader exposed")
	}
	_, _ = output.Write(make([]byte, 1024*1024+1))
	if !output.overflow || output.buffer.Len() != 0 {
		t.Fatal("output exceeded cap")
	}
}

func TestWorkspaceGitRechecksScopeAfterSnapshotBeforeMutation(t *testing.T) {
	c, root := gitFixture(t)
	workspaceWrite(t, root, "main.txt", []byte("pending phone stage\n"))
	revision := gitState(t, c, root).Revision
	for _, test := range []struct{ action, change string }{
		{"stage", "session-moved"},
		{"stage", "project-revoked"},
		{"stage", "binding-replaced"},
		{"stage", "phone-replaced"},
		{"stage", "station-changed"},
		{"stage", "guard-revoked"},
		{"stage", "cancelled"},
		{"worktree-create", "session-moved"},
	} {
		t.Run(test.action+"/"+test.change, func(t *testing.T) {
			before := c.o.Store.Get()
			t.Cleanup(func() {
				if err := c.o.Store.Update(func(cfg *config.Config) error { *cfg = before; return nil }); err != nil {
					t.Error(err)
				}
			})
			if err := c.o.Store.Update(func(cfg *config.Config) error {
				cfg.PhoneBindingID, cfg.PhoneHash = "git-snapshot-binding", strings.Repeat("a", 64)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			cfg := c.o.Store.Get()
			c.publishPairState(PairStatus{DeviceID: cfg.DeviceID}, eventIdentity(cfg), true)
			ctx, cancel := context.WithCancel(remoteAdmissionGuard(cfg))
			defer cancel()
			a := &workspaceWriteRaceAgent{fakeAgent: &fakeAgent{id: "codex"}, metadata: protocol.SessionInfo{SessionKey: workspaceTestKey, Cwd: root, ControlSurface: "cli"}}
			// The first lookup admits the old cwd. The next exact lookup observes
			// a change at the end of the expensive snapshot/preflight interval.
			// A post-mutation-only check still returns an error, so verify the
			// actual repository below rather than merely expecting rejection.
			a.onRecheck = func() {
				if test.change == "session-moved" {
					a.metadata.Cwd = filepath.Dir(root)
					return
				}
				if test.change == "guard-revoked" {
					c.publishPairState(PairStatus{DeviceID: cfg.DeviceID}, "", true)
					return
				}
				if test.change == "cancelled" {
					cancel()
					return
				}
				if err := c.o.Store.Update(func(next *config.Config) error {
					switch test.change {
					case "project-revoked":
						next.Projects = nil
					case "binding-replaced":
						next.PhoneBindingID = "replacement-binding"
					case "phone-replaced":
						next.PhoneHash = strings.Repeat("b", 64)
					case "station-changed":
						next.DeviceID = cfg.DeviceID + "-replacement"
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			cmd := gitCommand(root, "mutate", test.action)
			cmd["expectedRevision"] = revision
			if test.action == "stage" {
				cmd["path"] = "main.txt"
			} else {
				cmd["name"], cmd["branch"] = "must-not-exist", "must-not-exist"
			}
			if result, err := c.Dispatch(ctx, cmd); err == nil || result != nil {
				t.Fatal("changed ownership admitted a mutation", err)
			}
			if a.calls < 2 {
				t.Fatal("did not reach the post-snapshot exact-session check")
			}
			staged, err := workspaceGitRun(context.Background(), root, "git", "diff", "--cached", "--name-only")
			if err != nil || staged != "" {
				t.Fatalf("revoked command changed the old repository index: %q, %v", staged, err)
			}
			if _, err := os.Stat(filepath.Join(root, ".salcara-worktrees")); !os.IsNotExist(err) {
				t.Fatal("revoked command created a worktree directory", err)
			}
		})
	}
}
