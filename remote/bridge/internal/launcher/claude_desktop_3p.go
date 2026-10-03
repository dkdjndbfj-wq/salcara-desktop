package launcher

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/toolcfg"
)

// ClaudeDesktop3PService deliberately bypasses only the obsolete writer, never
// the application's managed policy or profile boundary. Platform is injectable
// for tests; production reads only distribution metadata and policy names.
type ClaudeDesktop3PService struct {
	Local    *Service
	Platform func(context.Context, Tool) (Claude3PPlatform, error)
}
type Claude3PPlatform struct {
	Root, Version string
	Managed       bool
}
type ClaudeDesktop3PStatus struct {
	Available          bool   `json:"available"`
	Supported          bool   `json:"supported"`
	Version            string `json:"version"`
	Verified           bool   `json:"verified"` // exact verified build; false = compatibility mode for a later build
	Managed            bool   `json:"managed"`
	Mode               string `json:"mode"`
	RequiresModeChange bool   `json:"requiresModeChange"`
	CatalogOverride    bool   `json:"catalogOverride"`
	ModelDiscovery     bool   `json:"modelDiscoveryEnabled"`
	Recoverable        bool   `json:"recoverable"`
	Message            string `json:"message"`
}
type claude3PBackup struct {
	Root      string      `json:"root"`
	ProfileID string      `json:"profileId"`
	Original  []savedFile `json:"original"`
	Applied   []savedFile `json:"applied"`
}

func NewClaudeDesktop3P(local *Service) *ClaudeDesktop3PService {
	return &ClaudeDesktop3PService{Local: local, Platform: inspectClaude3PPlatform}
}
func (s *ClaudeDesktop3PService) backupPath() string {
	return filepath.Join(s.Local.Dir, "default-backups", "claude-desktop-3p.json")
}
func (s *ClaudeDesktop3PService) inspect(ctx context.Context, paths map[string]string) (Tool, Claude3PPlatform, toolcfg.Claude3PState, ClaudeDesktop3PStatus, error) {
	status := ClaudeDesktop3PStatus{Mode: "unknown", Message: "尚未配置 Claude Desktop"}
	var tool Tool
	if s == nil || s.Local == nil || s.Platform == nil {
		return tool, Claude3PPlatform{}, toolcfg.Claude3PState{}, status, errors.New("Claude Desktop 配置服务不可用")
	}
	for _, t := range s.Local.Tools(ctx, paths) {
		if t.ID == "claude-desktop" {
			tool = t
			break
		}
	}
	status.Available = tool.Available
	if !tool.Available || !fileExists(tool.Path) {
		status.Message = "未找到 Claude Desktop，请先安装或设置程序路径"
		return tool, Claude3PPlatform{}, toolcfg.Claude3PState{}, status, errors.New(status.Message)
	}
	platform, err := s.Platform(ctx, tool)
	status.Version, status.Managed = platform.Version, platform.Managed
	if err != nil {
		status.Message = err.Error()
		return tool, platform, toolcfg.Claude3PState{}, status, err
	}
	compatible, verified := toolcfg.Claude3PVersionSupport(platform.Version)
	status.Verified = verified
	if !compatible {
		status.Message = "该 Claude Desktop 版本过旧或不在支持范围内，请更新 Claude Desktop 或使用原应用的第三方配置入口"
		return tool, platform, toolcfg.Claude3PState{}, status, errors.New(status.Message)
	}
	if platform.Managed {
		status.Message = "Claude Desktop 由组织策略管理；不会覆盖或绕过管理员配置"
		return tool, platform, toolcfg.Claude3PState{}, status, errors.New(status.Message)
	}
	state, err := toolcfg.InspectClaude3P(platform.Root)
	status.Mode, status.RequiresModeChange = state.Mode, state.RequiresModeChange
	status.CatalogOverride, status.ModelDiscovery = state.CatalogOverride, state.ModelDiscovery
	if err != nil {
		status.Message = err.Error()
		return tool, platform, state, status, err
	}
	status.Supported = true
	if _, err := s.readBackup(platform.Root); err == nil {
		status.Recoverable = true
	}
	status.Message = "沿用同一个 Claude-3p 本地聊天库；换 API 不创建独立实例，模型在 Claude 内选择。"
	if state.RequiresModeChange {
		status.Message = "将开启官方第三方模式，聊天保存在原 Claude-3p 本地库；标准账号聊天未删除，恢复原模式后查看。"
	}
	if !verified {
		status.Message = "Claude Desktop " + platform.Version + " 是比已验证版本更新的版本，按兼容模式运行：配置结构核对通过才会写入，切换前自动备份，可随时恢复。" + status.Message
	}
	return tool, platform, state, status, nil
}
func (s *ClaudeDesktop3PService) Status(ctx context.Context, paths map[string]string) ClaudeDesktop3PStatus {
	s.Local.mu.Lock()
	defer s.Local.mu.Unlock()
	_, _, _, status, _ := s.inspect(ctx, paths)
	return status
}

