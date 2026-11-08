package hubclient

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func initializeNestedGit(t *testing.T, workspace, relative string) string {
	t.Helper()
	root := filepath.Join(workspace, relative)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.name", "Fixture"}, {"config", "user.email", "fixture@example.invalid"}, {"config", "commit.gpgsign", "false"}, {"config", "core.autocrlf", "false"}, {"config", "core.hooksPath", t.TempDir()}} {
		if _, err := workspaceGitRun(context.Background(), root, "git", args...); err != nil {
			t.Fatal(err)
		}
	}
	workspaceWrite(t, root, "main.txt", []byte("original\n"))
	for _, args := range [][]string{{"add", "main.txt"}, {"commit", "-m", "base"}} {
		if _, err := workspaceGitRun(context.Background(), root, "git", args...); err != nil {
			t.Fatal(err)
		}
	}
	workspaceWrite(t, root, "main.txt", []byte("changed\n"))
	return root
}
func readRepositories(t *testing.T, c *Client, root string) workspaceGitRepositories {
	t.Helper()
	result, err := c.Dispatch(context.Background(), gitCommand(root, "read", "repositories"))
	if err != nil {
		t.Fatal(err)
	}
	return result.(workspaceGitRepositories)
}
func TestWorkspaceGitRepositoriesIsolateIdenticalFilesAndPatches(t *testing.T) {
	c, _, root := workspaceFixture(t)
	first := initializeNestedGit(t, root, "desktop")
	second := initializeNestedGit(t, root, "mobile")
	repositories := readRepositories(t, c, root)
	if repositories.Truncated || len(repositories.Repositories) != 2 || repositories.Repositories[0].Path != "desktop" || repositories.Repositories[1].Path != "mobile" {
		t.Fatalf("discovery: %+v", repositories)
	}
	a := gitReviewRead(t, c, root, "unstaged", map[string]any{"repositoryPath": "desktop"})
	b := gitReviewRead(t, c, root, "unstaged", map[string]any{"repositoryPath": "mobile"})
	if len(a.Files) != 1 || len(b.Files) != 1 || a.Files[0].Diff != b.Files[0].Diff {
		t.Fatal("fixture must have identical patches")
	}
	if a.State.Revision == b.State.Revision || a.Files[0].PatchRevision == b.Files[0].PatchRevision || a.Files[0].Hunks[0].ID == b.Files[0].Hunks[0].ID {
		t.Fatal("repository identity omitted from revisions")
	}
	cmd := gitReviewMutation(root, a, a.Files[0], "stage", a.Files[0].Hunks[0].ID)
	cmd["repositoryPath"] = "mobile"
	if _, err := c.Dispatch(context.Background(), cmd); err == nil || !strings.Contains(err.Error(), "git_conflict") {
		t.Fatal("cross-repository state accepted", err)
	}
	cmd["expectedRevision"] = b.State.Revision
	if _, err := c.Dispatch(context.Background(), cmd); err == nil || !strings.Contains(err.Error(), "git_patch_changed") {
		t.Fatal("cross-repository patch accepted", err)
	}
	cmd["patchRevision"] = b.Files[0].PatchRevision
	if _, err := c.Dispatch(context.Background(), cmd); err == nil || !strings.Contains(err.Error(), "git_hunk_unknown") {
		t.Fatal("cross-repository hunk accepted", err)
	}
	cmd["hunk"] = b.Files[0].Hunks[0].ID
	gitReviewApply(t, c, cmd)
	stagedA, _ := workspaceGitRun(context.Background(), first, "git", "diff", "--cached")
	stagedB, _ := workspaceGitRun(context.Background(), second, "git", "diff", "--cached")
	if stagedA != "" || !strings.Contains(stagedB, "+changed") {
		t.Fatal("wrong repository mutated")
	}
	b = gitReviewRead(t, c, root, "staged", map[string]any{"repositoryPath": "mobile"})
	cmd = gitReviewMutation(root, b, b.Files[0], "unstage", b.Files[0].Hunks[0].ID)
	cmd["repositoryPath"] = "mobile"
	gitReviewApply(t, c, cmd)
	if len(gitReviewRead(t, c, root, "staged", map[string]any{"repositoryPath": "mobile"}).Files) != 0 {
		t.Fatal("scoped unstage failed")
	}
	diff := gitCommand(root, "read", "diff")
	diff["repositoryPath"] = "desktop"
	diff["path"] = "main.txt"
	reply, err := c.Dispatch(context.Background(), diff)
	if err != nil || reply.(map[string]any)["repository"].(workspaceGitRepository).Path != "desktop" {
		t.Fatal("diff identity missing", err)
	}
}
func TestWorkspaceGitRepositoriesParentAndNestedRemainSeparate(t *testing.T) {
	c, root := gitFixture(t)
	initializeNestedGit(t, root, "child")
	workspaceWrite(t, root, "main.txt", []byte("parent edit\n"))
	repositories := readRepositories(t, c, root)
	if len(repositories.Repositories) != 2 || repositories.Repositories[0].Path != "" || repositories.Repositories[1].Path != "child" {
		t.Fatal(repositories)
	}
	review := gitReviewRead(t, c, root, "unstaged", nil)
	if len(review.Files) != 1 || review.Files[0].Path != "main.txt" {
		t.Fatal("parent reviewed embedded repository", review)
	}
	cmd := gitCommand(root, "read", "review")
	cmd["reviewScope"] = "last-turn"
	cmd["path"] = "child/main.txt"
	cmd["sourceDiff"] = "@@ -1 +1 @@\n-original\n+changed\n"
	if _, err := c.Dispatch(context.Background(), cmd); err == nil {
		t.Fatal("parent last-turn crossed repository boundary")
	}
	cmd = gitCommand(root, "mutate", "stage")
	cmd["path"] = "child"
	cmd["expectedRevision"] = review.State.Revision
	if _, err := c.Dispatch(context.Background(), cmd); err == nil {
		t.Fatal("parent staged nested repository")
	}
}
func TestWorkspaceGitRepositoriesRejectOutsideMetadataAndPaths(t *testing.T) {
	c, _, root := workspaceFixture(t)
	outside := t.TempDir()
	initializeNestedGit(t, outside, "external")
	if err := os.Mkdir(filepath.Join(root, "outside-link"), 0700); err != nil {
		t.Fatal(err)
	}
	workspaceWrite(t, root, "outside-link/.git", []byte("gitdir: "+filepath.Join(outside, "external", ".git")))
	inside := initializeNestedGit(t, root, "unsafe-common")
	workspaceWrite(t, inside, ".git/commondir", []byte(filepath.Join(outside, "external", ".git")))
	if err := os.Symlink(filepath.Join(outside, "external"), filepath.Join(root, "symlink")); err != nil {
		t.Log("symlink unavailable on this host", err)
	}
	discovery := readRepositories(t, c, root)
	if len(discovery.Repositories) != 0 {
		t.Fatal("outside metadata discovered", discovery)
	}
	for _, path := range []string{"../external", "/outside", "outside-link", "unsafe-common", "symlink", "a/../b", "a/.git"} {
		cmd := gitCommand(root, "read", "status")
		cmd["repositoryPath"] = path
		if _, err := c.Dispatch(context.Background(), cmd); err == nil {
			t.Fatal("unsafe repository accepted", path)
		}
	}
}
func TestWorkspaceGitRepositoriesLimitsAndCapability(t *testing.T) {
	c, _, root := workspaceFixture(t)
	if err := os.MkdirAll(filepath.Join(root, "one/two/three/four/five/six/seven/eight/nine/ten"), 0700); err != nil {
		t.Fatal(err)
	}
	if !readRepositories(t, c, root).Truncated {
		t.Fatal("scan bound not reported")
	}
	found := false
	for _, cap := range c.Device(context.Background()).Capabilities {
		if cap == "workspace.git.repositories.v1" {
			found = true
		}
	}
	if !found {
		t.Fatal("repository capability missing")
	}
}

