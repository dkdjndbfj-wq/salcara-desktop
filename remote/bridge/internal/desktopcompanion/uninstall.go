package desktopcompanion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

type uninstallPlan struct {
	p             paths
	config, state snapshot
	updated       []byte
	installed     bool
}

var errUninstallChanged = errors.New("配置或安装记录在卸载期间已改变，已拒绝覆盖；请重新预览")
var errUninstallCanceled = errors.New("卸载已取消，原 Codex 配置未修改；私有备份保留")

// PreviewUninstall checks only the existing configuration and ownership record.
// A removed bundle or runtime cannot prevent safely disabling this owned MCP
// entry. No files, directories, pipes, or processes are created or changed.
func (s *Service) PreviewUninstall(ctx context.Context) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := baseStatus()
	plan, err := s.planUninstall(ctx)
	if err != nil {
		st.Message = err.Error()
		return st, err
	}
	st.Installed, st.UninstallAvailable = plan.installed, plan.installed
	st.Message = "桌面验证插件未安装；不会创建目录或修改 Codex 配置"
	if plan.installed {
		st.Message = "可卸载桌面验证插件；只移除本安装器的 MCP 与审批 hook，保留其他配置、会话、插件资源和私有备份"
	}
	return st, nil
}

// Uninstall cuts out only the unchanged installer-owned TOML block. It never
// restores the whole pre-install backup: user edits made since installation
// must survive. The ownership record is moved to a recoverable private archive;
// payloads, runtimes, old versions, and all backups are deliberately retained.
func (s *Service) Uninstall(ctx context.Context) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := baseStatus()
	plan, err := s.planUninstall(ctx)
	if err != nil {
		st.Message = err.Error()
		return st, err
	}
	if !plan.state.exists {
		st.Message = "桌面验证插件未安装；未创建目录或修改配置，现有资源和备份保留"
		return st, nil
	}
	if ctx.Err() != nil {
		return st, errors.New("卸载已取消，原 Codex 配置未修改")
	}
	id, err := randomID()
	if err != nil {
		return st, errors.New("无法生成安全卸载标识，原配置未修改")
	}
	archive := filepath.Join(plan.p.root, "archives", "uninstalled-"+id+".json")
	if err = privateDir(filepath.Dir(archive)); err != nil {
		return st, errors.New("无法创建私有卸载归档，原配置未修改")
	}
	if plan.installed {
		if err = privateDir(filepath.Join(plan.p.root, "backups")); err != nil {
			return st, errors.New("无法创建私有备份目录，原配置未修改")
		}
		if err = writeExclusive(filepath.Join(plan.p.root, "backups", "uninstall-"+id+".toml"), plan.config.data); err != nil {
			return st, errors.New("无法备份当前 Codex 配置，卸载已停止")
		}
	}
	if ctx.Err() != nil {
		return st, errors.New("卸载已取消，原 Codex 配置未修改；私有备份保留")
	}
	if s.beforeCommit != nil {
		s.beforeCommit()
	}
	if plan.installed {
		err = atomicWriteChecked(plan.p.config, plan.updated, func() error {
			if ctx.Err() != nil {
				return errUninstallCanceled
			}
			return uninstallUnchanged(plan)
		})
		if err != nil {
			if errors.Is(err, errUninstallChanged) || errors.Is(err, errUninstallCanceled) {
				return st, err
			}
			return st, errors.New("无法原子移除插件 MCP 配置，原配置未覆盖")
		}
		if s.afterUninstallConfigCommit != nil {
			err = s.afterUninstallConfigCommit()
		}
	} else {
		if ctx.Err() != nil {
			err = errors.New("卸载已取消，原 Codex 配置未修改")
		} else {
			err = uninstallUnchanged(plan)
		}
	}
	if err == nil {
		if s.beforeUninstallArchive != nil {
			s.beforeUninstallArchive()
		}
		err = archiveUninstallRecord(plan, archive)
	}
	if err != nil {
		if plan.installed {
			if rollbackErr := rollback(plan.p.config, plan.config, plan.updated); rollbackErr != nil {
				return st, rollbackErr
			}
			return st, errors.New("卸载记录归档失败，Codex 配置已回滚；私有备份保留")
		}
		return st, errors.New("安装记录已改变或无法归档，未覆盖记录或 Codex 配置")
	}
	st.RestartRequired = plan.installed
	st.Message = "已卸载桌面验证插件的 MCP 与审批 hook；请手动重启 Codex 使已运行插件退出。其他配置、API、会话、插件资源和私有备份保留，安装记录已归档"
	if !plan.installed {
		st.Message = "配置中已无桌面验证插件；安装记录已归档，现有资源和私有备份保留，未修改 Codex 配置"
	}
	return st, nil
}

