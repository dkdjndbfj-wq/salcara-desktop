package hubclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"salcara/bridge/internal/config"
)

type workspaceGitFile struct {
	Path     string `json:"path"`
	Original string `json:"original,omitempty"`
	Index    string `json:"index"`
	Worktree string `json:"worktree"`
}
type workspaceGitWorktree struct {
	Path    string `json:"path"`
	Branch  string `json:"branch"`
	Managed bool   `json:"managed"`
}
type workspaceGitState struct {
	NestedIndexChanges bool                    `json:"-"`
	NestedChanges      bool                    `json:"-"`
	Repository         *workspaceGitRepository `json:"repository,omitempty"`
	Revision           string                  `json:"revision"`
	Branch             string                  `json:"branch"`
	Head               string                  `json:"head"`
	Upstream           string                  `json:"upstream"`
	Files              []workspaceGitFile      `json:"files"`
	Branches           []string                `json:"branches"`
	Worktrees          []workspaceGitWorktree  `json:"worktrees"`
	GitHubCLI          bool                    `json:"githubCLI"`
}

// Bound output before returning it to the phone. Failed git/credential helper
// stderr can contain secrets and is intentionally never returned verbatim.
type gitOutput struct {
	buffer   bytes.Buffer
	overflow bool
}

func (b *gitOutput) Write(p []byte) (int, error) {
	n := len(p)
	if b.buffer.Len()+n > 1024*1024 {
		b.overflow = true
		return n, nil
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}
func workspaceGitRun(ctx context.Context, root, exe string, args ...string) (string, error) {
	if exe == "git" {
		args = append([]string{"--no-pager", "--literal-pathspecs", "-c", "core.quotepath=false", "-c", "credential.interactive=false", "-C", root}, args...)
	}
	command := exec.CommandContext(ctx, exe, args...)
	command.Dir = root
	command.Env = append(terminalEnvironment(), "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=Never", "GIT_OPTIONAL_LOCKS=0", "GH_PROMPT_DISABLED=1", "NO_COLOR=1")
	hidePreviewProcess(command)
	var output gitOutput
	command.Stdout = &output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return "", errors.New("git_failed: 操作未完成，请在电脑检查仓库、登录凭据或冲突，然后刷新状态")
	}
	if output.overflow {
		return "", errors.New("git_output_limit: 内容过大，请在电脑查看")
	}
	return output.buffer.String(), nil
}
func gitSafePath(value string) bool {
	if value == "" || len(value) > 4096 || strings.ContainsAny(value, "\\:") || strings.HasPrefix(value, "/") {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}
func gitManagedPath(root, candidate string) bool {
	relative, err := filepath.Rel(filepath.Join(root, ".salcara-worktrees"), candidate)
	return err == nil && regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`).MatchString(relative)
}
func gitOwnedWorktree(root, candidate string) bool {
	if !gitManagedPath(root, candidate) {
		return false
	}
	opened, err := os.OpenRoot(root)
	if err != nil {
		return false
	}
	defer opened.Close()
	data, err := opened.ReadFile(filepath.Join(".salcara-worktrees", ".managed-"+filepath.Base(candidate)))
	return err == nil && workspaceSamePath(string(data), candidate)
}
func gitBranchValid(ctx context.Context, root, branch string) bool {
	if branch == "" || len(branch) > 200 || strings.HasPrefix(branch, "-") || strings.Contains(branch, "@{") {
		return false
	}
	_, err := workspaceGitRun(ctx, root, "git", "check-ref-format", "--branch", branch)
	return err == nil
}
func workspaceGitSnapshot(ctx context.Context, root string) (workspaceGitState, error) {
	state := workspaceGitState{Files: []workspaceGitFile{}, Branches: []string{}, Worktrees: []workspaceGitWorktree{}}
	top, err := workspaceGitRun(ctx, root, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return state, errors.New("git_not_repository: 当前授权项目不是 Git 仓库")
	}
	if !workspaceSamePath(root, strings.TrimSpace(top)) {
		return state, errors.New("git_root_mismatch: 请在电脑授权仓库根目录")
	}
	head, _ := workspaceGitRun(ctx, root, "git", "rev-parse", "--verify", "HEAD")
	state.Head = strings.TrimSpace(head)
	branch, _ := workspaceGitRun(ctx, root, "git", "symbolic-ref", "--quiet", "--short", "HEAD")
	state.Branch = strings.TrimSpace(branch)
	upstream, _ := workspaceGitRun(ctx, root, "git", "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	state.Upstream = strings.TrimSpace(upstream)
	status, err := workspaceGitRun(ctx, root, "git", "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return state, err
	}
	entries := strings.Split(status, "\x00")
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		if entry == "" {
			continue
		}
		if len(entry) < 4 {
			return state, errors.New("git_status_invalid")
		}
		f := workspaceGitFile{Path: entry[3:], Index: entry[:1], Worktree: entry[1:2]}
		if f.Index == "R" || f.Index == "C" {
			i++
			if i >= len(entries) {
				return state, errors.New("git_status_invalid")
			}
			f.Original = entries[i]
		}
		// These are separately managed repositories, never stage them in the parent.
		if f.Path == ".salcara-worktrees" || strings.HasPrefix(f.Path, ".salcara-worktrees/") {
			continue
		}
		if gitSafePath(strings.TrimSuffix(f.Path, "/")) && gitCrossesRepository(root, f.Path) {
			if f.Index != "?" {
				state.NestedChanges = true
				if f.Index != " " {
					state.NestedIndexChanges = true
				}
			}
			continue
		}
		if !gitSafePath(f.Path) || f.Original != "" && !gitSafePath(f.Original) {
			return state, errors.New("git_path_unsupported: 仓库包含无法安全操作的文件名")
		}
		state.Files = append(state.Files, f)
		if len(state.Files) > 1000 {
			return state, errors.New("git_status_limit: 改动超过 1000 个文件，请在电脑处理")
		}
	}
	refs, err := workspaceGitRun(ctx, root, "git", "for-each-ref", "--format=%(refname:short) %(objectname)", "refs/heads")
	if err != nil {
		return state, err
	}
	for _, line := range strings.Split(strings.TrimSpace(refs), "\n") {
		name, _, ok := strings.Cut(line, " ")
		if ok {
			state.Branches = append(state.Branches, name)
		}
	}
	index, err := workspaceGitRun(ctx, root, "git", "ls-files", "--stage", "-z")
	if err != nil {
		return state, err
	}
	trees, err := workspaceGitRun(ctx, root, "git", "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return state, err
	}
	var tree *workspaceGitWorktree
	for _, entry := range strings.Split(trees, "\x00") {
		if strings.HasPrefix(entry, "worktree ") {
			state.Worktrees = append(state.Worktrees, workspaceGitWorktree{Path: strings.TrimPrefix(entry, "worktree ")})
			tree = &state.Worktrees[len(state.Worktrees)-1]
			tree.Managed = gitOwnedWorktree(root, tree.Path)
		} else if tree != nil && strings.HasPrefix(entry, "branch ") {
			tree.Branch = strings.TrimPrefix(entry, "branch refs/heads/")
		}
	}
	h := sha256.New()
	fmt.Fprint(h, gitRepositoryID(root), "\x00", state.Head, "\x00", state.Branch, "\x00", state.Upstream, "\x00", status, "\x00", refs, "\x00", index, "\x00", trees)
	opened, err := os.OpenRoot(root)
	if err != nil {
		return state, err
	}
	defer opened.Close()
	total := int64(0)
	for _, f := range state.Files {
		info, e := opened.Lstat(f.Path)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return state, e
		}
		fmt.Fprint(h, f.Path, "\x00", info.Mode(), "\x00")
		if info.Mode()&os.ModeSymlink != 0 {
			target, e := opened.Readlink(f.Path)
			if e != nil {
				return state, e
			}
			fmt.Fprint(h, target)
			continue
		}
		if !info.Mode().IsRegular() {
			return state, errors.New("git_file_unsupported")
		}
		total += info.Size()
		if info.Size() > 32*1024*1024 || total > 64*1024*1024 {
			return state, errors.New("git_content_limit: 大文件请在电脑处理")
		}
		file, e := opened.Open(f.Path)
		if e != nil {
			return state, e
		}
		_, e = io.Copy(h, io.LimitReader(file, 32*1024*1024+1))
		file.Close()
		if e != nil {
			return state, e
		}
	}
	state.Revision = hex.EncodeToString(h.Sum(nil))
	_, err = exec.LookPath("gh")
	state.GitHubCLI = err == nil
	return state, nil
}

func (c *Client) dispatchWorkspaceGit(ctx context.Context, cmd map[string]any) (any, error) {
	if !c.workspaceFilesMu.TryLock() {
		return nil, errors.New("git_busy: 另一个文件或 Git 操作正在进行")
	}
	defer c.workspaceFilesMu.Unlock()
	typ := str(cmd, "type")
	if typ != "workspace.git.read" && typ != "workspace.git.mutate" || c.o.Store == nil {
		return nil, errors.New("git_command_invalid")
	}
	allowed := map[string]bool{"type": true, "sessionKey": true, "controlSurface": true, "expectedCwd": true, "action": true, "path": true, "expectedRevision": true, "branch": true, "message": true, "name": true, "title": true, "body": true,
		"reviewScope": true, "reviewRef": true, "sourceDiff": true, "patchRevision": true, "hunk": true, "operation": true, "repositoryPath": true}
	for k, v := range cmd {
		if !allowed[k] {
			return nil, errors.New("git_command_invalid")
		}
		s, ok := v.(string)
		limit := 16384
		if k == "sourceDiff" {
			limit = 192 * 1024
		}
		if !ok || len(s) > limit {
			return nil, errors.New("git_command_invalid")
		}
	}
	key, surface, expected, action := str(cmd, "sessionKey"), str(cmd, "controlSurface"), str(cmd, "expectedCwd"), str(cmd, "action")
	if key == "" || len(key) > 256 || (surface != "cli" && surface != "desktop") {
		return nil, errWorkspaceFiles
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cwd, err := c.workspaceSessionCwd(ctx, key, surface)
	if err != nil || !workspaceExpectedCwdMatches(expected, cwd) {
		return nil, errWorkspaceChanged
	}
	c.taskConfigMu.Lock()
	defer c.taskConfigMu.Unlock()
	if err = c.validateCommandGuard(ctx, typ); err != nil {
		return nil, err
	}
	cfg := c.o.Store.Get()
	root, err := c.workspaceSessionRoot(ctx, cfg, key, surface, cwd)
	if err != nil {
		return nil, err
	}
	rootHandle, err := os.Open(root)
	if err != nil {
		return nil, err
	}
	defer rootHandle.Close()
	if workspaceVerifyHandle(rootHandle, rootHandle, root, "", false) != nil {
		return nil, errWorkspaceChanged
	}
	workspaceRoot := root
	repositoryPath := str(cmd, "repositoryPath")
	var repositoryHandle *os.File
	// Snapshotting runs several child processes and hashes up to 64 MiB. Its
	// initial authorization cannot authorize a later mutation. Resolve the exact
	// live session and configured project again at each mutation boundary.
	validateScope := func() error {
		currentCwd, e := c.workspaceSessionCwd(ctx, key, surface)
		if e != nil || !workspaceExpectedCwdMatches(expected, currentCwd) || !workspaceExpectedCwdMatches(cwd, currentCwd) || ctx.Err() != nil {
			return errWorkspaceChanged
		}
		current := c.o.Store.Get()
		currentRoot, e := c.workspaceSessionRoot(ctx, current, key, surface, currentCwd)
		if e != nil || !workspaceSamePath(currentRoot, workspaceRoot) || config.RemoteIdentity(current) != config.RemoteIdentity(cfg) || current.DeviceID != cfg.DeviceID || current.PhoneBindingID != cfg.PhoneBindingID || current.PhoneHash != cfg.PhoneHash {
			return errWorkspaceChanged
		}
		currentInfo, statErr := os.Lstat(currentRoot)
		openedInfo, openedErr := rootHandle.Stat()
		if statErr != nil || openedErr != nil || !os.SameFile(currentInfo, openedInfo) || workspaceVerifyHandle(rootHandle, rootHandle, workspaceRoot, "", false) != nil {
			return errWorkspaceChanged
		}
		if repositoryHandle != nil {
			currentRepo, e := os.Lstat(root)
			openedRepo, openedErr := repositoryHandle.Stat()
			if e != nil || openedErr != nil || !os.SameFile(currentRepo, openedRepo) || workspaceVerifyHandle(rootHandle, repositoryHandle, workspaceRoot, repositoryPath, false) != nil {
				return errWorkspaceChanged
			}
			if _, _, e = workspaceGitRepositoryRoot(ctx, workspaceRoot, rootHandle, repositoryPath); e != nil {
				return errWorkspaceChanged
			}
		}
		return c.validateCommandGuard(ctx, typ)
	}
	if typ == "workspace.git.read" && action == "repositories" {
		if repositoryPath != "" {
			return nil, errors.New("git_repository_path_invalid")
		}
		discovery, e := workspaceDiscoverGitRepositories(ctx, workspaceRoot, rootHandle)
		if e != nil {
			return nil, e
		}
		if e = validateScope(); e != nil {
			return nil, e
		}
		return discovery, nil
	}
	root, repository, err := workspaceGitRepositoryRoot(ctx, workspaceRoot, rootHandle, repositoryPath)
	if err != nil {
		return nil, err
	}
	repositoryHandle, err = os.Open(root)
	if err != nil {
		return nil, err
	}
	defer repositoryHandle.Close()
	if err = validateScope(); err != nil {
		return nil, err
	}
	state, err := workspaceGitSnapshot(ctx, root)
	if err != nil {
		return nil, err
	}
	state.Repository = &repository
	var result any = state
	if typ == "workspace.git.read" {
		if action != "status" && action != "diff" && action != "staged-diff" && action != "review" {
			return nil, errors.New("git_read_invalid")
		}
		if action == "review" {
			result, err = workspaceGitReview(ctx, root, state, cmd)
			if err != nil {
				return nil, err
			}
		} else if action != "status" {
			p := str(cmd, "path")
			if !gitSafePath(p) || gitCrossesRepository(root, p) {
				return nil, errors.New("git_path_invalid")
			}
			args := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color"}
			if action == "staged-diff" {
				args = append(args, "--cached")
			}
			args = append(args, "--", p)
			diff, e := workspaceGitRun(ctx, root, "git", args...)
			if e != nil {
				return nil, e
			}
			result = map[string]any{"path": p, "diff": diff, "revision": state.Revision, "repository": repository}
		}
	} else {
		if str(cmd, "expectedRevision") != state.Revision {
			return nil, errors.New("git_conflict: 仓库已变化，请刷新后重新操作")
		}
		extra := map[string]any{}
		run := func(args ...string) error {
			if e := validateScope(); e != nil {
				return e
			}
			_, e := workspaceGitRun(ctx, root, "git", args...)
			return e
		}
		switch action {
		case "review-apply":
			err = workspaceGitReviewApply(ctx, root, state, cmd, validateScope)
		case "stage", "unstage":
			p := str(cmd, "path")
			var file *workspaceGitFile
			for i := range state.Files {
				if state.Files[i].Path == p {
					file = &state.Files[i]
					break
				}
			}
			if file == nil {
				return nil, errors.New("git_path_not_changed")
			}
			paths := []string{p}
			if file.Original != "" {
				paths = append(paths, file.Original)
			}
			if action == "stage" {
				err = run(append([]string{"add", "--"}, paths...)...)
			} else if state.Head == "" {
				err = run(append([]string{"rm", "--cached", "--"}, paths...)...)
			} else {
				err = run(append([]string{"reset", "HEAD", "--"}, paths...)...)
			}
		case "commit":
			if state.NestedIndexChanges {
				return nil, errors.New("git_nested_index: 暂存区包含嵌套仓库引用，请在电脑核对后提交")
			}
			message := str(cmd, "message")
			if strings.TrimSpace(message) == "" || len(message) > 4000 || strings.ContainsRune(message, 0) {
				return nil, errors.New("git_commit_message_required")
			}
			staged := false
			for _, f := range state.Files {
				if f.Index != " " && f.Index != "?" {
					staged = true
				}
			}
			if !staged {
				return nil, errors.New("git_nothing_staged: 请先暂存需要提交的文件")
			}
			err = run("commit", "-m", message)
		case "branch", "checkout":
			branch := str(cmd, "branch")
			if !gitBranchValid(ctx, root, branch) {
				return nil, errors.New("git_branch_invalid")
			}
			if action == "branch" {
				err = run("branch", branch)
			} else {
				if len(state.Files) > 0 || state.NestedChanges {
					return nil, errors.New("git_dirty: 请先提交或处理当前改动")
				}
				found := false
				for _, b := range state.Branches {
					if b == branch {
						found = true
					}
				}
				if !found {
					return nil, errors.New("git_branch_unknown")
				}
				err = run("switch", branch)
			}
		case "push":
			if state.Branch == "" || state.Upstream == "" {
				return nil, errors.New("git_no_upstream: 请先在电脑为该分支设置远程跟踪分支")
			}
			target, e := workspaceGitRun(ctx, root, "git", "for-each-ref", "--format=%(upstream:remotename)%00%(upstream:remoteref)", "refs/heads/"+state.Branch)
			remote, ref, ok := strings.Cut(strings.TrimSpace(target), "\x00")
			if e != nil || !ok || remote == "" || strings.HasPrefix(remote, "-") || strings.ContainsAny(remote, "\r\n\x00") || !strings.HasPrefix(ref, "refs/heads/") {
				return nil, errors.New("git_upstream_invalid")
			}
			err = run("push", "--", remote, "HEAD:"+ref)
		case "pr":
			if !state.GitHubCLI || state.Branch == "" || state.Upstream == "" {
				return nil, errors.New("git_pr_unavailable: 请先在电脑安装并登录 gh，再推送当前分支")
			}
			pushed, e := workspaceGitRun(ctx, root, "git", "rev-parse", "@{upstream}")
			if e != nil || strings.TrimSpace(pushed) != state.Head {
				return nil, errors.New("git_pr_push_required: 请先推送当前提交")
			}
			existing, e := workspaceGitRun(ctx, root, "gh", "pr", "list", "--head", state.Branch, "--state", "open", "--json", "url", "--limit", "1")
			if e != nil {
				return nil, errors.New("git_pr_lookup_failed: 无法确认已有 PR，请在电脑检查 gh 登录与网络")
			}
			var matches []struct {
				URL string `json:"url"`
			}
			if json.Unmarshal([]byte(existing), &matches) != nil || len(matches) > 1 {
				return nil, errors.New("git_pr_lookup_invalid")
			}
			if len(matches) == 1 {
				extra["url"] = matches[0].URL
			}
			if extra["url"] == nil {
				title := str(cmd, "title")
				if strings.TrimSpace(title) == "" || len(title) > 200 {
					return nil, errors.New("git_pr_title_required")
				}
				if e := validateScope(); e != nil {
					return nil, e
				}
				link, e := workspaceGitRun(ctx, root, "gh", "pr", "create", "--draft", "--head", state.Branch, "--title", title, "--body", str(cmd, "body"))
				err = e
				extra["url"] = strings.TrimSpace(link)
			}
			if err == nil {
				link, _ := extra["url"].(string)
				u, e := url.Parse(link)
				if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || !strings.Contains(u.Path, "/pull/") {
					return nil, errors.New("git_pr_result_unknown: 请在电脑核对 PR，勿重复创建")
				}
			}
		case "worktree-create":
			name := str(cmd, "name")
			dest := filepath.Join(root, ".salcara-worktrees", name)
			branch := str(cmd, "branch")
			if !gitManagedPath(root, dest) || !gitBranchValid(ctx, root, branch) {
				return nil, errors.New("git_worktree_invalid")
			}
			parent := filepath.Dir(dest)
			if info, e := os.Lstat(parent); e == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
				return nil, errors.New("git_worktree_invalid")
			}
			if _, e := os.Lstat(dest); !os.IsNotExist(e) {
				return nil, errors.New("git_worktree_exists")
			}
			if e := validateScope(); e != nil {
				return nil, e
			}
			if err = os.MkdirAll(parent, 0700); err == nil {
				err = run("worktree", "add", "-b", branch, "--", dest, "HEAD")
			}
			if err == nil {
				if e := validateScope(); e != nil {
					return nil, e
				}
				opened, e := os.OpenRoot(root)
				if e != nil {
					return nil, e
				}
				e = opened.WriteFile(filepath.Join(".salcara-worktrees", ".managed-"+name), []byte(dest), 0600)
				opened.Close()
				if e != nil {
					return nil, errors.New("git_worktree_created_unmanaged: 工作树已创建，管理标记未写入，请在电脑核对")
				}
				extra["path"] = dest
			}
		case "worktree-remove":
			target := str(cmd, "path")
			found := false
			for _, tree := range state.Worktrees {
				if tree.Managed && workspaceSamePath(tree.Path, target) {
					found = true
				}
			}
			if !found || !gitOwnedWorktree(root, target) {
				return nil, errors.New("git_worktree_not_managed")
			}
			if info, e := os.Lstat(target); e != nil || info.Mode()&os.ModeSymlink != 0 {
				return nil, errors.New("git_worktree_invalid")
			}
			err = run("worktree", "remove", "--", target) // deliberately no --force; Git refuses dirty/locked trees
			if err == nil {
				if e := validateScope(); e != nil {
					return nil, e
				}
				opened, e := os.OpenRoot(root)
				if e == nil {
					_ = opened.Remove(filepath.Join(".salcara-worktrees", ".managed-"+filepath.Base(target)))
					opened.Close()
				}
			}
		default:
			return nil, errors.New("git_action_invalid")
		}
		if err != nil {
			return nil, err
		}
		updated, e := workspaceGitSnapshot(ctx, root)
		if e != nil {
			return nil, errors.New("git_result_unknown: 操作可能已完成，请刷新核对，勿直接重试")
		}
		updated.Repository = &repository
		extra["state"] = updated
		extra["action"] = action
		result = extra
	}
	if err = validateScope(); err != nil {
		return nil, err
	}
	return result, nil
}