func TestWorkspaceGitRepositoriesRejectHiddenSubmoduleCommit(t *testing.T) {
	c, root := gitFixture(t)
	child := initializeNestedGit(t, root, "child")
	head, err := workspaceGitRun(context.Background(), child, "git", "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = workspaceGitRun(context.Background(), root, "git", "update-index", "--add", "--cacheinfo", "160000,"+strings.TrimSpace(head)+",child"); err != nil {
		t.Fatal(err)
	}
	workspaceWrite(t, root, "main.txt", []byte("parent edit\n"))
	if _, err = workspaceGitRun(context.Background(), root, "git", "add", "main.txt"); err != nil {
		t.Fatal(err)
	}
	state := gitState(t, c, root)
	if !state.NestedIndexChanges {
		t.Fatal("hidden gitlink changes were ignored")
	}
	cmd := gitCommand(root, "mutate", "commit")
	cmd["expectedRevision"] = state.Revision
	cmd["message"] = "must not commit an unreviewed submodule"
	if _, err = c.Dispatch(context.Background(), cmd); err == nil || !strings.Contains(err.Error(), "git_nested_index") {
		t.Fatal("unreviewed nested gitlink committed", err)
	}
}
func TestWorkspaceGitRepositoriesAllowContainedWorktree(t *testing.T) {
	c, root := gitFixture(t)
	if _, err := workspaceGitRun(context.Background(), root, "git", "worktree", "add", "--detach", filepath.Join(root, "linked"), "HEAD"); err != nil {
		t.Fatal(err)
	}
	repositories := readRepositories(t, c, root)
	if len(repositories.Repositories) != 2 || repositories.Repositories[1].Path != "linked" {
		t.Fatal("contained gitdir and commondir rejected", repositories)
	}
	review := gitReviewRead(t, c, root, "unstaged", map[string]any{"repositoryPath": "linked"})
	if review.State.Repository.Path != "linked" {
		t.Fatal("linked worktree identity missing")
	}
}
