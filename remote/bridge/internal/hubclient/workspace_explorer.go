package hubclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

var errExplorerRequest = errors.New("explorer_invalid: 目录请求无效，请重新选择磁盘和文件夹")
var errExplorerLocation = errors.New("explorer_unavailable: 无法访问这个位置，请选择可用的本地磁盘或真实文件夹")
var errExplorerPermission = errors.New("explorer_permission: 电脑当前用户没有这个目录的访问或创建权限，请选择其他位置")
var errExplorerLink = errors.New("explorer_link: 这个位置是链接或重定向目录，请从磁盘列表选择它的真实位置")
var errExplorerExists = errors.New("explorer_exists: 同名文件或文件夹已存在，请更换名称；不会覆盖已有内容")
var errExplorerUncertain = errors.New("explorer_uncertain: 文件夹可能已创建但尚未确认，请刷新父目录核对；不会自动重试创建")

type explorerEntry struct {
	Name          string `json:"name"`
	Path          string `json:"path"`
	Kind          string `json:"kind"`
	BlockedReason string `json:"blockedReason,omitempty"`
}

type explorerPage struct {
	Path       string          `json:"path"`
	Entries    []explorerEntry `json:"entries"`
	NextCursor string          `json:"nextCursor,omitempty"`
	Truncated  bool            `json:"truncated,omitempty"`
}

func explorerRelativePath(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !utf8.ValidString(value) || len(value) > 1024 || strings.ContainsAny(value, "\\:*?\"<>|\x00") {
		return "", errExplorerRequest
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." || len(part) > 255 || strings.TrimSpace(part) != part || strings.HasSuffix(part, ".") {
			return "", errExplorerRequest
		}
		for _, ch := range part {
			if unicode.IsControl(ch) || unicode.Is(unicode.Cf, ch) {
				return "", errExplorerRequest
			}
		}
		device := strings.ToLower(strings.SplitN(part, ".", 2)[0])
		if device == "con" || device == "prn" || device == "aux" || device == "nul" || device == "conin$" || device == "conout$" || len(device) == 4 && (strings.HasPrefix(device, "com") || strings.HasPrefix(device, "lpt")) && device[3] >= '1' && device[3] <= '9' {
			return "", errExplorerRequest
		}
	}
	return value, nil
}

func explorerIOError(err error) error {
	if errors.Is(err, os.ErrPermission) {
		return errExplorerPermission
	}
	return errExplorerLocation
}

func (c *Client) explorerGuard(ctx context.Context) error {
	if c.o.Store == nil || ctx.Err() != nil {
		return errExplorerLocation
	}
	if IsDirectExecution(ctx) {
		if err := validateDirectBinding(ctx, c.o.Store.Get()); err != nil {
			return err
		}
	} else if err := c.validateCommandGuard(ctx, "workspace.explorer"); err != nil {
		return err
	}
	return ctx.Err()
}

func sameExplorerAuthority(a, b config.Config) bool {
	if !sameProjectAuthorization(a, b) || a.ComputerID != b.ComputerID || a.ActiveRemoteMode != b.ActiveRemoteMode || len(a.ExplorerGrants) != len(b.ExplorerGrants) {
		return false
	}
	for i := range a.ExplorerGrants {
		if a.ExplorerGrants[i] != b.ExplorerGrants[i] {
			return false
		}
	}
	return true
}