func (s *ClaudeDesktop3PService) Switch(ctx context.Context, a config.LocalAccount, paths map[string]string, allowModeChange bool) (Result, error) {
	s.Local.mu.Lock()
	defer s.Local.mu.Unlock()
	a.Kind, a.Target = "claude", "claude-desktop"
	if err := config.ValidateLocalAccount(&a); err != nil {
		return Result{}, err
	}
	if ((a.Wire != "" && a.Wire != "auto" && a.Wire != "anthropic") || (a.Protocol != "" && a.Protocol != "anthropic")) && !claudeDesktopLoopbackGateway(a) {
		return Result{}, errors.New("Claude Desktop 需要 Anthropic Messages 接口；跨协议 API 必须经过本机专用兼容网关")
	}
	tool, platform, state, _, err := s.inspect(ctx, paths)
	if err != nil {
		return Result{}, err
	}
	id := state.ProfileID
	if id == "" {
		// A failed first launch may retain a recoverable pre-change backup. Reuse
		// its identifier on retry rather than allocating a second profile.
		if retry, err := s.readBackup(platform.Root); err == nil {
			id = retry.ProfileID
		} else {
			id = config.NewUUID()
		}
	}
	gateway := toolcfg.Claude3PGateway{Name: a.Name, BaseURL: a.BaseURL, Key: a.Key, AuthMode: a.AuthMode, Models: a.Models, CatalogOverride: a.CatalogOverride}
	for _, route := range a.Models {
		if label, found := a.DesktopModelLabels[route]; found && label != route {
			gateway.CatalogEntries = append(gateway.CatalogEntries, toolcfg.Claude3PModelEntry{Name: route, LabelOverride: label})
		}
	}
	write, err := toolcfg.PrepareClaude3P(state, gateway, id, allowModeChange)
	if err != nil {
		return Result{}, err
	}
	previous, err := captureClaude3P(write.Paths())
	if err != nil {
		return Result{}, err
	}
	backup, backupErr := s.readBackup(platform.Root)
	first := os.IsNotExist(backupErr)
	if backupErr != nil && !first {
		return Result{}, backupErr
	}
	if first {
		backup = claude3PBackup{Root: platform.Root, ProfileID: write.ProfileID, Original: previous}
	} else if backup.ProfileID != write.ProfileID {
		return Result{}, errors.New("Claude 已应用配置已改变，请先检查或恢复上次备份")
	}
	if !first {
		if err := matchingClaude3P(previous, backup.Applied); err != nil {
			return Result{}, err
		}
	}
	p := s.plan(tool, platform.Root)
	if s.Local.Stop == nil || s.Local.Start == nil {
		return Result{}, errors.New("没有可用的安全退出与启动方式")
	}
	if err := os.MkdirAll(p.RuntimeDir, 0o700); err != nil {
		return Result{}, errors.New("无法准备 Claude 启动目录")
	}
	if first {
		backup.Applied = previous
		if err := s.saveBackup(backup); err != nil {
			return Result{}, err
		}
	}
	if err := s.Local.Stop(ctx, p); err != nil {
		if first {
			_ = os.Remove(s.backupPath())
		}
		return Result{}, errors.New("Claude 尚未正常退出，未替换 API；请保存任务并从原应用退出后重试")
	}
	// The app may flush settings during graceful quit. Re-read, revalidate policy,
	// then prepare again from that exact state before writing anything.
	_, again, state, _, err := s.inspect(ctx, paths)
	if err != nil {
		return Result{}, err
	}
	if again.Root != platform.Root || state.ProfileID != write.State.ProfileID {
		return Result{}, errors.New("退出过程中 Claude 配置路径或配置编号变化，未写入")
	}
	write, err = toolcfg.PrepareClaude3P(state, gateway, id, allowModeChange)
	if err != nil {
		return Result{}, err
	}
	previous, err = captureClaude3P(write.Paths())
	if err != nil {
		return Result{}, err
	}
	if !first {
		if err := matchingClaude3P(previous, backup.Applied); err != nil {
			return Result{}, err
		}
	} else {
		backup.Original = previous
		backup.Applied = previous
		if err := s.saveBackup(backup); err != nil {
			return Result{}, err
		}
	}
	rollback := func() (Result, error) {
		if err := restoreClaude3P(previous); err != nil {
			return Result{}, errors.New("Claude 配置失败且回滚未完成；配置备份仍在本机，会话未删除")
		}
		backup.Applied = previous
		_ = s.saveBackup(backup)
		return Result{}, errors.New("Claude 配置或启动未完成，原配置已回滚；请手动重开原应用")
	}
	if err := write.Apply(); err != nil {
		return rollback()
	}
	backup.Applied, err = captureClaude3P(write.Paths())
	if err != nil {
		return rollback()
	}
	if err := s.saveBackup(backup); err != nil {
		return rollback()
	}
	pid, err := s.Local.Start(ctx, p)
	if err != nil {
		return rollback()
	}
	return Result{Target: "claude-desktop", PID: pid, ProfileDir: platform.Root, Workspace: p.Workspace, Message: "已配置并打开 Claude Desktop，沿用同一个 Claude-3p 本地库；模型在原应用内选择。标准账号聊天保留在原模式。启动成功不代表上游推理已验证。"}, nil
}

