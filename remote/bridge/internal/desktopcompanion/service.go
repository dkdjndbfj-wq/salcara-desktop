// Package desktopcompanion installs the explicitly approved experimental MCP
// companion. Installation never contacts a pipe, runs Node or restarts Codex.
package desktopcompanion

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"unicode/utf16"

	"github.com/pelletier/go-toml/v2"
)

const (
	Version        = "0.4.1"
	ServerName     = "salcara_desktop_probe"
	ConnectTool    = "salcara_desktop_connect"
	maxFileBytes   = 4 << 20
	maxBundleBytes = 16 << 20
	rootOwner      = "salcara-desktop-companion"
)

type Options struct{ BundleDir, DataDir, CodexHome, NodePath string }

type Status struct {
	Available          bool   `json:"available"`
	Installed          bool   `json:"installed"`
	UninstallAvailable bool   `json:"uninstallAvailable"`
	RestartRequired    bool   `json:"restartRequired"`
	CatalogOnly        bool   `json:"catalogOnly"`
	Version            string `json:"version"`
	Message            string `json:"message"`
}

type Service struct {
	o  Options
	mu sync.Mutex
	// Pure test hooks; neither is part of the public installation interface.
	beforeCommit               func()
	afterConfigCommit          func() error
	afterUninstallConfigCommit func() error
	beforeUninstallArchive     func()
}

func New(o Options) *Service { return &Service{o: o} }

type paths struct{ bundle, root, payload, config, node, state string }
type snapshot struct {
	exists bool
	data   []byte
}
type ownership struct {
	Schema      int    `json:"schema"`
	Owner       string `json:"owner"`
	ID          string `json:"id"`
	Version     string `json:"version"`
	Config      string `json:"config"`
	EntryHash   string `json:"entryHash"`
	HookHash    string `json:"hookHash,omitempty"`
	PayloadHash string `json:"payloadHash"`
}
type bundleFile struct {
	path string
	data []byte
}