// Unlike file-content APIs, the explorer is a directory-name picker. Opening
// a disk here grants no Agent or project access and never reads file contents.
func (c *Client) dispatchWorkspaceExplorer(ctx context.Context, cmd map[string]any) (any, error) {
	if !c.workspaceFilesMu.TryLock() {
		return nil, errors.New("explorer_busy: 目录正在读取，请稍后重试")
	}
	defer c.workspaceFilesMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" || c.o.Store == nil || str(cmd, "controlSurface") != "cli" {
		return nil, errExplorerRequest
	}
	for _, field := range []string{"type", "controlSurface", "tool", "workspaceMode", "action", "cwd", "path"} {
		if _, ok := cmd[field].(string); !ok {
			return nil, errExplorerRequest
		}
	}
	action, tool, mode := str(cmd, "action"), str(cmd, "tool"), str(cmd, "workspaceMode")
	if action != "list" && action != "mkdir" || mode != "code" && mode != "work" {
		return nil, errExplorerRequest
	}
	for field, value := range cmd {
		if field != "type" && field != "controlSurface" && field != "tool" && field != "workspaceMode" && field != "action" && field != "cwd" && field != "path" && (action != "list" || field != "cursor") && (action != "mkdir" || field != "name") {
			return nil, errExplorerRequest
		}
		if _, ok := value.(string); !ok {
			return nil, errExplorerRequest
		}
	}
	m := c.manager()
	if m == nil {
		return nil, errExplorerLocation
	}
	if err := requireAgentCapability(m.Get(tool), "read"); err != nil {
		return nil, err
	}
	rel, err := explorerRelativePath(str(cmd, "path"))
	if err != nil {
		return nil, err
	}
	requested, cursor, name := str(cmd, "cwd"), str(cmd, "cursor"), str(cmd, "name")
	if len(requested) > 4096 || !utf8.ValidString(requested) || len(cursor) > 512 {
		return nil, errExplorerRequest
	}
	if action == "mkdir" {
		if checked, e := explorerRelativePath(name); e != nil || checked == "" || path.Base(checked) != checked {
			return nil, errExplorerRequest
		}
	}
	c.taskConfigMu.Lock()
	err = c.explorerGuard(ctx)
	cfg := c.o.Store.Get()
	c.taskConfigMu.Unlock()
	if err != nil {
		return nil, err
	}
	roots, err := explorerRoots()
	if err != nil {
		return nil, err
	}
	reply := map[string]any{"tool": tool, "workspaceMode": mode, "cwd": requested, "path": rel}
	if requested == "" {
		_, hasCursor := cmd["cursor"]
		if action != "list" || rel != "" || hasCursor {
			return nil, errExplorerRequest
		}
		c.taskConfigMu.Lock()
		defer c.taskConfigMu.Unlock()
		if err = c.explorerGuard(ctx); err != nil {
			return nil, err
		}
		if !sameExplorerAuthority(cfg, c.o.Store.Get()) {
			return nil, errWorkspaceChanged
		}
		reply["roots"] = roots
		return reply, nil
	}
	rootPath := ""
	for _, candidate := range roots {
		if workspaceExpectedCwdMatches(candidate.Path, requested) {
			rootPath = candidate.Path
			break
		}
	}
	if rootPath == "" || !filepath.IsAbs(requested) {
		return nil, errExplorerLocation
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, explorerIOError(err)
	}
	defer root.Close()
	handle, err := root.Open(".")
	if err != nil {
		return nil, explorerIOError(err)
	}
	defer handle.Close()
	dir, err := explorerOpen(root, handle, rootPath, rel)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	var page explorerPage
	if action == "list" {
		page, err = explorerList(ctx, dir, rootPath, rel, cursor, durableHash([]string{tool, mode, projectlessOwner(cfg, projectlessSource(ctx)), c.eventScopeFromContext(ctx, cfg)}))
		if err != nil {
			return nil, err
		}
	}
	c.taskConfigMu.Lock()
	defer c.taskConfigMu.Unlock()
	if err = c.explorerGuard(ctx); err != nil {
		return nil, err
	}
	if !sameExplorerAuthority(cfg, c.o.Store.Get()) || explorerVerify(handle, rootPath) != nil || explorerVerify(dir, filepath.Join(rootPath, filepath.FromSlash(rel))) != nil {
		return nil, errWorkspaceChanged
	}
	if action == "list" {
		reply["directory"] = page
		return reply, nil
	}
	if len(cfg.Projects) >= 128 || len(cfg.ExplorerGrants) >= 4096 {
		return nil, errors.New("project_limit: 远程目录授权已满，请在电脑设置中整理后重试")
	}
	selectedPath := filepath.Join(rootPath, filepath.FromSlash(rel), name)
	if c.isManagedCwd(selectedPath) {
		return nil, errors.New("explorer_managed: 这里用于保存独立会话，请选择其他父目录")
	}
	parentName := filepath.FromSlash(rel)
	if parentName == "" {
		parentName = "."
	}
	parent, err := root.OpenRoot(parentName)
	if err != nil {
		return nil, explorerIOError(err)
	}
	defer parent.Close()
	parentHandle, err := parent.Open(".")
	if err != nil {
		return nil, explorerIOError(err)
	}
	defer parentHandle.Close()
	parentPath := filepath.Dir(selectedPath)
	if explorerVerify(parentHandle, parentPath) != nil || !explorerSameFile(dir, parentHandle) {
		return nil, errWorkspaceChanged
	}
	if err = parent.Mkdir(name, 0700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, errExplorerExists
		}
		return nil, explorerIOError(err)
	}
	// From this point, do not claim an atomic rollback and never delete or
	// adopt the directory on retry. It may already contain user-owned work.
	created, err := parent.Open(name)
	if err != nil {
		return nil, errExplorerUncertain
	}
	defer created.Close()
	identity, err := projectlessDirectoryIdentity(created)
	if err != nil || explorerVerify(created, selectedPath) != nil || explorerVerify(parentHandle, parentPath) != nil || explorerVerify(handle, rootPath) != nil || c.explorerGuard(ctx) != nil || !sameExplorerAuthority(cfg, c.o.Store.Get()) {
		return nil, errExplorerUncertain
	}
	selected := protocol.Project{Path: selectedPath, Name: config.ProjectName(selectedPath)}
	err = c.o.Store.Update(func(latest *config.Config) error {
		if ctx.Err() != nil || !sameExplorerAuthority(cfg, *latest) || explorerVerify(created, selectedPath) != nil {
			return errWorkspaceChanged
		}
		grants := make([]config.ExplorerGrant, 0, len(latest.ExplorerGrants)+1)
		for _, grant := range latest.ExplorerGrants {
			if !workspaceExpectedCwdMatches(grant.Path, selectedPath) {
				grants = append(grants, grant)
			}
		}
		latest.ExplorerGrants = append(grants, config.ExplorerGrant{Path: selectedPath, DirectoryID: identity})
		latest.Projects = append(latest.Projects, selected)
		return nil
	})
	if err != nil || c.explorerGuard(ctx) != nil || explorerVerify(created, selectedPath) != nil {
		return nil, errExplorerUncertain
	}
	c.Reregister()
	reply["project"] = selected
	return reply, nil
}