// Only this application's dedicated loopback route may substitute its native
// Anthropic interface for an upstream wire protocol. Never permit a remote URL
// to claim protocol conversion merely because it looks like a gateway path.
func claudeDesktopLoopbackGateway(a config.LocalAccount) bool {
	u, err := url.Parse(a.BaseURL)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimRight(u.EscapedPath(), "/") != "/gateway/claude-desktop" || a.AuthMode != "bearer" {
		return false
	}
	port, err := strconv.Atoi(u.Port())
	return err == nil && port > 0 && port <= 65535
}

func (s *ClaudeDesktop3PService) Restore(ctx context.Context, paths map[string]string) (Result, error) {
	s.Local.mu.Lock()
	defer s.Local.mu.Unlock()
	tool, platform, _, _, err := s.inspect(ctx, paths)
	if err != nil {
		return Result{}, err
	}
	backup, err := s.readBackup(platform.Root)
	if err != nil {
		return Result{}, errors.New("没有可用的 Claude Desktop 原配置备份")
	}
	current, err := captureClaude3P(claude3PPaths(backup))
	if err != nil {
		return Result{}, err
	}
	if err := matchingClaude3P(current, backup.Applied); err != nil {
		return Result{}, err
	}
	p := s.plan(tool, platform.Root)
	if s.Local.Stop == nil || s.Local.Start == nil {
		return Result{}, errors.New("没有可用的安全退出与启动方式")
	}
	if err := s.Local.Stop(ctx, p); err != nil {
		return Result{}, errors.New("Claude 尚未正常退出，未恢复配置；请保存任务并正常退出后重试")
	}
	_, check, _, _, err := s.inspect(ctx, paths)
	if err != nil || check.Root != platform.Root {
		return Result{}, errors.New("Claude 配置状态改变，未恢复")
	}
	current, err = captureClaude3P(claude3PPaths(backup))
	if err != nil {
		return Result{}, err
	}
	if err := matchingClaude3P(current, backup.Applied); err != nil {
		return Result{}, err
	}
	if err := restoreClaude3P(backup.Original); err != nil {
		_ = restoreClaude3P(current)
		return Result{}, errors.New("Claude 原配置恢复失败，未删除聊天库")
	}
	pid, err := s.Local.Start(ctx, p)
	if err != nil {
		_ = restoreClaude3P(current)
		return Result{}, errors.New("Claude 重新打开失败，切换前配置已回滚；请手动重开原应用")
	}
	if err := os.Remove(s.backupPath()); err != nil {
		return Result{Target: "claude-desktop", PID: pid, Message: "原配置已恢复并重新打开，但本机备份标记未能移除。"}, nil
	}
	return Result{Target: "claude-desktop", PID: pid, ProfileDir: platform.Root, Workspace: p.Workspace, Message: "已恢复原配置并重新打开 Claude；第三方聊天文件没有删除，标准账号聊天仍在原模式。"}, nil
}

