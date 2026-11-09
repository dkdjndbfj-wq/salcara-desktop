package hubclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"salcara/bridge/internal/config"
)

type workspaceFileMutationReply struct {
	Action      string             `json:"action"`
	Path        string             `json:"path"`
	Destination string             `json:"destination,omitempty"`
	File        *workspaceFileText `json:"file,omitempty"`
}

// Mutations retain the same session/project/handle authorization as reads and
// writes. Creation and rename publish with Link: an existing destination is
// never replaced, including one created after preflight by another editor.
func (c *Client) dispatchWorkspaceFileMutation(ctx context.Context, cmd map[string]any) (any, error) {
	if !c.workspaceFilesMu.TryLock() {
		return nil, errors.New("workspace_files_busy: 请稍后重试")
	}
	defer c.workspaceFilesMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	action := str(cmd, "action")
	allowed := map[string]bool{"type": true, "sessionKey": true, "controlSurface": true, "expectedCwd": true, "path": true, "action": true}
	switch action {
	case "create":
		allowed["text"] = true
	case "delete":
		allowed["expectedRevision"] = true
	case "rename":
		allowed["expectedRevision"], allowed["destination"] = true, true
	default:
		return nil, errWorkspaceFiles
	}
	if c.o.Store == nil || str(cmd, "type") != "workspace.files.mutate" || len(cmd) != len(allowed) {
		return nil, errWorkspaceFiles
	}
	for key, value := range cmd {
		if !allowed[key] {
			return nil, errWorkspaceFiles
		}
		if _, ok := value.(string); !ok {
			return nil, errWorkspaceFiles
		}
	}
	rel, err := workspaceRelativePath(str(cmd, "path"))
	if err != nil || rel == "" || !workspaceTextName(path.Base(rel)) {
		return nil, errWorkspaceFiles
	}
	key, surface, expected := str(cmd, "sessionKey"), str(cmd, "controlSurface"), str(cmd, "expectedCwd")
	if key == "" || len(key) > 256 || (surface != "cli" && surface != "desktop") || expected == "" || len(expected) > 4096 {
		return nil, errWorkspaceFiles
	}
	destination := str(cmd, "destination")
	if action == "rename" {
		if checked, e := workspaceRelativePath(destination); e != nil || checked == "" || checked == rel || path.Dir(checked) != path.Dir(rel) || !workspaceTextName(path.Base(checked)) {
			return nil, errWorkspaceFiles
		}
	}
	text := str(cmd, "text")
	revision := str(cmd, "expectedRevision")
	if action == "create" {
		if len(text) > workspaceFileBytes || !utf8.ValidString(text) {
			return nil, errWorkspaceFiles
		}
		for _, b := range []byte(text) {
			if b < 32 && b != '\n' && b != '\r' && b != '\t' || b == 127 {
				return nil, errWorkspaceFiles
			}
		}
	} else {
		decoded, e := hex.DecodeString(revision)
		if e != nil || len(decoded) != 32 || revision != strings.ToLower(revision) {
			return nil, errWorkspaceFiles
		}
	}
	c.taskConfigMu.Lock()
	err = c.validateCommandGuard(ctx, "workspace.files.mutate")
	cfg := c.o.Store.Get()
	c.taskConfigMu.Unlock()
	if err != nil {
		return nil, err
	}
	cwd, err := c.workspaceSessionCwd(ctx, key, surface)
	if err != nil {
		return nil, err
	}
	if !workspaceExpectedCwdMatches(expected, cwd) {
		return nil, errWorkspaceChanged
	}
	rootPath, err := c.workspaceSessionRoot(ctx, cfg, key, surface, cwd)
	if err != nil {
		return nil, err
	}
	scope := workspaceFileScope{key, surface, rootPath, config.RemoteIdentity(cfg), cfg.DeviceID, cfg.PhoneBindingID, cfg.PhoneHash}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, errWorkspaceFiles
	}
	defer root.Close()
	handle, err := root.Open(".")
	if err != nil {
		return nil, errWorkspaceFiles
	}
	defer handle.Close()
	verifyParent := func(name string) error {
		parent := path.Dir(name)
		if parent == "." {
			parent = ""
		}
		h, e := workspaceOpen(root, handle, rootPath, parent, false)
		if e != nil {
			return e
		}
		defer h.Close()
		return nil
	}
	if err = verifyParent(rel); err != nil {
		return nil, err
	}
	if action == "rename" {
		if err = verifyParent(destination); err != nil {
			return nil, err
		}
	}
	parentPath := path.Dir(rel)
	verifyPath := parentPath
	if verifyPath == "." {
		verifyPath = ""
	}
	parent, err := root.OpenRoot(parentPath)
	if err != nil {
		return nil, errWorkspaceFiles
	}
	defer parent.Close()
	parentHandle, err := parent.Open(".")
	if err != nil {
		return nil, errWorkspaceFiles
	}
	defer parentHandle.Close()
	if workspaceVerifyHandle(handle, parentHandle, rootPath, verifyPath, false) != nil {
		return nil, errWorkspaceChanged
	}
	var before workspaceFileText
	if action != "create" {
		before, err = workspaceRead(ctx, root, handle, scope, rel)
		if err != nil {
			return nil, err
		}
		if before.Revision != revision {
			return nil, errWorkspaceFileConflict
		}
	}
	// Obtain fresh metadata after all preflight IO, then freeze binding changes.
	currentCwd, err := c.workspaceSessionCwd(ctx, key, surface)
	if err != nil || !workspaceSamePath(cwd, currentCwd) {
		return nil, errWorkspaceChanged
	}
	c.taskConfigMu.Lock()
	defer c.taskConfigMu.Unlock()
	if err = c.validateCommandGuard(ctx, "workspace.files.mutate"); err != nil {
		return nil, err
	}
	current := c.o.Store.Get()
	currentRoot, err := c.workspaceSessionRoot(ctx, current, key, surface, currentCwd)
	if err != nil || !workspaceSamePath(currentRoot, scope.root) || config.RemoteIdentity(current) != scope.identity || current.DeviceID != scope.device || current.PhoneBindingID != scope.binding || current.PhoneHash != scope.phone || ctx.Err() != nil {
		return nil, errWorkspaceChanged
	}
	if workspaceVerifyHandle(handle, handle, rootPath, "", false) != nil || workspaceVerifyHandle(handle, parentHandle, rootPath, verifyPath, false) != nil {
		return nil, errWorkspaceChanged
	}
	if err = verifyParent(rel); err != nil {
		return nil, err
	}
	if action == "rename" {
		if err = verifyParent(destination); err != nil {
			return nil, err
		}
	}
	if action != "create" {
		latest, e := workspaceRead(ctx, root, handle, scope, rel)
		if e != nil {
			return nil, e
		}
		if latest.Revision != revision {
			return nil, errWorkspaceFileConflict
		}
	}
	reply := workspaceFileMutationReply{Action: action, Path: rel, Destination: destination}
	switch action {
	case "create":
		var random [16]byte
		if _, err = rand.Read(random[:]); err != nil {
			return nil, errWorkspaceFiles
		}
		temp := ".salcara-create-" + hex.EncodeToString(random[:])
		staged, e := parent.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return nil, errWorkspaceFiles
		}
		defer parent.Remove(temp)
		_, e = staged.WriteString(text)
		if e == nil {
			e = staged.Sync()
		}
		closeErr := staged.Close()
		if e != nil || closeErr != nil {
			return nil, errors.New("workspace_file_create_failed: 文件未创建")
		}
		if err = parent.Link(temp, path.Base(rel)); err != nil {
			return nil, errors.New("workspace_file_exists: 目标文件已存在或不可创建，请刷新目录")
		}
		if err = parent.Remove(temp); err != nil {
			return nil, errors.New("workspace_file_create_unconfirmed: 文件已创建，但暂存清理未确认，请刷新目录")
		}
		after, e := workspaceRead(ctx, root, handle, scope, rel)
		if e != nil || after.Revision != workspaceFileRevision([]byte(text)) {
			return nil, errors.New("workspace_file_create_unconfirmed: 创建结果未确认，请重新读取")
		}
		reply.File = &after
	case "delete":
		if err = parent.Remove(path.Base(rel)); err != nil {
			return nil, errors.New("workspace_file_delete_failed: 文件未删除，可能正在被电脑程序占用")
		}
	case "rename":
		if err = parent.Link(path.Base(rel), path.Base(destination)); err != nil {
			return nil, errors.New("workspace_file_exists: 目标文件已存在或无法移动，请刷新目录")
		}
		// Same inode at both paths until source removal; edits to the open original
		// survive at the new name. Roll back only our own destination on failure.
		oldInfo, e1 := parent.Lstat(path.Base(rel))
		newInfo, e2 := parent.Lstat(path.Base(destination))
		if e1 != nil || e2 != nil || !os.SameFile(oldInfo, newInfo) {
			return nil, errors.New("workspace_file_rename_unconfirmed: 文件在移动期间变化，请核对两个路径")
		}
		if err = parent.Remove(path.Base(rel)); err != nil {
			_ = parent.Remove(path.Base(destination))
			return nil, errors.New("workspace_file_rename_failed: 文件未移动，可能正在被电脑程序占用")
		}
		after, e := workspaceRead(ctx, root, handle, scope, destination)
		if e != nil {
			return nil, errors.New("workspace_file_rename_unconfirmed: 文件已移动，请重新读取确认")
		}
		reply.File = &after
	}
	return reply, nil
}