func explorerSameFile(a, b *os.File) bool {
	x, e := a.Stat()
	y, f := b.Stat()
	return e == nil && f == nil && os.SameFile(x, y)
}

// Mounts may be traversed only when the destination is a supported local
// filesystem. Each final handle is compared to its exact visible path; links
// are never treated as a route to another root.
func explorerVerify(file *os.File, expected string) error {
	if !explorerLocalPath(expected) || workspaceVerifyHandle(file, file, expected, "", false) != nil {
		return errExplorerLocation
	}
	current, err := os.Lstat(expected)
	opened, e := file.Stat()
	if err != nil || e != nil || !opened.IsDir() || !os.SameFile(current, opened) || explorerIsLink(current) {
		return errExplorerLocation
	}
	return nil
}

func explorerOpen(root *os.Root, rootHandle *os.File, rootPath, rel string) (*os.File, error) {
	if explorerVerify(rootHandle, rootPath) != nil {
		return nil, errExplorerLocation
	}
	prefix := ""
	for _, component := range strings.Split(rel, "/") {
		if component == "" {
			continue
		}
		prefix = filepath.Join(prefix, component)
		if !explorerLocalPath(filepath.Join(rootPath, prefix)) {
			return nil, errExplorerLocation
		}
		st, err := root.Lstat(prefix)
		if err != nil {
			return nil, explorerIOError(err)
		}
		if explorerIsLink(st) {
			return nil, errExplorerLink
		}
		if !st.IsDir() {
			return nil, errExplorerLocation
		}
	}
	name := filepath.FromSlash(rel)
	if name == "" {
		name = "."
	}
	f, err := workspaceRootReadFile(root, name)
	if err != nil {
		return nil, explorerIOError(err)
	}
	if explorerVerify(f, filepath.Join(rootPath, name)) != nil || explorerVerify(rootHandle, rootPath) != nil {
		f.Close()
		return nil, errWorkspaceChanged
	}
	return f, nil
}

