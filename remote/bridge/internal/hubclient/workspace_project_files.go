package hubclient

import (
	"context"
	"errors"
	"os"
	"time"
	"unicode/utf8"

	"salcara/bridge/internal/config"
)

// Select the root from the computer's saved project list before performing
// any filesystem operation. Phone cwd strings are equality assertions only.
func workspaceProjectCwd(cfg config.Config, requested string) (string, error) {
	for _, project := range cfg.Projects {
		if workspaceExpectedCwdMatches(requested, project.Path) {
			return project.Path, nil
		}
	}
	return "", errWorkspaceFiles
}

// New-task file selection is a paired project read. It does not create an
// artificial native session or borrow desktop history/execution permission.
func (c *Client) dispatchWorkspaceProjectFiles(ctx context.Context, cmd map[string]any) (any, error) {
	if !c.workspaceFilesMu.TryLock() {
		return nil, errors.New("workspace_files_busy: 请稍后重试")
	}
	defer c.workspaceFilesMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if c.o.Store == nil || str(cmd, "controlSurface") != "cli" {
		return nil, errWorkspaceFiles
	}
	for name, value := range cmd {
		if name != "type" && name != "tool" && name != "cwd" && name != "expectedCwd" && name != "controlSurface" && name != "path" && name != "cursor" {
			return nil, errWorkspaceFiles
		}
		if _, ok := value.(string); !ok {
			return nil, errWorkspaceFiles
		}
	}
	requested, expected := str(cmd, "cwd"), str(cmd, "expectedCwd")
	if len(requested) == 0 || len(requested) > 4096 || len(expected) == 0 || len(expected) > 4096 || !utf8.ValidString(requested) || !utf8.ValidString(expected) || !workspaceExpectedCwdMatches(expected, requested) {
		return nil, errWorkspaceFiles
	}
	m := c.manager()
	if m == nil {
		return nil, errWorkspaceFiles
	}
	a := m.Get(str(cmd, "tool"))
	if err := requireAgentCapability(a, "read"); err != nil {
		return nil, err
	}
	rel, err := workspaceRelativePath(str(cmd, "path"))
	typ := str(cmd, "type")
	if err != nil || typ == "workspace.project.files.read" && (rel == "" || str(cmd, "cursor") != "") {
		return nil, errWorkspaceFiles
	}
	if typ != "workspace.project.files.read" && typ != "workspace.project.files.list" {
		return nil, errWorkspaceFiles
	}
	c.taskConfigMu.Lock()
	err = c.validateCommandGuard(ctx, typ)
	cfg := c.o.Store.Get()
	c.taskConfigMu.Unlock()
	if err != nil {
		return nil, err
	}
	cwd, err := workspaceProjectCwd(cfg, requested)
	if err != nil {
		return nil, err
	}
	rootPath, err := workspaceConfiguredRoot(cfg, cwd)
	if err != nil {
		return nil, err
	}
	scope := workspaceFileScope{"project:" + a.ID(), "cli", rootPath, config.RemoteIdentity(cfg), cfg.DeviceID, cfg.PhoneBindingID, cfg.PhoneHash}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, errWorkspaceFiles
	}
	defer root.Close()
	rootHandle, err := root.Open(".")
	if err != nil {
		return nil, errWorkspaceFiles
	}
	defer rootHandle.Close()
	if workspaceVerifyHandle(rootHandle, rootHandle, rootPath, "", false) != nil {
		return nil, errWorkspaceFiles
	}
	var result any
	if typ == "workspace.project.files.list" {
		result, err = workspaceList(ctx, root, rootHandle, scope, rel, str(cmd, "cursor"))
	} else {
		result, err = workspaceRead(ctx, root, rootHandle, scope, rel)
	}
	if err != nil {
		return nil, err
	}
	// Recheck pairing/station/project authorization and exact opened root after
	// IO, before any buffered private text can leave the computer.
	c.taskConfigMu.Lock()
	defer c.taskConfigMu.Unlock()
	if err = c.validateCommandGuard(ctx, typ); err != nil {
		return nil, err
	}
	current := c.o.Store.Get()
	currentCwd, err := workspaceProjectCwd(current, requested)
	if err != nil || !workspaceExpectedCwdMatches(expected, currentCwd) {
		return nil, errWorkspaceChanged
	}
	currentRoot, err := workspaceConfiguredRoot(current, currentCwd)
	if err != nil || !workspaceSamePath(currentRoot, scope.root) || config.RemoteIdentity(current) != scope.identity || current.DeviceID != scope.device || current.PhoneBindingID != scope.binding || current.PhoneHash != scope.phone || ctx.Err() != nil {
		return nil, errWorkspaceChanged
	}
	currentInfo, err := os.Lstat(currentRoot)
	openedInfo, openedErr := rootHandle.Stat()
	if err != nil || openedErr != nil || !os.SameFile(currentInfo, openedInfo) || workspaceVerifyHandle(rootHandle, rootHandle, scope.root, "", false) != nil {
		return nil, errWorkspaceChanged
	}
	return result, nil
}