func (s *Service) uninstallPaths(ctx context.Context) (paths, error) {
	var p paths
	if ctx.Err() != nil {
		return p, errors.New("卸载操作已取消")
	}
	if s.o.DataDir == "" {
		return p, errors.New("插件数据目录未配置，无法验证卸载所有权")
	}
	data, err := filepath.Abs(s.o.DataDir)
	if err != nil {
		return p, errors.New("插件数据目录无效")
	}
	home := s.o.CodexHome
	if home == "" {
		home = os.Getenv("CODEX_HOME")
	}
	if home == "" {
		user, err := os.UserHomeDir()
		if err != nil {
			return p, errors.New("无法确定原 Codex 配置目录")
		}
		home = filepath.Join(user, ".codex")
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return p, errors.New("原 Codex 配置目录无效")
	}
	p.root, p.payload, p.config = filepath.Join(data, "desktop-companion"), filepath.Join(data, "desktop-companion", "v"+Version), filepath.Join(home, "config.toml")
	p.state = filepath.Join(p.root, "installs", hash([]byte(p.config))+".json")
	for _, path := range []string{p.root, p.config, p.state} {
		if rejectLinks(path) != nil {
			return p, errors.New("卸载路径包含符号链接或不可安全读取，已停止")
		}
	}
	if within(p.root, p.config) {
		return p, errors.New("原 Codex 配置不能位于插件安装目录内部")
	}
	if err := checkPrivateRoot(p.root); err != nil {
		return p, err
	}
	return p, nil
}

func (s *Service) planUninstall(ctx context.Context) (uninstallPlan, error) {
	var plan uninstallPlan
	p, err := s.uninstallPaths(ctx)
	if err != nil {
		return plan, err
	}
	plan.p = p
	config, doc, err := readConfig(p.config)
	if err != nil {
		return plan, err
	}
	plan.config = config
	state, err := readSnapshot(p.state)
	if err != nil {
		return plan, errors.New("安装记录不可安全读取，已拒绝卸载")
	}
	plan.state = state
	var own ownership
	if state.exists {
		if json.Unmarshal(state.data, &own) != nil || own.Schema != 1 || own.Owner != rootOwner || own.Config != p.config || (!hasPermissionHook(own.Version) && own.Version != "0.3.0" && own.Version != "0.1.0") || !ownerID.MatchString(own.ID) {
			return plan, errors.New("现有安装记录不是本安装器所有，已拒绝卸载")
		}
		p.payload = filepath.Join(p.root, "v"+own.Version)
		plan.p = p
	}
	servers, exists := doc["mcp_servers"]
	if !exists {
		if state.exists && hasPermissionHook(own.Version) && (permissionHookCount(doc, p) != 0 || installerMarkerExists(config.data, own.ID)) {
			return plan, errors.New("MCP 配置已移除但审批 hook 或安装标记仍在，未归档记录；请手动检查")
		}
		return plan, nil
	}
	m, ok := servers.(map[string]any)
	if !ok {
		return plan, errors.New("现有 MCP 配置结构无效，原配置未修改")
	}
	value, exists := m[ServerName]
	if !exists {
		if state.exists && hasPermissionHook(own.Version) && (permissionHookCount(doc, p) != 0 || installerMarkerExists(config.data, own.ID)) {
			return plan, errors.New("MCP 配置已移除但审批 hook 或安装标记仍在，未归档记录；请手动检查")
		}
		return plan, nil
	}
	entry, ok := value.(map[string]any)
	if !ok || !state.exists || own.EntryHash != entryHash(entry) {
		return plan, errors.New("同名 MCP 配置不是本安装器所有或已被修改，已拒绝卸载")
	}
	command, ok := entry["command"].(string)
	if !ok || !filepath.IsAbs(command) {
		return plan, errors.New("已安装 MCP 入口无效，已拒绝卸载")
	}
	p.node = command
	if !reflect.DeepEqual(entry, expectedEntryVersion(p, own.Version)) {
		return plan, errors.New("已安装 MCP 配置的路径或策略已改变，已拒绝卸载")
	}
	if hasPermissionHook(own.Version) && (own.HookHash != entryHash(expectedPermissionHook(p)) || permissionHookCount(doc, p) != 1) {
		return plan, errors.New("已安装审批 hook 的路径或策略已改变，已拒绝卸载")
	}
	start, end, err := ownedBlockRange(config.data, own, p)
	if err != nil {
		return plan, err
	}
	plan.updated = append(append([]byte{}, config.data[:start]...), config.data[end:]...)
	var after map[string]any
	if toml.Unmarshal(plan.updated, &after) != nil || !sameOtherSettings(doc, after, p, own.Version) {
		return plan, errors.New("不能仅移除插件配置并保持其他 TOML 设置，已拒绝卸载")
	}
	if remaining, ok := after["mcp_servers"].(map[string]any); ok {
		if _, exists := remaining[ServerName]; exists {
			return plan, errors.New("无法验证插件配置已完整移除，已拒绝卸载")
		}
	}
	if hasPermissionHook(own.Version) && permissionHookCount(after, p) != 0 {
		return plan, errors.New("无法验证审批 hook 已完整移除，已拒绝卸载")
	}
	plan.installed = true
	return plan, nil
}