var mjsName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*\.mjs$`)
var ownerID = regexp.MustCompile(`^[a-f0-9]{32}$`)
var errConfigChanged = errors.New("Codex 配置在安装期间已改变，已拒绝覆盖；请重新预览")

func baseStatus() Status { return Status{CatalogOnly: false, Version: Version} }

// Preview is read-only. In particular it does not create CODEX_HOME, copy a
// payload, inspect any app credentials, execute a runtime, or enumerate pipes.
func (s *Service) Preview(ctx context.Context) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := baseStatus()
	p, files, digest, err := s.prepare(ctx)
	if err != nil {
		st.Message = err.Error()
		return st, err
	}
	old, doc, err := readConfig(p.config)
	if err != nil {
		st.Message = err.Error()
		return st, err
	}
	installed, err := installationOwned(p, old, doc, digest)
	if err != nil {
		st.Message = err.Error()
		return st, err
	}
	if len(files) == 0 {
		return st, errors.New("桌面验证插件内容不可用")
	}
	if !installed {
		if _, err = appendedConfig(old.data, doc, p, strings.Repeat("0", 32)); err != nil {
			st.Message = err.Error()
			return st, err
		}
	}
	st.Available, st.Installed, st.RestartRequired = true, installed, installed
	st.UninstallAvailable = installed
	st.Message = "安装实验桌面连接插件与审批 hook；不会自动打开或重启 Codex。连接须在 Codex 中授权，审批 hook 须另行审核并信任"
	if installed {
		st.Message = "实验桌面连接插件已安装；请在 Codex 中调用 salcara_desktop_connect 授权会话，并审核信任审批 hook。安装不会自动启用远程控制或信任"
	}
	return st, nil
}

// Install appends an owned MCP table and synchronous permission hook. Existing TOML bytes and all
// other settings remain untouched. Existing foreign or edited entries fail
// closed instead of being silently repaired or replaced.
func (s *Service) Install(ctx context.Context) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := baseStatus()
	p, files, digest, err := s.prepare(ctx)
	if err != nil {
		st.Message = err.Error()
		return st, err
	}
	old, doc, err := readConfig(p.config)
	if err != nil {
		st.Message = err.Error()
		return st, err
	}
	installed, err := installationOwned(p, old, doc, digest)
	if err != nil {
		st.Message = err.Error()
		return st, err
	}
	if installed {
		st.Available, st.Installed, st.RestartRequired = true, true, true
		st.UninstallAvailable = true
		st.Message = "桌面验证插件已安装；未重写配置或重启 Codex"
		return st, nil
	}
	if err = ctx.Err(); err != nil {
		return st, errors.New("安装已取消，原 Codex 配置未修改")
	}
	id, err := randomID()
	if err != nil {
		return st, errors.New("无法生成安全安装标识，原配置未修改")
	}
	entry := expectedEntry(p)
	updated, err := appendedConfig(old.data, doc, p, id)
	if err != nil {
		return st, err
	}
	if err = ensurePrivateRoot(p.root); err != nil {
		return st, err
	}
	if err = stagePayload(ctx, p, files); err != nil {
		return st, err
	}
	if err = privateDir(filepath.Dir(p.state)); err != nil {
		return st, errors.New("无法创建私有安装记录，原配置未修改")
	}
	if err = privateDir(filepath.Join(p.root, "backups")); err != nil {
		return st, errors.New("无法创建私有备份目录，原配置未修改")
	}
	if old.exists {
		if err = writeExclusive(filepath.Join(p.root, "backups", "config-"+id+".toml"), old.data); err != nil {
			return st, errors.New("无法创建私有原配置备份，安装已停止")
		}
	}
	if err = ensureParents(filepath.Dir(p.config)); err != nil {
		return st, errors.New("Codex 配置目录不可安全写入，原配置未修改")
	}
	if err = ctx.Err(); err != nil {
		return st, errors.New("安装已取消，原 Codex 配置未修改")
	}
	if s.beforeCommit != nil {
		s.beforeCommit()
	}
	if err = replaceIfUnchanged(p.config, old, updated); err != nil {
		return st, err
	}
	record := ownership{Schema: 1, Owner: rootOwner, ID: id, Version: Version, Config: p.config, EntryHash: entryHash(entry), HookHash: entryHash(expectedPermissionHook(p)), PayloadHash: digest}
	data, _ := json.Marshal(record)
	if s.afterConfigCommit != nil {
		err = s.afterConfigCommit()
	}
	if err == nil {
		err = atomicWrite(p.state, data)
	}
	if err != nil {
		if rollbackErr := rollback(p.config, old, updated); rollbackErr != nil {
			return st, rollbackErr
		}
		return st, errors.New("安装记录保存失败，Codex 配置已回滚；私有备份保留")
	}
	st.Available, st.Installed, st.RestartRequired = true, true, true
	st.UninstallAvailable = true
	st.Message = "已安装实验桌面连接插件与审批 hook；请在 Codex 中授权 salcara_desktop_connect 并审核信任审批 hook。未重启应用、未修改信任、API 或会话"
	return st, nil
}

func (s *Service) prepare(ctx context.Context) (paths, []bundleFile, string, error) {
	var p paths
	if ctx.Err() != nil {
		return p, nil, "", errors.New("操作已取消")
	}
	if s.o.BundleDir == "" || s.o.DataDir == "" {
		return p, nil, "", errors.New("桌面验证插件资源目录未配置")
	}
	var err error
	p.bundle, err = filepath.Abs(s.o.BundleDir)
	if err != nil {
		return p, nil, "", errors.New("插件资源目录无效")
	}
	data, err := filepath.Abs(s.o.DataDir)
	if err != nil {
		return p, nil, "", errors.New("插件数据目录无效")
	}
	home := s.o.CodexHome
	if home == "" {
		home = os.Getenv("CODEX_HOME")
	}
	if home == "" {
		var user string
		user, err = os.UserHomeDir()
		if err != nil {
			return p, nil, "", errors.New("无法确定原 Codex 配置目录")
		}
		home = filepath.Join(user, ".codex")
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return p, nil, "", errors.New("原 Codex 配置目录无效")
	}
	p.root, p.payload, p.config = filepath.Join(data, "desktop-companion"), filepath.Join(data, "desktop-companion", "v"+Version), filepath.Join(home, "config.toml")
	p.state = filepath.Join(p.root, "installs", hash([]byte(p.config))+".json")
	for _, path := range []string{p.bundle, p.root, p.config} {
		if rejectLinks(path) != nil {
			return p, nil, "", errors.New("安装路径包含符号链接或不是普通目录/文件，已停止")
		}
	}
	if within(p.bundle, p.root) || within(p.root, p.bundle) {
		return p, nil, "", errors.New("插件源目录不能与安装目录重叠")
	}
	if within(p.bundle, p.config) || within(p.root, p.config) {
		return p, nil, "", errors.New("原 Codex 配置不能位于插件资源或安装目录内部")
	}
	if err = checkPrivateRoot(p.root); err != nil {
		return p, nil, "", err
	}
	if err = checkExistingOwnership(p); err != nil {
		return p, nil, "", err
	}
	p.node = s.o.NodePath
	if p.node == "" {
		p.node, err = exec.LookPath("node")
	} else if !filepath.IsAbs(p.node) {
		err = errors.New("relative runtime")
	}
	if err != nil || p.node == "" {
		return p, nil, "", errors.New("未找到 Node.js；请先安装 Node.js 后重试，当前未安装插件")
	}
	p.node, err = filepath.Abs(p.node)
	if err == nil {
		p.node, err = filepath.EvalSymlinks(p.node)
	}
	if err != nil {
		return p, nil, "", errors.New("Node.js 程序路径不可用，当前未安装插件")
	}
	info, err := os.Stat(p.node)
	if err != nil || !info.Mode().IsRegular() {
		return p, nil, "", errors.New("Node.js 程序不是普通文件，当前未安装插件")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0111 == 0 {
		return p, nil, "", errors.New("Node.js 程序不可执行，当前未安装插件")
	}
	if filepath.Ext(p.node) != "" && !strings.EqualFold(filepath.Ext(p.node), ".exe") {
		return p, nil, "", errors.New("Node.js 必须使用原生可执行程序，不能使用 shell 脚本入口")
	}
	files, digest, err := readBundle(p.bundle)
	return p, files, digest, err
}

func readBundle(root string) ([]bundleFile, string, error) {
	names := []string{"LICENSE", "package.json"}
	if rejectLinks(filepath.Join(root, "src")) != nil {
		return nil, "", errors.New("插件源文件目录不安全")
	}
	entries, err := os.ReadDir(filepath.Join(root, "src"))
	if err != nil {
		return nil, "", errors.New("桌面验证插件资源未找到")
	}
	for _, e := range entries {
		if mjsName.MatchString(e.Name()) {
			names = append(names, "src/"+e.Name())
		}
	}
	sort.Strings(names)
	files := make([]bundleFile, 0, len(names))
	total := 0
	index := false
	permissionHook := false
	for _, name := range names {
		data, err := readRegular(filepath.Join(root, filepath.FromSlash(name)), maxFileBytes)
		if err != nil {
			return nil, "", errors.New("插件缺少必要普通文件，或包含不安全文件")
		}
		total += len(data)
		if total > maxBundleBytes {
			return nil, "", errors.New("插件资源超出安全大小限制")
		}
		if name == "src/index.mjs" {
			index = true
		}
		if name == "src/permission-hook.mjs" {
			permissionHook = true
		}
		files = append(files, bundleFile{name, data})
	}
	if !index {
		return nil, "", errors.New("桌面验证插件入口 src/index.mjs 缺失")
	}
	if !permissionHook {
		return nil, "", errors.New("桌面审批桥接入口 src/permission-hook.mjs 缺失")
	}
	var pkg struct {
		Version string `json:"version"`
	}
	for _, f := range files {
		if f.path == "package.json" && (json.Unmarshal(f.data, &pkg) != nil || pkg.Version != Version) {
			return nil, "", errors.New("桌面验证插件版本不匹配")
		}
	}
	h := sha256.New()
	for _, f := range files {
		h.Write([]byte(f.path))
		h.Write([]byte{0})
		h.Write(f.data)
		h.Write([]byte{0})
	}
	return files, hex.EncodeToString(h.Sum(nil)), nil
}

func expectedEntry(p paths) map[string]any {
	return expectedEntryVersion(p, Version)
}
func expectedEntryVersion(p paths, version string) map[string]any {
	tools := []any{ServerName}
	timeout := int64(15)
	if hasPermissionHook(version) || version == "0.3.0" {
		tools = append(tools, ConnectTool)
		timeout = 2592060
	}
	return map[string]any{"command": p.node, "cwd": p.payload, "args": []any{filepath.Join(p.payload, "src", "index.mjs")}, "env_vars": []any{"CODEX_APP_TOOLS_PIPE_PATH", "CODEX_ELECTRON_RESOURCES_PATH"}, "enabled": true, "enabled_tools": tools, "default_tools_approval_mode": "prompt", "startup_timeout_sec": int64(10), "tool_timeout_sec": timeout}
}
func entryTOML(p paths) string {
	return entryTOMLVersion(p, Version)
}
func entryTOMLVersion(p paths, version string) string {
	q := func(v string) string { b, _ := json.Marshal(v); return string(b) }
	tools := q(ServerName)
	timeout := "15"
	if hasPermissionHook(version) || version == "0.3.0" {
		tools += ", " + q(ConnectTool)
		timeout = "2592060"
	}
	return "[mcp_servers." + ServerName + "]\ncommand = " + q(p.node) + "\ncwd = " + q(p.payload) + "\nargs = [" + q(filepath.Join(p.payload, "src", "index.mjs")) + "]\nenv_vars = [\"CODEX_APP_TOOLS_PIPE_PATH\", \"CODEX_ELECTRON_RESOURCES_PATH\"]\nenabled = true\nenabled_tools = [" + tools + "]\ndefault_tools_approval_mode = \"prompt\"\nstartup_timeout_sec = 10\ntool_timeout_sec = " + timeout + "\n"
}

func permissionHookCommand(p paths) string {
	// A command hook is a shell command, not an argv array. Quote both absolute
	// paths, without evaluating variables or deriving commands from user data.
	quote := func(path string) string {
		return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'"
	}
	return quote(p.node) + " " + quote(filepath.Join(p.payload, "src", "permission-hook.mjs"))
}

func permissionHookCommandWindows(p paths) string {
	quote := func(path string) string { return "'" + strings.ReplaceAll(path, "'", "''") + "'" }
	// Pin the inner shell and encode only installer-owned script text. The host's
	// outer Windows shell never interprets paths or JSON input. Explicit UTF-8
	// preserves non-ASCII approval descriptions through Windows PowerShell 5.1.
	script := "[Console]::InputEncoding=[System.Text.UTF8Encoding]::new($false); " +
		"[Console]::OutputEncoding=[System.Text.UTF8Encoding]::new($false); " +
		"$OutputEncoding=[System.Text.UTF8Encoding]::new($false); " +
		"$inputJson=[Console]::In.ReadToEnd(); $inputJson | & " + quote(p.node) + " " + quote(filepath.Join(p.payload, "src", "permission-hook.mjs")) + "; exit $LASTEXITCODE"
	codepoints := utf16.Encode([]rune(script))
	encoded := make([]byte, 2*len(codepoints))
	for i, point := range codepoints {
		binary.LittleEndian.PutUint16(encoded[i*2:], point)
	}
	return "C:/Windows/System32/WindowsPowerShell/v1.0/powershell.exe -NoProfile -NonInteractive -EncodedCommand " + base64.StdEncoding.EncodeToString(encoded)
}

func expectedPermissionHook(p paths) map[string]any {
	return map[string]any{"hooks": []any{map[string]any{"type": "command", "command": permissionHookCommand(p), "command_windows": permissionHookCommandWindows(p), "timeout": int64(120), "async": false}}}
}

func hookTOMLVersion(p paths, version string) string {
	if !hasPermissionHook(version) {
		return ""
	}
	command, _ := json.Marshal(permissionHookCommand(p))
	commandWindows, _ := json.Marshal(permissionHookCommandWindows(p))
	return "\n[[hooks.PermissionRequest]]\n\n[[hooks.PermissionRequest.hooks]]\ntype = \"command\"\ncommand = " + string(command) + "\ncommand_windows = " + string(commandWindows) + "\ntimeout = 120\nasync = false\n"
}

func hasPermissionHook(version string) bool { return version == Version || version == "0.4.0" }

func installedBlockTOML(p paths, version string) string {
	return entryTOMLVersion(p, version) + hookTOMLVersion(p, version)
}

func appendedConfig(original []byte, before map[string]any, p paths, id string) ([]byte, error) {
	if permissionHookCount(before, p) != 0 {
		return nil, errors.New("审批 hook 已存在但无法验证安装所有权，原配置未修改")
	}
	updated := append(append([]byte{}, original...), []byte("\n# Salcara desktop companion installer: "+id+"\n"+installedBlockTOML(p, Version))...)
	var after map[string]any
	if toml.Unmarshal(updated, &after) != nil || permissionHookCount(after, p) != 1 || !sameOtherSettings(before, after, p, Version) {
		return nil, errors.New("现有 TOML 不允许安全追加此 MCP 表；原配置未修改，请手动检查配置")
	}
	servers, ok := after["mcp_servers"].(map[string]any)
	if !ok || !reflect.DeepEqual(servers[ServerName], expectedEntry(p)) {
		return nil, errors.New("无法验证新增 MCP 配置的安全策略，原配置未修改")
	}
	return updated, nil
}
func checkExistingOwnership(p paths) error {
	if _, err := os.Lstat(p.state); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return errors.New("安装记录不可安全读取，已停止")
	}
	b, err := readRegular(p.state, maxFileBytes)
	var own ownership
	if err != nil || json.Unmarshal(b, &own) != nil || own.Schema != 1 || own.Owner != rootOwner || own.Config != p.config || !ownerID.MatchString(own.ID) {
		return errors.New("现有安装记录不是本安装器所有，已拒绝覆盖")
	}
	return nil
}
func installationOwned(p paths, snap snapshot, doc map[string]any, digest string) (bool, error) {
	servers, ok := doc["mcp_servers"]
	if !ok {
		return false, nil
	}
	m, ok := servers.(map[string]any)
	if !ok {
		return false, errors.New("现有 MCP 配置结构无效，原配置未修改")
	}
	value, exists := m[ServerName]
	if !exists {
		return false, nil
	}
	entry, ok := value.(map[string]any)
	if !ok {
		return false, errors.New("同名 MCP 配置存在且不属于安装器，已拒绝覆盖")
	}
	b, err := readRegular(p.state, maxFileBytes)
	var own ownership
	if err != nil || json.Unmarshal(b, &own) != nil || own.Schema != 1 || own.Owner != rootOwner || own.Version != Version || own.Config != p.config || !ownerID.MatchString(own.ID) || own.EntryHash != entryHash(entry) || !bytes.Contains(snap.data, []byte("# Salcara desktop companion installer: "+own.ID+"\n")) {
		return false, errors.New("同名 MCP 配置属于旧版或已被修改，已拒绝覆盖；请先预览卸载旧版，再安装新版")
	}
	if !reflect.DeepEqual(entry, expectedEntry(p)) || own.PayloadHash != digest {
		return false, errors.New("已安装插件的路径、运行时或版本内容不同，原配置保留；请先手动检查")
	}
	if own.HookHash != entryHash(expectedPermissionHook(p)) || permissionHookCount(doc, p) != 1 {
		return false, errors.New("已安装审批 hook 的路径或策略已改变，原配置保留；请手动检查")
	}
	if _, _, err := ownedBlockRange(snap.data, own, p); err != nil {
		return false, err
	}
	files, payloadDigest, err := readBundle(p.payload)
	if err != nil || len(files) == 0 || payloadDigest != digest {
		return false, errors.New("已安装插件内容已变化，原配置保留；请手动检查")
	}
	return true, nil
}
func permissionHookCount(doc map[string]any, p paths) int {
	hooks, _ := doc["hooks"].(map[string]any)
	groups, _ := hooks["PermissionRequest"].([]any)
	count := 0
	for _, group := range groups {
		if reflect.DeepEqual(group, expectedPermissionHook(p)) {
			count++
		}
	}
	return count
}

func sameOtherSettings(before, after map[string]any, p paths, version string) bool {
	deleteEntry := func(src map[string]any) map[string]any {
		out := make(map[string]any, len(src))
		for k, v := range src {
			out[k] = v
		}
		if servers, ok := src["mcp_servers"].(map[string]any); ok {
			copyServers := make(map[string]any, len(servers))
			for k, v := range servers {
				if k != ServerName {
					copyServers[k] = v
				}
			}
			if len(copyServers) == 0 {
				delete(out, "mcp_servers")
			} else {
				out["mcp_servers"] = copyServers
			}
		}
		if hasPermissionHook(version) {
			if hooks, ok := src["hooks"].(map[string]any); ok {
				copyHooks := make(map[string]any, len(hooks))
				for k, v := range hooks {
					copyHooks[k] = v
				}
				if groups, ok := hooks["PermissionRequest"].([]any); ok {
					var kept []any
					for _, group := range groups {
						if !reflect.DeepEqual(group, expectedPermissionHook(p)) {
							kept = append(kept, group)
						}
					}
					if len(kept) == 0 {
						delete(copyHooks, "PermissionRequest")
					} else {
						copyHooks["PermissionRequest"] = kept
					}
				}
				if len(copyHooks) == 0 {
					delete(out, "hooks")
				} else {
					out["hooks"] = copyHooks
				}
			}
		}
		return out
	}
	return reflect.DeepEqual(deleteEntry(before), deleteEntry(after))
}
func readConfig(path string) (snapshot, map[string]any, error) {
	old, err := readSnapshot(path)
	if err != nil {
		return old, nil, errors.New("原 Codex 配置不能安全读取，未修改任何配置")
	}
	var doc map[string]any
	if toml.Unmarshal(old.data, &doc) != nil {
		return old, nil, errors.New("原 Codex 配置不是有效 TOML，已拒绝修改")
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return old, doc, nil
}
func readSnapshot(path string) (snapshot, error) {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return snapshot{}, nil
	} else if err != nil {
		return snapshot{}, err
	}
	b, err := readRegular(path, maxFileBytes)
	return snapshot{exists: true, data: b}, err
}
func readRegular(path string, limit int64) ([]byte, error) {
	if rejectLinks(path) != nil {
		return nil, errors.New("unsafe path")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("not a bounded regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("file identity changed")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, errors.New("read failed or oversized")
	}
	return b, nil
}
func rejectLinks(path string) error {
	path = filepath.Clean(path)
	for {
		info, err := os.Lstat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlink")
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
	}
}
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
func hash(b []byte) string                  { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func entryHash(entry map[string]any) string { b, _ := json.Marshal(entry); return hash(b) }
func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func checkPrivateRoot(root string) error {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() {
		return errors.New("插件安装目录已被其他文件占用")
	}
	b, err := readRegular(filepath.Join(root, ".salcara-owner.json"), 4096)
	var marker struct {
		Owner  string `json:"owner"`
		Schema int    `json:"schema"`
	}
	if err != nil || json.Unmarshal(b, &marker) != nil || marker.Owner != rootOwner || marker.Schema != 1 {
		return errors.New("插件安装目录不是本安装器所有，已停止")
	}
	return nil
}
func ensurePrivateRoot(root string) error {
	if err := checkPrivateRoot(root); err != nil {
		return err
	}
	if _, err := os.Lstat(root); err == nil {
		return nil
	}
	if err := ensureParents(filepath.Dir(root)); err != nil {
		return errors.New("插件安装父目录不可安全写入")
	}
	if err := os.Mkdir(root, 0700); err != nil {
		return errors.New("插件安装目录并发创建或不可写入，已停止")
	}
	if err := setPrivatePermissions(root, true); err != nil {
		return errors.New("无法保护插件私有目录，已停止")
	}
	if err := writeExclusive(filepath.Join(root, ".salcara-owner.json"), []byte(`{"owner":"salcara-desktop-companion","schema":1}`)); err != nil {
		return errors.New("无法写入插件目录所有权标识，已停止")
	}
	return nil
}
func ensureParents(path string) error {
	if rejectLinks(path) != nil {
		return errors.New("unsafe parent")
	}
	return os.MkdirAll(path, 0700)
}
func privateDir(path string) error {
	if rejectLinks(path) != nil {
		return errors.New("unsafe private dir")
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return setPrivatePermissions(path, true)
}
func stagePayload(ctx context.Context, p paths, files []bundleFile) error {
	if _, err := os.Lstat(p.payload); err == nil {
		existing, _, readErr := readBundle(p.payload)
		if readErr != nil || !reflect.DeepEqual(existing, files) {
			return errors.New("同版本安装内容已存在且不同，已拒绝覆盖原版本")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("安装目录不可用")
	}
	stage, err := os.MkdirTemp(p.root, ".stage-")
	if err != nil {
		return errors.New("无法准备插件安装目录")
	}
	staged := true
	defer func() {
		// Only remove the exact private temporary directory we still own. After
		// rename, never delete a later directory created at the old stage name.
		if staged {
			_ = os.RemoveAll(stage)
		}
	}()
	if err = setPrivatePermissions(stage, true); err != nil {
		return errors.New("无法保护插件安装目录")
	}
	if err = privateDir(filepath.Join(stage, "src")); err != nil {
		return errors.New("无法准备插件源文件目录")
	}
	for _, f := range files {
		if ctx.Err() != nil {
			return errors.New("安装已取消，原配置未修改")
		}
		if writeExclusive(filepath.Join(stage, filepath.FromSlash(f.path)), f.data) != nil {
			return errors.New("无法复制插件必要文件，原配置未修改")
		}
	}
	if os.Rename(stage, p.payload) != nil {
		return errors.New("安装版本目录并发更改或不可写入，已停止")
	}
	staged = false
	return nil
}
func writeExclusive(path string, data []byte) error {
	if rejectLinks(path) != nil {
		return errors.New("unsafe file")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err = setPrivatePermissions(path, false); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	return err
}
func atomicWrite(path string, data []byte) error {
	return atomicWriteChecked(path, data, nil)
}
func atomicWriteChecked(path string, data []byte, check func() error) error {
	if rejectLinks(path) != nil {
		return errors.New("unsafe atomic file")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".salcara-write-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = setPrivatePermissions(tmp, false); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	// Check only after the temporary file has been flushed, immediately before
	// the atomic replacement. This is optimistic concurrency, not a claim that
	// arbitrary external editors honor a cross-process compare-and-swap lock.
	if check != nil {
		if err = check(); err != nil {
			return err
		}
	}
	return os.Rename(tmp, path)
}
func snapshotsEqual(a, b snapshot) bool { return a.exists == b.exists && bytes.Equal(a.data, b.data) }
func replaceIfUnchanged(path string, old snapshot, data []byte) error {
	// Never fall back to truncating the original file when atomic replacement
	// fails. The private original backup has already been persisted.
	err := atomicWriteChecked(path, data, func() error {
		current, err := readSnapshot(path)
		if err != nil || !snapshotsEqual(old, current) {
			return errConfigChanged
		}
		return nil
	})
	if errors.Is(err, errConfigChanged) {
		return err
	}
	if err != nil {
		return errors.New("无法原子写入 Codex 配置，原配置未覆盖")
	}
	return nil
}
func rollback(path string, old snapshot, installed []byte) error {
	current, err := readSnapshot(path)
	if err != nil || !current.exists || !bytes.Equal(current.data, installed) {
		return errors.New("Codex 配置已出现后续修改，未自动回滚以免覆盖；原始私有备份保留")
	}
	if old.exists {
		err = atomicWriteChecked(path, old.data, func() error {
			latest, readErr := readSnapshot(path)
			if readErr != nil || !latest.exists || !bytes.Equal(latest.data, installed) {
				return errConfigChanged
			}
			return nil
		})
		if err != nil {
			return errors.New("自动回滚未完成或配置出现后续修改，原始私有备份保留；请手动检查")
		}
	} else if os.Remove(path) != nil {
		return errors.New("自动回滚未完成，未删除其他配置或会话")
	}
	return nil
}
