package hubclient

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"salcara/bridge/internal/config"
)

var errWorkspaceFileConflict = errors.New("workspace_file_conflict: 电脑上的文件已变化，请比较最新内容后再保存")

func workspaceFileRevision(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Writes replace an existing authorized text file only. The phone cannot use
// this command to create paths, change projects, follow links or force a save.
func (c *Client) dispatchWorkspaceFileWrite(ctx context.Context, cmd map[string]any) (any, error) {
	return c.dispatchWorkspaceTextWrite(ctx, cmd, workspaceRead)
}

// Artifact editors share the same anchored replacement and compare-and-swap
// transaction; only their bounded reader differs (HTML/SVG document types).
func (c *Client) dispatchWorkspaceTextWrite(ctx context.Context, cmd map[string]any, readFile func(context.Context, *os.Root, *os.File, workspaceFileScope, string) (workspaceFileText, error)) (any, error) {
	if !c.workspaceFilesMu.TryLock() {
		return nil, errors.New("workspace_files_busy: 请稍后重试")
	}
	defer c.workspaceFilesMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if len(cmd) != 7 || str(cmd, "type") != "workspace.files.write" || c.o.Store == nil {
		return nil, errWorkspaceFiles
	}
	for name, value := range cmd {
		if name != "type" && name != "sessionKey" && name != "controlSurface" && name != "expectedCwd" && name != "path" && name != "text" && name != "expectedRevision" {
			return nil, errWorkspaceFiles
		}
		if _, ok := value.(string); !ok {
			return nil, errWorkspaceFiles
		}
	}
	key, surface, expectedCwd := str(cmd, "sessionKey"), str(cmd, "controlSurface"), str(cmd, "expectedCwd")
	rel, err := workspaceRelativePath(str(cmd, "path"))
	data, revision := []byte(str(cmd, "text")), str(cmd, "expectedRevision")
	decodedRevision, revisionErr := hex.DecodeString(revision)
	if err != nil || rel == "" || key == "" || len(key) > 256 || (surface != "cli" && surface != "desktop") || expectedCwd == "" || len(expectedCwd) > 4096 || !utf8.ValidString(expectedCwd) || len(data) > workspaceFileBytes || !utf8.Valid(data) || revisionErr != nil || len(decodedRevision) != sha256.Size || revision != strings.ToLower(revision) {
		return nil, errWorkspaceFiles
	}
	for _, b := range data {
		if b < 32 && b != '\n' && b != '\r' && b != '\t' || b == 127 {
			return nil, errWorkspaceFiles
		}
	}
	c.taskConfigMu.Lock()
	err = c.validateCommandGuard(ctx, "workspace.files.write")
	cfg := c.o.Store.Get()
	c.taskConfigMu.Unlock()
	if err != nil {
		return nil, err
	}
	cwd, err := c.workspaceSessionCwd(ctx, key, surface)
	if err != nil {
		return nil, err
	}
	if !workspaceExpectedCwdMatches(expectedCwd, cwd) {
		return nil, errWorkspaceChanged
	}
	rootPath, err := workspaceConfiguredRoot(cfg, cwd)
	if err != nil {
		return nil, err
	}
	scope := workspaceFileScope{key, surface, rootPath, config.RemoteIdentity(cfg), cfg.DeviceID, cfg.PhoneBindingID, cfg.PhoneHash}
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
	before, err := readFile(ctx, root, rootHandle, scope, rel)
	if err != nil {
		return nil, err
	}
	if before.Revision != revision {
		return nil, errWorkspaceFileConflict
	}
	// Anchor the replacement in the same already-verified parent directory.
	parentPath := path.Dir(rel)
	verifyParent := parentPath
	if verifyParent == "." {
		verifyParent = ""
	}
	parentHandle, err := workspaceOpen(root, rootHandle, rootPath, verifyParent, false)
	if err != nil {
		return nil, err
	}
	defer parentHandle.Close()
	parent, err := root.OpenRoot(parentPath)
	if err != nil {
		return nil, errWorkspaceFiles
	}
	defer parent.Close()
	anchoredParent, err := parent.Open(".")
	if err != nil {
		return nil, errWorkspaceFiles
	}
	defer anchoredParent.Close()
	oldParentInfo, err := parentHandle.Stat()
	newParentInfo, newParentErr := anchoredParent.Stat()
	if err != nil || newParentErr != nil || !os.SameFile(oldParentInfo, newParentInfo) {
		return nil, errWorkspaceChanged
	}
	originalInfo, err := root.Lstat(rel)
	if err != nil {
		return nil, errWorkspaceChanged
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return nil, errWorkspaceFiles
	}
	tempName := ".salcara-save-" + hex.EncodeToString(random[:])
	staged, err := parent.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, originalInfo.Mode().Perm())
	if err != nil {
		return nil, errWorkspaceFiles
	}
	defer parent.Remove(tempName)
	_, writeErr := staged.Write(data)
	if writeErr == nil {
		writeErr = staged.Sync()
	}
	closeErr := staged.Close()
	if writeErr != nil || closeErr != nil {
		return nil, errors.New("workspace_file_save_failed: 文件未保存，请重试")
	}
	// Recheck the live session before entering the configuration lock. No disk
	// mutation is allowed after project revocation or pairing replacement.
	currentCwd, err := c.workspaceSessionCwd(ctx, key, surface)
	if err != nil || !workspaceSamePath(currentCwd, cwd) {
		return nil, errWorkspaceChanged
	}
	c.taskConfigMu.Lock()
	defer c.taskConfigMu.Unlock()
	if err = c.validateCommandGuard(ctx, "workspace.files.write"); err != nil {
		return nil, err
	}
	current := c.o.Store.Get()
	currentRoot, err := workspaceConfiguredRoot(current, currentCwd)
	if err != nil || !workspaceSamePath(currentRoot, scope.root) || config.RemoteIdentity(current) != scope.identity || current.DeviceID != scope.device || current.PhoneBindingID != scope.binding || current.PhoneHash != scope.phone || ctx.Err() != nil {
		return nil, errWorkspaceChanged
	}
	if workspaceVerifyHandle(rootHandle, anchoredParent, rootPath, verifyParent, false) != nil {
		return nil, errWorkspaceChanged
	}
	latest, err := readFile(ctx, root, rootHandle, scope, rel)
	if err != nil {
		return nil, err
	}
	latestInfo, err := root.Lstat(rel)
	if err != nil || !os.SameFile(originalInfo, latestInfo) || latest.Revision != revision {
		return nil, errWorkspaceFileConflict
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err = parent.Rename(tempName, path.Base(rel)); err != nil {
		return nil, errors.New("workspace_file_save_failed: 文件未保存，可能正在被电脑程序占用")
	}
	// Only a confirmed readback is reported as saved. If acknowledgement fails,
	// the editor retains its draft and must reload before retrying.
	result, err := readFile(ctx, root, rootHandle, scope, rel)
	if err != nil || result.Revision != workspaceFileRevision(data) {
		return nil, errors.New("workspace_file_save_unconfirmed: 保存结果无法确认，请重新读取文件后比较")
	}
	return result, nil
}