func (s *ClaudeDesktop3PService) plan(tool Tool, root string) Plan {
	cwd, _ := os.UserHomeDir()
	if cwd == "" {
		cwd = s.Local.Dir
	}
	drop := []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "SUB2API_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "CLAUDE_CONFIG_DIR", "CLAUDE_USER_DATA_DIR", "CLAUDE_CODE_OAUTH_TOKEN", "ELECTRON_RUN_AS_NODE"}
	return Plan{Tool: tool, Workspace: cwd, ProfileDir: root, AppData: root, RuntimeDir: filepath.Join(s.Local.Dir, "launch-scratch", "claude-desktop-3p"), Shared: true, Environment: agents.LocalEnvironment(tool.Path, nil, drop)}
}
func captureClaude3P(paths []string) ([]savedFile, error) {
	out := []savedFile{}
	for _, path := range paths {
		data, exists, err := toolcfg.ReadClaude3PFile(path)
		if err != nil {
			return nil, err
		}
		out = append(out, savedFile{Path: path, Existed: exists, Data: data, AppliedHash: digest(data)})
	}
	return out, nil
}
func restoreClaude3P(files []savedFile) error {
	for _, f := range files {
		if err := toolcfg.ValidateClaude3PPath(f.Path); err != nil {
			return err
		}
		if f.Existed {
			if err := toolcfg.WriteClaude3PFile(f.Path, f.Data); err != nil {
				return err
			}
		} else {
			if err := os.Remove(f.Path); err != nil && !os.IsNotExist(err) {
				return errors.New("无法移除本次创建的 Claude 配置文件")
			}
		}
	}
	return nil
}
func matchingClaude3P(current, applied []savedFile) error {
	if len(current) != len(applied) {
		return errors.New("Claude 配置备份不匹配")
	}
	for i, f := range current {
		a := applied[i]
		if f.Path != a.Path || f.Existed != a.Existed || digest(f.Data) != digest(a.Data) {
			return errors.New("Claude 配置在上次应用后被修改，已停止覆盖；请检查本机备份或在原应用配置")
		}
	}
	return nil
}
func claude3PPaths(b claude3PBackup) []string {
	return []string{filepath.Join(b.Root, "claude_desktop_config.json"), filepath.Join(b.Root, "configLibrary", "_meta.json"), filepath.Join(b.Root, "configLibrary", b.ProfileID+".json")}
}
func (s *ClaudeDesktop3PService) readBackup(root string) (claude3PBackup, error) {
	var b claude3PBackup
	data, exists, err := toolcfg.ReadClaude3PFileLimit(s.backupPath(), 10<<20)
	if err != nil {
		return b, err
	}
	if !exists {
		return b, os.ErrNotExist
	}
	if json.Unmarshal(data, &b) != nil || b.Root != root || len(b.Original) != 3 || len(b.Applied) != 3 {
		return b, errors.New("Claude 原配置备份无法安全识别")
	}
	if !toolcfg.ValidClaudeDesktopProfileID(b.ProfileID) {
		return b, errors.New("Claude 原配置备份编号无效")
	}
	paths := claude3PPaths(b)
	for i := range paths {
		if b.Original[i].Path != paths[i] || b.Applied[i].Path != paths[i] {
			return b, errors.New("Claude 原配置备份路径不匹配")
		}
	}
	return b, nil
}
func (s *ClaudeDesktop3PService) saveBackup(b claude3PBackup) error {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return errors.New("无法准备 Claude 配置备份")
	}
	if len(data) > 10<<20 {
		return errors.New("Claude 配置备份超过安全容量，未继续切换")
	}
	return toolcfg.WriteClaude3PFile(s.backupPath(), append(data, '\n'))
}

