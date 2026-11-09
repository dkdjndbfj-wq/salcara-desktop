package hubclient

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

var errProjectAuthorization = errors.New("project_not_authorized: 请在电脑设置的远程目录授权中允许这个目录，再重新读取；不会改用其他目录")
var errProjectUnavailable = errors.New("project_unavailable: 目录不可用，请在电脑恢复目录后重试")

// A phone can browse only explicitly granted roots. Selecting a child persists
// that exact project without widening the root grant. No session is started.
func (c *Client) dispatchWorkspaceProjects(ctx context.Context, cmd map[string]any) (any, error) {
	if !c.workspaceFilesMu.TryLock() {
		return nil, errors.New("project_busy: 目录正在读取，请稍后重试")
	}
	defer c.workspaceFilesMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if c.o.Store == nil || str(cmd, "controlSurface") != "cli" {
		return nil, errProjectAuthorization
	}
	for _, key := range []string{"type", "controlSurface", "tool", "workspaceMode", "action", "cwd", "path"} {
		if _, ok := cmd[key].(string); !ok {
			return nil, errProjectAuthorization
		}
	}
	action, tool, mode := str(cmd, "action"), str(cmd, "tool"), str(cmd, "workspaceMode")
	if action != "list" && action != "select" && action != "mkdir" || mode != "code" && mode != "work" {
		return nil, errProjectAuthorization
	}
	for key, value := range cmd {
		if key != "type" && key != "controlSurface" && key != "action" && key != "tool" && key != "workspaceMode" && key != "cwd" && key != "path" && key != "cursor" && (action != "mkdir" || key != "name") {
			return nil, errProjectAuthorization
		}
		if _, ok := value.(string); !ok {
			return nil, errProjectAuthorization
		}
	}
	m := c.manager()
	if m == nil {
		return nil, errProjectUnavailable
	}
	if err := requireAgentCapability(m.Get(tool), "read"); err != nil {
		return nil, err
	}
	rel, err := projectRelativePath(str(cmd, "path"))
	if err != nil {
		return nil, errProjectAuthorization
	}
	requested, cursor, name := str(cmd, "cwd"), str(cmd, "cursor"), str(cmd, "name")
	if len(requested) > 4096 || !utf8.ValidString(requested) || action != "list" && cursor != "" {
		return nil, errProjectAuthorization
	}
	if action == "mkdir" {
		if checked, e := projectRelativePath(name); e != nil || checked == "" || path.Base(checked) != checked {
			return nil, errProjectAuthorization
		}
	}
	c.taskConfigMu.Lock()
	err = c.validateProjectGuard(ctx)
	cfg := c.o.Store.Get()
	c.taskConfigMu.Unlock()
	if err != nil {
		return nil, err
	}
	reply := map[string]any{"tool": tool, "workspaceMode": mode, "cwd": requested, "path": rel}
	if requested == "" {
		if action != "list" || rel != "" || cursor != "" {
			return nil, errProjectAuthorization
		}
		roots := []protocol.Project{}
		for _, project := range cfg.Projects {
			if _, e := workspaceConfiguredRoot(cfg, project.Path); e == nil {
				roots = append(roots, project)
			}
			if len(roots) >= 128 {
				break
			}
		}
		c.taskConfigMu.Lock()
		defer c.taskConfigMu.Unlock()
		if err := c.validateProjectGuard(ctx); err != nil {
			return nil, err
		}
		if ctx.Err() != nil || !sameProjectAuthorization(cfg, c.o.Store.Get()) {
			return nil, errWorkspaceChanged
		}
		reply["roots"] = roots
		return reply, nil
	}
	cwd, err := workspaceProjectCwd(cfg, requested)
	if err != nil {
		return nil, errProjectAuthorization
	}
	rootPath, err := workspaceConfiguredRoot(cfg, cwd)
	if err != nil {
		return nil, errProjectUnavailable
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, errProjectUnavailable
	}
	defer root.Close()
	handle, err := root.Open(".")
	if err != nil {
		return nil, errProjectUnavailable
	}
	defer handle.Close()
	dir, err := workspaceOpen(root, handle, rootPath, rel, false)
	if err != nil {
		return nil, errProjectUnavailable
	}
	defer dir.Close()
	scope := workspaceFileScope{"project:" + tool + ":" + mode, "cli", rootPath, config.RemoteIdentity(cfg), cfg.DeviceID, cfg.PhoneBindingID, cfg.PhoneHash}
	var page workspaceFilesPage
	if action == "list" {
		page, err = workspaceListFiltered(ctx, root, handle, scope, rel, cursor, func(child string, entry os.DirEntry) bool {
			_, err := projectRelativePath(child)
			return err == nil && entry.IsDir()
		})
		if err != nil {
			return nil, err
		}
	}
	c.taskConfigMu.Lock()
	defer c.taskConfigMu.Unlock()
	if err := c.validateProjectGuard(ctx); err != nil {
		return nil, err
	}
	current := c.o.Store.Get()
	if ctx.Err() != nil || !sameProjectAuthorization(cfg, current) {
		return nil, errWorkspaceChanged
	}
	currentRoot, e := workspaceConfiguredRoot(current, cwd)
	if e != nil || !workspaceSamePath(currentRoot, rootPath) || workspaceVerifyHandle(handle, handle, rootPath, "", false) != nil || workspaceVerifyHandle(handle, dir, rootPath, rel, false) != nil {
		return nil, errWorkspaceChanged
	}
	if action == "list" {
		reply["directory"] = page
		return reply, nil
	}
	if action == "mkdir" {
		parentName := rel
		if parentName == "" {
			parentName = "."
		}
		parent, e := root.OpenRoot(filepath.FromSlash(parentName))
		if e != nil {
			return nil, errProjectUnavailable
		}
		defer parent.Close()
		parentHandle, e := parent.Open(".")
		if e != nil {
			return nil, errProjectUnavailable
		}
		defer parentHandle.Close()
		if workspaceVerifyHandle(handle, parentHandle, rootPath, rel, false) != nil {
			return nil, errWorkspaceChanged
		}
		if e := parent.Mkdir(name, 0700); e != nil {
			if errors.Is(e, os.ErrExist) {
				return nil, errors.New("project_exists: 同名目录已存在，请刷新后选择它")
			}
			return nil, errProjectUnavailable
		}
		rel = path.Join(rel, name)
		created, e := workspaceOpen(root, handle, rootPath, rel, false)
		if e != nil {
			return nil, errProjectUnavailable
		}
		created.Close()
	}
	selected := protocol.Project{Path: filepath.Join(cwd, filepath.FromSlash(rel))}
	selected.Name = config.ProjectName(selected.Path)
	// The saved path must identify the handle just verified, even if a root
	// was renamed or replaced during IO. Never silently select another folder.
	selectedInfo, e := os.Lstat(selected.Path)
	if e != nil || !selectedInfo.IsDir() {
		return nil, errProjectUnavailable
	}
	selectedHandle, e := workspaceOpen(root, handle, rootPath, rel, false)
	if e != nil {
		return nil, errProjectUnavailable
	}
	defer selectedHandle.Close()
	openedInfo, e := selectedHandle.Stat()
	if e != nil || !os.SameFile(selectedInfo, openedInfo) {
		return nil, errWorkspaceChanged
	}
	err = c.o.Store.Update(func(latest *config.Config) error {
		if !sameProjectAuthorization(current, *latest) {
			return errWorkspaceChanged
		}
		for _, p := range latest.Projects {
			if workspaceExpectedCwdMatches(p.Path, selected.Path) {
				return nil
			}
		}
		if len(latest.Projects) >= 128 {
			return errors.New("project_limit: 远程目录授权已满，请在电脑设置中整理后重试")
		}
		latest.Projects = append(latest.Projects, selected)
		return nil
	})
	if err != nil {
		return nil, err
	}
	c.Reregister()
	reply["project"] = selected
	return reply, nil
}

