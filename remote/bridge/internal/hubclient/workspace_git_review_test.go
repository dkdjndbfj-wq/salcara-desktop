package hubclient

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
)

func gitReviewRead(t *testing.T, c *Client, root, scope string, extra map[string]any) gitReview {
	t.Helper()
	cmd := gitCommand(root, "read", "review")
	cmd["reviewScope"] = scope
	for k, v := range extra {
		cmd[k] = v
	}
	result, err := c.Dispatch(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	return result.(gitReview)
}
func gitReviewMutation(root string, r gitReview, f gitReviewFile, operation, hunk string) map[string]any {
	cmd := gitCommand(root, "mutate", "review-apply")
	cmd["reviewScope"], cmd["expectedRevision"], cmd["path"], cmd["patchRevision"], cmd["operation"] = r.Scope, r.State.Revision, f.Path, f.PatchRevision, operation
	if hunk != "" {
		cmd["hunk"] = hunk
	}
	if r.Scope == "last-turn" {
		cmd["sourceDiff"] = f.Diff
	}
	return cmd
}
func gitReviewApply(t *testing.T, c *Client, cmd map[string]any) {
	t.Helper()
	if _, err := c.Dispatch(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
}
func gitLongFixture(t *testing.T, c *Client, root string) string {
	t.Helper()
	lines := []string{}
	for i := 0; i < 20; i++ {
		lines = append(lines, "unchanged line")
	}
	original := strings.Join(lines, "\n") + "\n"
	workspaceWrite(t, root, "main.txt", []byte(original))
	gitMutate(t, c, root, "stage", map[string]any{"path": "main.txt"})
	gitMutate(t, c, root, "commit", map[string]any{"message": "base with distant hunks"})
	return original
}
func TestGitReviewAdvertisesIndependentCapability(t *testing.T) {
	c, _, _ := workspaceFixture(t)
	caps := c.Device(context.Background()).Capabilities
	foundBase, foundReview := false, false
	for _, cap := range caps {
		if cap == "workspace.git.v1" {
			foundBase = true
		}
		if cap == "workspace.git.review.v1" {
			foundReview = true
		}
	}
	if !foundBase || !foundReview {
		t.Fatal("review must advertise a separate capability", caps)
	}
}

func TestGitReviewPartialStageUnstageDiscardRealRepository(t *testing.T) {
	c, root := gitFixture(t)
	original := gitLongFixture(t, c, root)
	changed := "first edit\n" + strings.TrimPrefix(original, "unchanged line\n")
	changed = strings.TrimSuffix(changed, "unchanged line\n") + "last edit\n"
	workspaceWrite(t, root, "main.txt", []byte(changed))
	review := gitReviewRead(t, c, root, "unstaged", nil)
	if len(review.Files) != 1 || len(review.Files[0].Hunks) != 2 {
		t.Fatal(review)
	}
	first := review.Files[0]
	gitReviewApply(t, c, gitReviewMutation(root, review, first, "stage", first.Hunks[0].ID))
	staged := gitReviewRead(t, c, root, "staged", nil)
	if len(staged.Files) != 1 || !strings.Contains(staged.Files[0].Diff, "+first edit") || strings.Contains(staged.Files[0].Diff, "+last edit") {
		t.Fatal("wrong hunk staged", staged)
	}
	remaining := gitReviewRead(t, c, root, "unstaged", nil)
	if strings.Contains(remaining.Files[0].Diff, "+first edit") || !strings.Contains(remaining.Files[0].Diff, "+last edit") {
		t.Fatal("other hunk lost")
	}
	gitReviewApply(t, c, gitReviewMutation(root, staged, staged.Files[0], "unstage", staged.Files[0].Hunks[0].ID))
	if len(gitReviewRead(t, c, root, "staged", nil).Files) != 0 {
		t.Fatal("unstage failed")
	}
	review = gitReviewRead(t, c, root, "unstaged", nil)
	gitReviewApply(t, c, gitReviewMutation(root, review, review.Files[0], "discard", review.Files[0].Hunks[1].ID))
	data, err := os.ReadFile(filepath.Join(root, "main.txt"))
	if err != nil || !strings.HasPrefix(string(data), "first edit\n") || strings.Contains(string(data), "last edit") {
		t.Fatal("discard touched wrong text", err)
	}
	review = gitReviewRead(t, c, root, "unstaged", nil)
	gitReviewApply(t, c, gitReviewMutation(root, review, review.Files[0], "discard", ""))
	data, _ = os.ReadFile(filepath.Join(root, "main.txt"))
	if string(data) != original {
		t.Fatal("whole-file discard not restored")
	}
}

func TestGitReviewRenameIncludesBothPaths(t *testing.T) {
	c, root := gitFixture(t)
	if _, err := workspaceGitRun(context.Background(), root, "git", "mv", "main.txt", "moved.txt"); err != nil {
		t.Fatal(err)
	}
	workspaceWrite(t, root, "main.txt", []byte("new untracked replacement\n"))
	r := gitReviewRead(t, c, root, "staged", nil)
	if len(r.Files) != 2 {
		t.Fatal("rename did not retain both sides", r.Files)
	}
	foundOld, foundNew := false, false
	for _, file := range r.Files {
		if file.Path == "main.txt" && file.Deleted {
			foundOld = true
		}
		if file.Path == "moved.txt" && file.Added {
			foundNew = true
		}
	}
	if !foundOld || !foundNew {
		t.Fatal("rename review conflated recreated working file with staged deletion", r.Files)
	}
}

func TestGitReviewMergeCommitComparesFirstParent(t *testing.T) {
	c, root := gitFixture(t)
	run := func(args ...string) {
		t.Helper()
		if _, err := workspaceGitRun(context.Background(), root, "git", args...); err != nil {
			t.Fatal(err)
		}
	}
	run("switch", "-c", "feature")
	workspaceWrite(t, root, "feature.txt", []byte("feature content\n"))
	run("add", "feature.txt")
	run("commit", "-m", "feature")
	run("switch", "main")
	workspaceWrite(t, root, "main-only.txt", []byte("main content\n"))
	run("add", "main-only.txt")
	run("commit", "-m", "main")
	run("merge", "--no-ff", "feature", "-m", "merge feature")
	review := gitReviewRead(t, c, root, "commit", map[string]any{"reviewRef": "HEAD"})
	if review.Base == "" || len(review.Files) != 1 || review.Files[0].Path != "feature.txt" || !strings.Contains(review.Files[0].Diff, "+feature content") {
		t.Fatal("merge review was empty or mixed the wrong parent", review)
	}
}
func TestGitReviewRejectsChangedRevisionAndUnknownHunks(t *testing.T) {
	c, root := gitFixture(t)
	workspaceWrite(t, root, "main.txt", []byte("changed\n"))
	r := gitReviewRead(t, c, root, "unstaged", nil)
	cmd := gitReviewMutation(root, r, r.Files[0], "stage", strings.Repeat("0", 64))
	if _, err := c.Dispatch(context.Background(), cmd); err == nil || !strings.Contains(err.Error(), "hunk_unknown") {
		t.Fatal("unknown hunk admitted", err)
	}
	cmd = gitReviewMutation(root, r, r.Files[0], "discard", r.Files[0].Hunks[0].ID)
	workspaceWrite(t, root, "main.txt", []byte("external newer edit\n"))
	if _, err := c.Dispatch(context.Background(), cmd); err == nil || !strings.Contains(err.Error(), "git_conflict") {
		t.Fatal("stale discard admitted", err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "main.txt"))
	if string(data) != "external newer edit\n" {
		t.Fatal("newer content overwritten")
	}
}
func TestGitReviewStagedDiscardNeverOverwritesUnstagedChanges(t *testing.T) {
	c, root := gitFixture(t)
	workspaceWrite(t, root, "main.txt", []byte("staged version\n"))
	gitMutate(t, c, root, "stage", map[string]any{"path": "main.txt"})
	workspaceWrite(t, root, "main.txt", []byte("unrelated working version\n"))
	r := gitReviewRead(t, c, root, "staged", nil)
	if _, err := c.Dispatch(context.Background(), gitReviewMutation(root, r, r.Files[0], "discard", r.Files[0].Hunks[0].ID)); err == nil {
		t.Fatal("overwrote partial staging")
	}
	data, _ := os.ReadFile(filepath.Join(root, "main.txt"))
	if string(data) != "unrelated working version\n" {
		t.Fatal("working edit lost")
	}
	staged := gitReviewRead(t, c, root, "staged", nil)
	if !strings.Contains(staged.Files[0].Diff, "+staged version") {
		t.Fatal("index changed on refusal")
	}
}
func TestGitReviewBranchCommitAndUntrackedScopes(t *testing.T) {
	c, root := gitFixture(t)
	base := gitState(t, c, root).Head
	gitMutate(t, c, root, "branch", map[string]any{"branch": "feature"})
	gitMutate(t, c, root, "checkout", map[string]any{"branch": "feature"})
	workspaceWrite(t, root, "main.txt", []byte("committed branch change\n"))
	gitMutate(t, c, root, "stage", map[string]any{"path": "main.txt"})
	gitMutate(t, c, root, "commit", map[string]any{"message": "feature change"})
	workspaceWrite(t, root, "main.txt", []byte("dirty worktree\n"))
	workspaceWrite(t, root, "new.txt", []byte("new unicode 世界"))
	for _, scope := range []string{"branch", "commit"} {
		r := gitReviewRead(t, c, root, scope, map[string]any{"reviewRef": map[string]string{"branch": "main", "commit": "HEAD"}[scope]})
		if len(r.Files) != 1 || !strings.Contains(r.Files[0].Diff, "+committed branch change") || strings.Contains(r.Files[0].Diff, "dirty worktree") {
			t.Fatal(scope, r)
		}
		if len(r.Commits) < 2 {
			t.Fatal("missing commit choices")
		}
	}
	initial := gitReviewRead(t, c, root, "commit", map[string]any{"reviewRef": base})
	if len(initial.Files) != 1 || !initial.Files[0].Added {
		t.Fatal("root commit missing", initial)
	}
	r := gitReviewRead(t, c, root, "unstaged", nil)
	var added gitReviewFile
	for _, f := range r.Files {
		if f.Path == "new.txt" {
			added = f
		}
	}
	if len(added.Hunks) != 1 || !strings.Contains(added.Diff, "No newline") {
		t.Fatal("untracked text unavailable", added)
	}
	gitReviewApply(t, c, gitReviewMutation(root, r, added, "stage", added.Hunks[0].ID))
	staged := gitReviewRead(t, c, root, "staged", nil)
	if len(staged.Files) != 1 || !strings.Contains(staged.Files[0].Diff, "世界") {
		t.Fatal("untracked staging lost bytes")
	}
	gitReviewApply(t, c, gitReviewMutation(root, staged, staged.Files[0], "unstage", ""))
	r = gitReviewRead(t, c, root, "unstaged", nil)
	for _, f := range r.Files {
		if f.Path == "new.txt" {
			gitReviewApply(t, c, gitReviewMutation(root, r, f, "discard", ""))
		}
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("new file not removed", err)
	}
}
func TestGitReviewLastTurnPatchIsPathBoundAndScopeRechecked(t *testing.T) {
	c, root := gitFixture(t)
	workspaceWrite(t, root, "main.txt", []byte("turn edit\n"))
	source := "@@ -1 +1 @@\n-original\n+turn edit\n"
	r := gitReviewRead(t, c, root, "last-turn", map[string]any{"path": "main.txt", "sourceDiff": source})
	gitReviewApply(t, c, gitReviewMutation(root, r, r.Files[0], "discard", r.Files[0].Hunks[0].ID))
	data, _ := os.ReadFile(filepath.Join(root, "main.txt"))
	if string(data) != "original\n" {
		t.Fatal("last turn reverse failed")
	}
	cmd := gitCommand(root, "read", "review")
	cmd["reviewScope"], cmd["path"], cmd["sourceDiff"] = "last-turn", "main.txt", "--- a/other.txt\n+++ b/other.txt\n"+source
	if _, err := c.Dispatch(context.Background(), cmd); err == nil {
		t.Fatal("mismatched path accepted")
	}
	workspaceWrite(t, root, "main.txt", []byte("turn edit\n"))
	r = gitReviewRead(t, c, root, "unstaged", nil)
	a := &workspaceWriteRaceAgent{fakeAgent: &fakeAgent{id: "codex"}, metadata: protocol.SessionInfo{SessionKey: workspaceTestKey, Cwd: root, ControlSurface: "cli"}}
	a.onRecheck = func() { a.metadata.Cwd = filepath.Dir(root) }
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	if _, err := c.Dispatch(context.Background(), gitReviewMutation(root, r, r.Files[0], "discard", r.Files[0].Hunks[0].ID)); err == nil {
		t.Fatal("revoked session changed old repo")
	}
	data, _ = os.ReadFile(filepath.Join(root, "main.txt"))
	if string(data) != "turn edit\n" {
		t.Fatal("revoked discard changed file")
	}
}