func explorerList(ctx context.Context, dir *os.File, rootPath, rel, cursor, owner string) (explorerPage, error) {
	page := explorerPage{Path: rel, Entries: []explorerEntry{}}
	before, err := dir.Stat()
	if err != nil {
		return page, explorerIOError(err)
	}
	identity, err := projectlessDirectoryIdentity(dir)
	if err != nil {
		return page, errExplorerLocation
	}
	scope := workspaceFileScope{root: rootPath, key: owner + ":" + identity, surface: "explorer"}
	revision := workspaceDirectoryRevision(scope, rel, before)
	position := workspaceDirectoryCursor{Version: 1, Revision: revision}
	if cursor != "" {
		position, err = workspaceDecodeCursor(cursor, revision)
		if err != nil {
			return page, errWorkspaceChanged
		}
	}
	for i := 0; i < position.Offset; i++ {
		if ctx.Err() != nil {
			return page, errWorkspaceChanged
		}
		if _, err = dir.ReadDir(1); err != nil {
			return page, errWorkspaceChanged
		}
	}
	eof := false
	for position.Offset < workspaceListScan && len(page.Entries) < workspaceListPage {
		if ctx.Err() != nil {
			return page, errWorkspaceChanged
		}
		entries, e := dir.ReadDir(1)
		if e == io.EOF {
			eof = true
			break
		}
		if e != nil || len(entries) != 1 {
			return page, explorerIOError(e)
		}
		position.Offset++
		entry := entries[0]
		child := path.Join(rel, entry.Name())
		if _, e = explorerRelativePath(child); e != nil {
			continue
		}
		if !entry.IsDir() && entry.Type()&(os.ModeSymlink|os.ModeIrregular) == 0 {
			continue
		}
		if !explorerLocalPath(filepath.Join(rootPath, filepath.FromSlash(child))) {
			page.Entries = append(page.Entries, explorerEntry{Name: entry.Name(), Path: child, Kind: "directory", BlockedReason: "这个位置不是可访问的本地文件系统"})
			continue
		}
		info, e := entry.Info() // lstat metadata only; never opens the target.
		if e != nil || !info.IsDir() && !explorerIsLink(info) {
			continue
		}
		item := explorerEntry{Name: entry.Name(), Path: child, Kind: "directory"}
		if explorerIsLink(info) {
			item.BlockedReason = "链接目录，请选择它的真实位置"
		} else if !explorerLocalPath(filepath.Join(rootPath, filepath.FromSlash(child))) {
			item.BlockedReason = "这个位置不是可访问的本地文件系统"
		}
		page.Entries = append(page.Entries, item)
	}
	if !eof {
		if _, err = dir.ReadDir(1); err == io.EOF {
			eof = true
		} else if err != nil {
			return page, explorerIOError(err)
		}
	}
	after, err := dir.Stat()
	if err != nil || revision != workspaceDirectoryRevision(scope, rel, after) || explorerVerify(dir, filepath.Join(rootPath, filepath.FromSlash(rel))) != nil {
		return page, errWorkspaceChanged
	}
	if !eof && position.Offset < workspaceListScan {
		raw, _ := json.Marshal(position)
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	page.Truncated = !eof && position.Offset >= workspaceListScan
	return page, nil
}

// A saved exact project inside an explorer-created subtree may retain that
// subtree grant. The containing disk, siblings and unrelated system folders
// remain ungranted. Every use checks the originally created directory identity.
func explorerGrantForCwd(cfg config.Config, cwd string) (bool, error) {
	granted := false
	for _, grant := range cfg.ExplorerGrants {
		if !filepath.IsAbs(grant.Path) || !IsWithin(grant.Path, cwd, runtime.GOOS == "windows") {
			continue
		}
		active := false
		for _, project := range cfg.Projects {
			if workspaceExpectedCwdMatches(project.Path, grant.Path) {
				active = true
				break
			}
		}
		if !active {
			return false, errWorkspaceFiles
		}
		if !explorerLocalPath(grant.Path) || !explorerLocalPath(cwd) {
			return false, errWorkspaceFiles
		}
		f, err := explorerOpenAbsolute(grant.Path)
		if err != nil {
			return false, errWorkspaceFiles
		}
		id, e := projectlessDirectoryIdentity(f)
		valid := e == nil && id == grant.DirectoryID && explorerVerify(f, grant.Path) == nil
		f.Close()
		if !valid || !sameCwdAncestor(grant.Path, cwd) {
			return false, errWorkspaceFiles
		}
		granted = true
	}
	return granted, nil
}

func explorerOpenAbsolute(value string) (*os.File, error) {
	roots, err := explorerRoots()
	if err != nil {
		return nil, err
	}
	base := ""
	for _, candidate := range roots {
		if len(candidate.Path) > len(base) && IsWithin(candidate.Path, value, runtime.GOOS == "windows") {
			base = candidate.Path
		}
	}
	if base == "" {
		return nil, errExplorerLocation
	}
	rel, err := filepath.Rel(base, value)
	if err != nil {
		return nil, errExplorerLocation
	}
	if rel == "." {
		rel = ""
	}
	if _, err = explorerRelativePath(filepath.ToSlash(rel)); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, explorerIOError(err)
	}
	defer root.Close()
	handle, err := root.Open(".")
	if err != nil {
		return nil, explorerIOError(err)
	}
	defer handle.Close()
	return explorerOpen(root, handle, base, filepath.ToSlash(rel))
}

func (c *Client) validateExplorerSession(ctx context.Context, cfg config.Config, key, surface string) error {
	if key == "" || surface != "cli" || len(cfg.ExplorerGrants) == 0 {
		return nil
	}
	cwd, err := c.workspaceSessionCwd(ctx, key, "cli")
	if err != nil {
		return err
	}
	_, err = explorerGrantForCwd(cfg, cwd)
	return err
}