func sameProjectAuthorization(a, b config.Config) bool {
	if config.RemoteIdentity(a) != config.RemoteIdentity(b) || a.DeviceID != b.DeviceID || a.PhoneBindingID != b.PhoneBindingID || a.PhoneHash != b.PhoneHash || a.DirectRelayEnabled != b.DirectRelayEnabled || a.DirectPhoneID != b.DirectPhoneID || a.DirectPhoneHash != b.DirectPhoneHash || len(a.Projects) != len(b.Projects) {
		return false
	}
	for i := range a.Projects {
		if a.Projects[i] != b.Projects[i] {
			return false
		}
	}
	return true
}

func (c *Client) validateProjectGuard(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if isLocalExecution(ctx) && IsDirectExecution(ctx) {
		return validateDirectBinding(ctx, c.o.Store.Get())
	}
	return c.validateCommandGuard(ctx, "workspace.projects")
}

// Directory names such as auth, config and my-token-service are normal
// projects. This never changes workspaceRelativePath's file-content policy.
func projectRelativePath(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !utf8.ValidString(value) || len(value) > 1024 || strings.ContainsAny(value, "\\:*?\"<>|\x00") {
		return "", errProjectAuthorization
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || strings.HasPrefix(part, ".") || len(part) > 255 || strings.TrimSpace(part) != part || strings.HasSuffix(part, ".") {
			return "", errProjectAuthorization
		}
		for _, ch := range part {
			if unicode.IsControl(ch) || unicode.Is(unicode.Cf, ch) {
				return "", errProjectAuthorization
			}
		}
		device := strings.ToLower(strings.SplitN(part, ".", 2)[0])
		if device == "con" || device == "prn" || device == "aux" || device == "nul" || device == "conin$" || device == "conout$" || len(device) == 4 && (strings.HasPrefix(device, "com") || strings.HasPrefix(device, "lpt")) && device[3] >= '1' && device[3] <= '9' {
			return "", errProjectAuthorization
		}
	}
	return value, nil
}