func inspectClaude3PPlatform(ctx context.Context, tool Tool) (Claude3PPlatform, error) {
	p := Claude3PPlatform{}
	if os.Getenv("CLAUDE_USER_DATA_DIR") != "" {
		return p, errors.New("检测到 Claude 自定义数据目录，无法确定原聊天库；请使用原应用配置入口")
	}
	root, asar, err := claude3PLocations(runtime.GOOS, tool.Path)
	if err != nil {
		return p, err
	}
	p.Root = root
	version, err := distributionClaudeVersion(asar)
	if err != nil {
		return p, err
	}
	p.Version = version
	p.Managed, err = claudeManagedPolicy(ctx)
	return p, err
}

// claude3PLocations returns the 3P data root and the distribution archive.
// Windows: %LOCALAPPDATA%\Claude-3p and <exe dir>\resources\app.asar.
// macOS: ~/Library/Application Support/Claude-3p and
// Claude.app/Contents/Resources/app.asar. Both are only candidates: the
// distribution signature (package name/product) and every configuration file
// are still validated, and anything unexpected fails closed.
func claude3PLocations(goos, exe string) (root, asar string, err error) {
	switch goos {
	case "windows":
		local := os.Getenv("LOCALAPPDATA")
		if !filepath.IsAbs(local) {
			return "", "", errors.New("无法确认 Claude 官方第三方配置目录")
		}
		return filepath.Join(local, "Claude-3p"), filepath.Join(filepath.Dir(exe), "resources", "app.asar"), nil
	case "darwin":
		home, e := os.UserHomeDir()
		if e != nil || !filepath.IsAbs(home) {
			return "", "", errors.New("无法确认 Claude 官方第三方配置目录")
		}
		// exe is Claude.app/Contents/MacOS/Claude.
		contents := filepath.Dir(filepath.Dir(exe))
		if filepath.Base(contents) != "Contents" || !strings.HasSuffix(strings.ToLower(filepath.Dir(contents)), ".app") {
			return "", "", errors.New("所选程序不是 Claude.app，请在原应用配置")
		}
		return filepath.Join(home, "Library", "Application Support", "Claude-3p"), filepath.Join(contents, "Resources", "app.asar"), nil
	default:
		return "", "", errors.New("Claude Desktop 自动第三方配置支持 Windows 与 macOS；请使用原应用配置入口")
	}
}

func distributionClaudeVersion(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("无法核验 Claude Desktop 官方分发版本，请在原应用配置")
	}
	defer f.Close()
	prefix := make([]byte, 16)
	if _, err := io.ReadFull(f, prefix); err != nil {
		return "", errors.New("Claude 分发元数据不可读")
	}
	size := binary.LittleEndian.Uint32(prefix[12:])
	if size == 0 || size > 4<<20 {
		return "", errors.New("Claude 分发元数据大小异常")
	}
	header := make([]byte, size)
	if _, err := io.ReadFull(f, header); err != nil {
		return "", errors.New("Claude 分发元数据不可读")
	}
	var archive struct {
		Files map[string]struct {
			Size     int64  `json:"size"`
			Offset   string `json:"offset"`
			Unpacked bool   `json:"unpacked"`
		} `json:"files"`
	}
	if json.Unmarshal(header, &archive) != nil {
		return "", errors.New("Claude 分发元数据无效")
	}
	entry, ok := archive.Files["package.json"]
	if !ok || entry.Unpacked || entry.Size <= 0 || entry.Size > 1<<20 {
		return "", errors.New("Claude 版本元数据异常")
	}
	var offset int64
	for _, c := range entry.Offset {
		if c < '0' || c > '9' {
			return "", errors.New("Claude 版本元数据无效")
		}
		offset = offset*10 + int64(c-'0')
		if offset > 1<<32 {
			return "", errors.New("Claude 版本元数据无效")
		}
	}
	data := make([]byte, entry.Size)
	if _, err := f.ReadAt(data, 8+int64(binary.LittleEndian.Uint32(prefix[4:]))+offset); err != nil {
		return "", errors.New("Claude 版本信息无法读取")
	}
	var pkg struct{ Name, ProductName, Version string }
	if json.Unmarshal(data, &pkg) != nil || pkg.Name != "@ant/desktop" || pkg.ProductName != "Claude" {
		return "", errors.New("所选程序不是已核验的 Claude Desktop 分发")
	}
	return pkg.Version, nil
}