// Use the TOML parser rather than textual header search: headers and ownership
// comments that happen to occur inside multiline strings are not executable
// configuration. Only the exact canonical block emitted by this installer may
// be removed. Formatting/comment changes inside it fail closed as user edits.
func ownedBlockRange(data []byte, own ownership, p paths) (int, int, error) {
	marker := "# Salcara desktop companion installer: " + own.ID
	var parser unstable.Parser
	parser.KeepComments = true
	parser.Reset(data)
	commentAt, tableAt, commentCount, tableCount := -1, -1, 0, 0
	for parser.NextExpression() {
		n := parser.Expression()
		if n.Kind == unstable.Comment && bytes.Equal(parser.Raw(n.Raw), []byte(marker)) {
			commentAt, commentCount = int(n.Raw.Offset), commentCount+1
		}
		if n.Kind != unstable.Table {
			continue
		}
		keys := n.Key()
		var names []string
		firstOffset := -1
		for keys.Next() {
			key := keys.Node()
			if firstOffset == -1 {
				firstOffset = int(key.Raw.Offset)
			}
			names = append(names, string(key.Data))
		}
		if reflect.DeepEqual(names, []string{"mcp_servers", ServerName}) {
			tableAt = bytes.LastIndexByte(data[:firstOffset], '\n') + 1
			tableCount++
		}
	}
	block := []byte(marker + "\n" + installedBlockTOML(p, own.Version))
	if parser.Error() != nil || commentCount != 1 || tableCount != 1 || commentAt < 1 || data[commentAt-1] != '\n' || tableAt != commentAt+len(marker)+1 || commentAt+len(block) > len(data) || !bytes.Equal(data[commentAt:commentAt+len(block)], block) {
		return 0, 0, errors.New("插件安装标记或配置文本已被修改，已拒绝卸载；请手动检查")
	}
	// Keep the leading separator newline. It may now separate user settings
	// appended after installation from an original file without a final newline.
	return commentAt, commentAt + len(block), nil
}

func installerMarkerExists(data []byte, id string) bool {
	marker := []byte("# Salcara desktop companion installer: " + id)
	var parser unstable.Parser
	parser.KeepComments = true
	parser.Reset(data)
	for parser.NextExpression() {
		n := parser.Expression()
		if n.Kind == unstable.Comment && bytes.Equal(parser.Raw(n.Raw), marker) {
			return true
		}
	}
	return false
}

func uninstallUnchanged(plan uninstallPlan) error {
	config, configErr := readSnapshot(plan.p.config)
	state, stateErr := readSnapshot(plan.p.state)
	if configErr != nil || stateErr != nil || !snapshotsEqual(plan.config, config) || !snapshotsEqual(plan.state, state) || checkPrivateRoot(plan.p.root) != nil {
		return errUninstallChanged
	}
	return nil
}

func archiveUninstallRecord(plan uninstallPlan, archive string) error {
	state, err := readSnapshot(plan.p.state)
	if err != nil || !snapshotsEqual(plan.state, state) || rejectLinks(archive) != nil || checkPrivateRoot(plan.p.root) != nil {
		return errUninstallChanged
	}
	// Archive names are fresh random identifiers in a private directory. Never
	// intentionally replace a pre-existing archive or delete a shared runtime.
	if _, err = os.Lstat(archive); !errors.Is(err, os.ErrNotExist) {
		return errors.New("private archive already exists or cannot be checked")
	}
	if err = os.Rename(plan.p.state, archive); err != nil {
		return errors.New("private archive rename failed")
	}
	return nil
}
