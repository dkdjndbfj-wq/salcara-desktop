package hubclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

type gitReviewHunk struct {
	ID       string `json:"id"`
	Header   string `json:"header"`
	OldStart int    `json:"oldStart"`
	OldCount int    `json:"oldCount"`
	NewStart int    `json:"newStart"`
	NewCount int    `json:"newCount"`
	Patch    string `json:"-"`
}
type gitReviewFile struct {
	Path          string          `json:"path"`
	Diff          string          `json:"diff"`
	PatchRevision string          `json:"patchRevision"`
	Additions     int             `json:"additions"`
	Deletions     int             `json:"deletions"`
	Binary        bool            `json:"binary"`
	Reason        string          `json:"reason,omitempty"`
	Hunks         []gitReviewHunk `json:"hunks"`
	Added         bool            `json:"added"`
	Deleted       bool            `json:"deleted"`
}
type gitReviewCommit struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}
type gitReview struct {
	Scope   string            `json:"scope"`
	Ref     string            `json:"ref"`
	Base    string            `json:"base"`
	State   workspaceGitState `json:"state"`
	Files   []gitReviewFile   `json:"files"`
	Commits []gitReviewCommit `json:"commits"`
}

func gitReviewHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

var gitHunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@.*$`)
var gitCommitID = regexp.MustCompile(`^[a-f0-9]{40,64}$`)

// Only complete, single-file unified text patches are eligible for partial
// changes. The patch returned by Git is retained exactly, including no-newline
// markers. Phone-supplied last-turn snapshots are validated by the same parser.
func parseGitReviewFile(path, diff string) (gitReviewFile, error) {
	f := gitReviewFile{Path: path, Diff: diff, PatchRevision: gitReviewHash(path + "\x00" + diff), Hunks: []gitReviewHunk{}}
	if !gitSafePath(path) || len(diff) > 192*1024 || strings.ContainsRune(diff, 0) {
		return f, errors.New("git_patch_invalid")
	}
	if diff == "" {
		f.Reason = "此文件没有文本差异"
		return f, nil
	}
	lines := strings.SplitAfter(diff, "\n")
	header := ""
	chunks := []string{}
	var current strings.Builder
	oldPath, newPath := "", ""
	oldLeft, newLeft := 0, 0
	priorOldEnd, priorNewEnd := 0, 0
	hasHunk := false
	flush := func() {
		if current.Len() > 0 {
			chunks = append(chunks, current.String())
			current.Reset()
		}
	}
	for _, raw := range lines {
		if raw == "" {
			continue
		}
		line := strings.TrimSuffix(raw, "\n")
		if match := gitHunkHeader.FindStringSubmatch(line); match != nil {
			if oldLeft != 0 || newLeft != 0 {
				return f, errors.New("git_patch_incomplete")
			}
			flush()
			oldStart, _ := strconv.Atoi(match[1])
			newStart, _ := strconv.Atoi(match[3])
			oldCount, newCount := 1, 1
			if match[2] != "" {
				oldCount, _ = strconv.Atoi(match[2])
			}
			if match[4] != "" {
				newCount, _ = strconv.Atoi(match[4])
			}
			if oldStart < priorOldEnd || newStart < priorNewEnd || oldCount < 0 || newCount < 0 || oldCount > 100000 || newCount > 100000 || oldCount > 0 && oldStart < 1 || newCount > 0 && newStart < 1 {
				return f, errors.New("git_patch_coordinates")
			}
			priorOldEnd, priorNewEnd = oldStart+oldCount, newStart+newCount
			oldLeft, newLeft = oldCount, newCount
			hasHunk = true
			f.Hunks = append(f.Hunks, gitReviewHunk{Header: line, OldStart: oldStart, OldCount: oldCount, NewStart: newStart, NewCount: newCount})
			current.WriteString(raw)
			continue
		}
		if hasHunk {
			if line == "\\ No newline at end of file" {
				current.WriteString(raw)
				continue
			}
			if len(line) == 0 {
				return f, errors.New("git_patch_incomplete")
			}
			switch line[0] {
			case ' ':
				if oldLeft <= 0 || newLeft <= 0 {
					return f, errors.New("git_patch_incomplete")
				}
				oldLeft--
				newLeft--
			case '-':
				if oldLeft <= 0 {
					return f, errors.New("git_patch_incomplete")
				}
				oldLeft--
				f.Deletions++
			case '+':
				if newLeft <= 0 {
					return f, errors.New("git_patch_incomplete")
				}
				newLeft--
				f.Additions++
			default:
				return f, errors.New("git_patch_invalid")
			}
			current.WriteString(raw)
			continue
		}
		switch {
		case strings.HasPrefix(line, "--- "):
			if oldPath != "" {
				return f, errors.New("git_patch_multiple_files")
			}
			oldPath = strings.SplitN(strings.TrimPrefix(line, "--- "), "\t", 2)[0]
			if oldPath != "/dev/null" && oldPath != "a/"+path {
				return f, errors.New("git_patch_path_mismatch")
			}
		case strings.HasPrefix(line, "+++ "):
			if newPath != "" {
				return f, errors.New("git_patch_multiple_files")
			}
			newPath = strings.SplitN(strings.TrimPrefix(line, "+++ "), "\t", 2)[0]
			if newPath != "/dev/null" && newPath != "b/"+path {
				return f, errors.New("git_patch_path_mismatch")
			}
		case strings.HasPrefix(line, "diff --git "):
			if header != "" || line != "diff --git a/"+path+" b/"+path {
				return f, errors.New("git_patch_path_mismatch")
			}
		case strings.HasPrefix(line, "index "):
		case line == "new file mode 100644" || line == "new file mode 100755":
			f.Added = true
		case line == "deleted file mode 100644" || line == "deleted file mode 100755":
			f.Deleted = true
		case line == "old mode 100644" || line == "old mode 100755" || line == "new mode 100644" || line == "new mode 100755":
			f.Reason = "权限变化请按整个文件处理"
		case strings.HasPrefix(line, "Binary files "):
			f.Binary = true
			f.Reason = "二进制文件仅支持整个文件暂存或取消暂存"
		default:
			f.Reason = "此类型差异暂不支持分块修改"
		}
		header += raw
	}
	if oldLeft != 0 || newLeft != 0 {
		return f, errors.New("git_patch_incomplete")
	}
	flush()
	if hasHunk && oldPath == "" && newPath == "" {
		header = "--- a/" + path + "\n+++ b/" + path + "\n"
		oldPath = "a/" + path
		newPath = "b/" + path
	}
	if hasHunk && (oldPath == "" || newPath == "" || oldPath == "/dev/null" && newPath == "/dev/null") {
		return f, errors.New("git_patch_invalid")
	}
	f.Added = f.Added || oldPath == "/dev/null"
	f.Deleted = f.Deleted || newPath == "/dev/null"
	for i := range f.Hunks {
		f.Hunks[i].Patch = header + chunks[i]
		f.Hunks[i].ID = gitReviewHash(path + "\x00" + f.Hunks[i].Patch)
	}
	if !hasHunk && f.Reason == "" {
		f.Reason = "无可操作的文本差异段"
	}
	return f, nil
}
func gitReviewDiffArgs(scope, base, ref string) []string {
	args := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color", "--no-renames", "--unified=3"}
	switch scope {
	case "staged":
		args = append(args, "--cached")
	case "branch":
		args = append(args, base, ref)
	case "commit":
		if base != "" {
			args = append(args, base, ref)
		} else {
			args = []string{"diff-tree", "--root", "--no-commit-id", "--no-ext-diff", "--no-textconv", "--no-color", "--no-renames", "--unified=3", "-r", "-p", ref}
		}
	}
	return args
}
func gitReviewRefs(ctx context.Context, root, scope, requested string, state workspaceGitState) (string, string, error) {
	if scope != "branch" && scope != "commit" {
		return "", "", nil
	}
	if requested == "" {
		if scope == "commit" {
			requested = state.Head
		} else {
			requested = state.Upstream
			if requested == "" {
				for _, name := range []string{"main", "master"} {
					if name != state.Branch {
						for _, branch := range state.Branches {
							if branch == name {
								requested = name
							}
						}
					}
				}
			}
			if requested == "" {
				requested = state.Head
			}
		}
	}
	if requested == "" || len(requested) > 256 || strings.HasPrefix(requested, "-") || strings.ContainsAny(requested, "\x00\r\n") || strings.Contains(requested, "@{") {
		return "", "", errors.New("git_review_ref_invalid")
	}
	resolved, err := workspaceGitRun(ctx, root, "git", "rev-parse", "--verify", "--end-of-options", requested+"^{commit}")
	ref := strings.TrimSpace(resolved)
	if err != nil || !gitCommitID.MatchString(ref) {
		return "", "", errors.New("git_review_ref_unknown: 比较分支或提交不存在")
	}
	if scope == "commit" {
		parents, e := workspaceGitRun(ctx, root, "git", "rev-list", "--parents", "-n", "1", ref)
		if e != nil {
			return "", "", e
		}
		fields := strings.Fields(parents)
		if len(fields) > 1 {
			return fields[1], ref, nil
		}
		return "", ref, nil
	}
	base, err := workspaceGitRun(ctx, root, "git", "merge-base", ref, "HEAD")
	if err != nil {
		return "", "", errors.New("git_review_no_merge_base")
	}
	return strings.TrimSpace(base), state.Head, nil
}
func gitUntrackedPatch(root, path string) (string, error) {
	opened, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer opened.Close()
	info, err := opened.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 128*1024 {
		return "", errors.New("git_review_untracked_unsupported: 新文件过大或不是普通文件")
	}
	data, err := opened.ReadFile(path)
	if err != nil {
		return "", err
	}
	if bytesContainsZero(data) {
		return "Binary files /dev/null and b/" + path + " differ\n", nil
	}
	header := "diff --git a/" + path + " b/" + path + "\nnew file mode 100644\n--- /dev/null\n+++ b/" + path + "\n"
	if len(data) == 0 {
		return header, nil
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	header += fmt.Sprintf("@@ -0,0 +1,%d @@\n", len(lines))
	for _, line := range lines {
		header += "+" + line + "\n"
	}
	if data[len(data)-1] != '\n' {
		header += "\\ No newline at end of file\n"
	}
	return header, nil
}
func bytesContainsZero(data []byte) bool {
	for _, b := range data {
		if b == 0 {
			return true
		}
	}
	return false
}
func workspaceGitReview(ctx context.Context, root string, state workspaceGitState, cmd map[string]any) (gitReview, error) {
	scope, requested := str(cmd, "reviewScope"), str(cmd, "reviewRef")
	r := gitReview{Scope: scope, State: state, Files: []gitReviewFile{}, Commits: []gitReviewCommit{}}
	if scope != "unstaged" && scope != "staged" && scope != "branch" && scope != "commit" && scope != "last-turn" {
		return r, errors.New("git_review_scope_invalid")
	}
	base, ref, err := gitReviewRefs(ctx, root, scope, requested, state)
	if err != nil {
		return r, err
	}
	r.Base, r.Ref = base, ref
	if state.Head != "" {
		log, e := workspaceGitRun(ctx, root, "git", "log", "-30", "--format=%H%x00%s%x00")
		if e != nil {
			return r, e
		}
		parts := strings.Split(log, "\x00")
		for i := 0; i+1 < len(parts); i += 2 {
			id := strings.TrimSpace(parts[i])
			if gitCommitID.MatchString(id) {
				r.Commits = append(r.Commits, gitReviewCommit{ID: id, Title: parts[i+1]})
			}
		}
	}
	if scope == "last-turn" {
		if !gitSafePath(str(cmd, "path")) || gitCrossesRepository(root, str(cmd, "path")) {
			return r, errors.New("git_repository_boundary: 请选择文件所属仓库")
		}
		f, e := parseGitReviewFile(str(cmd, "path"), str(cmd, "sourceDiff"))
		if e != nil {
			return r, e
		}
		r.Files = append(r.Files, bindGitReviewRepository(root, f))
		return r, nil
	}
	paths := []string{}
	untracked := map[string]bool{}
	if scope == "unstaged" || scope == "staged" {
		for _, f := range state.Files {
			if scope == "unstaged" && f.Worktree != " " || scope == "staged" && f.Index != " " && f.Index != "?" {
				paths = append(paths, f.Path)
				if scope == "staged" && f.Original != "" {
					paths = append(paths, f.Original)
				}
				if f.Index == "?" {
					untracked[f.Path] = true
				}
			}
		}
	} else {
		args := gitReviewDiffArgs(scope, base, ref)
		args = append(args, "--name-only", "-z")
		names, e := workspaceGitRun(ctx, root, "git", args...)
		if e != nil {
			return r, e
		}
		for _, p := range strings.Split(names, "\x00") {
			if p != "" {
				paths = append(paths, p)
			}
		}
	}
	total := 0
	seenPaths := map[string]bool{}
	for _, path := range paths {
		if seenPaths[path] {
			continue
		}
		seenPaths[path] = true
		if !gitSafePath(path) || len(paths) > 1000 {
			return r, errors.New("git_review_paths_unsupported")
		}
		var patch string
		if untracked[path] {
			patch, err = gitUntrackedPatch(root, path)
		} else {
			args := append(gitReviewDiffArgs(scope, base, ref), "--", path)
			patch, err = workspaceGitRun(ctx, root, "git", args...)
		}
		if err != nil {
			return r, err
		}
		total += len(patch)
		if total > 192*1024 {
			return r, errors.New("git_review_limit: 差异过大，请在电脑查看")
		}
		f, e := parseGitReviewFile(path, patch)
		if e != nil {
			return r, e
		}
		r.Files = append(r.Files, bindGitReviewRepository(root, f))
	}
	return r, nil
}
func bindGitReviewRepository(root string, file gitReviewFile) gitReviewFile {
	id := gitRepositoryID(root)
	file.PatchRevision = gitReviewHash(id + "\x00" + file.PatchRevision)
	for index := range file.Hunks {
		file.Hunks[index].ID = gitReviewHash(id + "\x00" + file.Hunks[index].ID)
	}
	return file
}
func gitApplyPatch(ctx context.Context, root, patch string, args ...string) error {
	argv := append([]string{"--no-pager", "--literal-pathspecs", "-C", root, "apply", "--whitespace=nowarn"}, args...)
	command := exec.CommandContext(ctx, "git", argv...)
	command.Dir = root
	command.Env = append(terminalEnvironment(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	hidePreviewProcess(command)
	command.Stdin = strings.NewReader(patch)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return errors.New("git_patch_conflict: 所选改动与当前文件或暂存区不一致；未覆盖其他改动，请刷新审查")
	}
	return nil
}
func workspaceGitReviewApply(ctx context.Context, root string, state workspaceGitState, cmd map[string]any, validateScope func() error) error {
	scope, operation, path, hunk := str(cmd, "reviewScope"), str(cmd, "operation"), str(cmd, "path"), str(cmd, "hunk")
	if scope != "unstaged" && scope != "staged" && scope != "last-turn" || operation != "stage" && operation != "unstage" && operation != "discard" || scope == "unstaged" && operation == "unstage" || scope == "staged" && operation == "stage" {
		return errors.New("git_review_action_invalid")
	}
	review, err := workspaceGitReview(ctx, root, state, cmd)
	if err != nil {
		return err
	}
	var file *gitReviewFile
	for i := range review.Files {
		if review.Files[i].Path == path {
			file = &review.Files[i]
			break
		}
	}
	if file == nil || file.PatchRevision != str(cmd, "patchRevision") {
		return errors.New("git_patch_changed: 所选差异已变化，请重新查看")
	}
	// Regenerate canonical patches on the computer. A hunk identifier never
	// authorizes arbitrary patch text from the phone for live Git scopes.
	patch := file.Diff
	if hunk != "" {
		patch = ""
		for _, part := range file.Hunks {
			if part.ID == hunk {
				patch = part.Patch
			}
		}
		if patch == "" {
			return errors.New("git_hunk_unknown")
		}
	}
	check := func() error {
		if e := validateScope(); e != nil {
			return e
		}
		latest, e := workspaceGitSnapshot(ctx, root)
		if e != nil {
			return e
		}
		if latest.Revision != state.Revision {
			return errors.New("git_conflict: 仓库已变化，请刷新后重新操作")
		}
		return validateScope()
	}
	if hunk == "" && (operation == "stage" || operation == "unstage") && scope != "last-turn" {
		if err = check(); err != nil {
			return err
		}
		args := []string{"add", "--", path}
		if operation == "unstage" {
			args = []string{"reset", "HEAD", "--", path}
			if state.Head == "" {
				args = []string{"rm", "--cached", "--", path}
			}
		}
		_, err = workspaceGitRun(ctx, root, "git", args...)
		return err
	}
	if file.Binary || file.Reason != "" || len(file.Hunks) == 0 {
		return errors.New("git_patch_unsupported: 此差异不能安全分块修改")
	}
	if (file.Added || file.Deleted) && hunk != "" && len(file.Hunks) > 1 {
		return errors.New("git_patch_lifecycle: 新增或删除文件请按整个文件处理")
	}
	if hunk == "" && scope == "last-turn" && !strings.Contains(patch, "--- ") {
		patch = "--- a/" + path + "\n+++ b/" + path + "\n" + patch
	}
	args := []string{}
	switch operation {
	case "stage":
		args = append(args, "--cached")
	case "unstage":
		args = append(args, "--cached", "--reverse")
	case "discard":
		args = append(args, "--reverse")
		if scope == "staged" {
			args = append(args, "--index")
		}
	}
	if err = gitApplyPatch(ctx, root, patch, append(args, "--check")...); err != nil {
		return err
	}
	if err = check(); err != nil {
		return err
	}
	return gitApplyPatch(ctx, root, patch, args...)
}
