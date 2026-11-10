package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/toolcfg"
)

// Plan contains secrets and must never be serialized or logged. Result is safe.
type Plan struct {
	Tool                           Tool
	Workspace, ProfileDir, AppData string
	RuntimeDir                     string // helpers belong to Bridge, never the original tool's data directory
	Shared                         bool
	Environment                    []string
	Args                           []string
	ClaudeSettingsPin              string // private per-launch API settings; terminal supervisor owns cleanup after start
}

type Result struct {
	Uncertain  bool   `json:"uncertain,omitempty"`
	Submitted  bool   `json:"submitted,omitempty"`
	Target     string `json:"target"`
	PID        int    `json:"pid"`
	ProfileDir string `json:"profileDir"`
	Workspace  string `json:"workspace"`
	Message    string `json:"message"`
}

type Service struct {
	Dir string
	// Start is injectable to verify the complete launch plan without opening apps.
	Start           func(context.Context, Plan) (int, error)
	FindTools       func(context.Context, map[string]string) []Tool
	Stop            func(context.Context, Plan) error
	mu              sync.Mutex
	inventoryMu     sync.Mutex
	inventoryKey    string
	inventoryAt     time.Time
	inventory       []Tool
	inventoryFlight *inventoryProbe
	inventoryEpoch  uint64
	locationsMu     sync.Mutex
	locations       map[string]toolLocation
}

func New(dir string) *Service {
	s := &Service{Dir: dir, Start: startPlatform, FindTools: Discover, Stop: stopPlatform}
	s.loadLocations()
	return s
}

func (s *Service) Tools(ctx context.Context, paths map[string]string) []Tool {
	var tools []Tool
	if s.FindTools != nil {
		tools = s.FindTools(ctx, paths)
	} else {
		tools = Discover(ctx, paths)
	}
	var deepSeekBefore Tool
	for _, tool := range tools {
		if tool.ID == "deepseek-harness" {
			deepSeekBefore = tool
			break
		}
	}
	tools = s.applyLocations(tools, paths)
	for i := range tools {
		tool := &tools[i]
		if tool.ID != "deepseek-harness" || !tool.Available || deepSeekBefore.Available {
			continue
		}
		// A remembered file location is not proof that its runtime still works.
		// Reuse a failed check for the same path instead of probing it twice.
		tool.runtimeError = deepSeekBefore.runtimeError
		if tool.Path != deepSeekBefore.Path || tool.runtimeError == nil {
			tool.runtimeError = agents.CheckDeepSeekRuntime(ctx, tool.Path)
		}
		tool.Available = tool.runtimeError == nil
	}
	s.rememberLocations(tools)
	return tools
}

func (s *Service) Prepare(a config.LocalAccount, t Tool, workspace string) (p Plan, err error) {
	return s.prepare(context.Background(), a, t, workspace)
}

func (s *Service) prepare(ctx context.Context, a config.LocalAccount, t Tool, workspace string) (p Plan, err error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}
	if t.ID == "claude-desktop" {
		return Plan{}, toolcfg.ErrClaudeDesktopAutomaticAPIUnavailable
	}
	if err := config.ValidateLocalAccount(&a); err != nil {
		return Plan{}, err
	}
	if !ValidTarget(t.ID) || t.Kind != a.Kind {
		return Plan{}, errors.New("这个账号不能启动所选工具")
	}
	if !t.Available || !fileExists(t.Path) {
		if t.runtimeError != nil {
			return Plan{}, t.runtimeError
		}
		return Plan{}, fmt.Errorf("没有找到 %s，请先安装或设置程序路径", toolNames[t.ID])
	}
	if a.Model == "" {
		return Plan{}, errors.New("请先为这个账号选择或填写默认模型")
	}
	if !config.ModelAllowed(a, a.Model) {
		return Plan{}, errors.New("当前 API 的模型规则拒绝了默认模型，请重新选择模型")
	}
	if workspace == "" {
		workspace = a.Workspace
	}
	if workspace == "" {
		workspace, _ = os.UserHomeDir()
	}
	if !filepath.IsAbs(workspace) {
		return Plan{}, errors.New("项目文件夹需要绝对路径")
	}
	st, err := os.Stat(workspace)
	if err != nil || !st.IsDir() {
		return Plan{}, errors.New("项目文件夹不存在或不是文件夹")
	}
	root, err := filepath.Abs(filepath.Join(s.Dir, "instances", a.ID))
	if err != nil {
		return Plan{}, err
	}
	p = Plan{Tool: t, Workspace: filepath.Clean(workspace), ProfileDir: filepath.Join(root, a.Kind), AppData: filepath.Join(root, t.ID+"-app"), RuntimeDir: filepath.Join(s.Dir, "launch-scratch", t.ID)}
	// Both Codex and Claude preparations can fail after writing credentials
	// (for example while resolving an npm shim). Restore the exact previous
	// Bridge-owned files, including their absence, on every failed preparation.
	before, err := captureFiles(isolatedProfileFiles(p.ProfileDir, a.Kind))
	if err != nil {
		return Plan{}, err
	}
	defer func() {
		if err != nil {
			if restoreErr := restoreFiles(before); restoreErr != nil {
				err = fmt.Errorf("工具配置准备失败且回滚失败：%w（原错误：%v）", restoreErr, err)
			}
		}
	}()
	_, v1, _ := config.APIBase(a.BaseURL)
	env := map[string]string{}
	drop := []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "SUB2API_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "CLAUDE_CODE_OAUTH_TOKEN", "ELECTRON_RUN_AS_NODE"}
	if a.Kind == "codex" {
		if a.CatalogOverride {
			if _, err := toolcfg.CodexCatalogJSON(p.ProfileDir, catalogModels(a), a.Name); err != nil {
				return Plan{}, err
			}
		}
		if err := toolcfg.ApplyLocalCodex(p.ProfileDir, v1, a.Key, a.Model); err != nil {
			return Plan{}, err
		}
		// An isolated launch has its own CODEX_HOME, so the system Codex
		// catalogue cannot be relied on here.  When the user enabled the
		// per-card picker override, publish the verified upstream catalogue
		// into this instance as well. ApplyCodexCatalog only removes the
		// Bridge-owned pointer when the option is off; the snapshot above
		// protects the previous catalogue if preparation fails. An empty opted-in catalogue fails closed
		// instead of opening a picker that advertises no usable models.
		if err := toolcfg.ApplyCodexCatalog(p.ProfileDir, a.CatalogOverride, catalogModels(a), a.Name); err != nil {
			return Plan{}, err
		}
		env["CODEX_HOME"], env["OPENAI_API_KEY"] = p.ProfileDir, a.Key
		if t.ID == "codex-desktop" {
			env["CODEX_ELECTRON_USER_DATA_PATH"] = p.AppData
			p.Args = []string{"--user-data-dir=" + p.AppData}
		}
	} else if config.IsAdditionalTool(a.Kind) {
		if err := applyAdditional(p, a); err != nil {
			return Plan{}, err
		}
		additionalPlan(&p, a, env)
	} else {
		if err := toolcfg.ApplyLocalClaude(filepath.Join(p.ProfileDir, "settings.json"), a.BaseURL, a.Key, a.Model, a.AuthMode); err != nil {
			return Plan{}, err
		}
		env = toolcfg.ClaudeAPIEnvironment(a.BaseURL, a.Key, a.AuthMode)
		env["CLAUDE_CONFIG_DIR"], env["ANTHROPIC_MODEL"] = p.ProfileDir, a.Model
		p.Args = []string{"--model", a.Model, "--settings", filepath.Join(p.ProfileDir, "settings.json")}
	}
	if err := os.MkdirAll(p.AppData, 0o700); err != nil {
		return Plan{}, err
	}
	if err := normalizeCLIContext(ctx, &p); err != nil {
		return Plan{}, err
	}
	p.Environment = agents.LocalEnvironment(p.Tool.Path, env, drop)
	return p, nil
}

func isolatedProfileFiles(dir, kind string) []string {
	paths := []string{filepath.Join(dir, "settings.json")}
	if config.IsAdditionalTool(kind) {
		paths = toolcfg.AdditionalPaths(kind, dir)
	}
	if kind == "codex" {
		paths = []string{filepath.Join(dir, "config.toml"), filepath.Join(dir, "auth.json"), toolcfg.CodexCatalogPath(dir)}
	}
	// Tool writers may create one-time backups while preparing an existing
	// profile. Those sidecars belong to the same transaction as their files.
	for _, path := range append([]string(nil), paths...) {
		paths = append(paths, path+toolcfg.BackupSuffix)
	}
	return paths
}

func normalizeCLI(p *Plan) error {
	return normalizeCLIContext(context.Background(), p)
}

func normalizeCLIContext(ctx context.Context, p *Plan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if isDesktop(p.Tool.ID) {
		return nil
	}
	if p.Tool.Kind == "deepseek-harness" {
		cmd, err := agents.DeepSeekCommand(ctx, p.Tool.Path, p.Args...)
		if err != nil {
			return err
		}
		p.Tool.Path, p.Args = cmd.Path, append([]string(nil), cmd.Args[1:]...)
		return nil
	}
	ext := strings.ToLower(filepath.Ext(p.Tool.Path))
	if ext != ".cmd" && ext != ".bat" {
		return nil
	}
	if p.Tool.Kind == "claude" {
		if native, ok := agents.LocalExecutable("claude", p.Tool.Path); ok && native != p.Tool.Path {
			p.Tool.Path = native
			return nil
		}
	}
	// npm shims feed arguments back through cmd.exe. Run their JS entry directly
	// so model identifiers are never interpreted as shell commands.
	pkg := "@anthropic-ai/claude-code/cli.js"
	if p.Tool.Kind == "codex" {
		pkg = "@openai/codex/bin/codex.js"
	} else if p.Tool.Kind == "hermes" {
		return errors.New("请设置 Hermes 原生可执行文件路径（venv/Scripts/hermes.exe），不支持 CMD/BAT 包装脚本")
	}
	dir := filepath.Dir(p.Tool.Path)
	for _, root := range []string{filepath.Join(dir, "node_modules"), filepath.Dir(dir)} {
		entry := filepath.Join(root, filepath.FromSlash(pkg))
		if fileExists(entry) {
			node, ok := agents.LocalExecutable("node", "")
			if !ok {
				return errors.New("这个 npm 工具需要 Node.js，请先安装 Node.js（https://nodejs.org）或改用官方原生 CLI 可执行文件")
			}
			p.Tool.Path = node
			p.Args = append([]string{entry}, p.Args...)
			return nil
		}
	}
	return errors.New("无法解析这个 CMD/BAT 工具入口，请选择原生可执行文件或标准 npm 安装的 CLI")
}

func (s *Service) Launch(ctx context.Context, a config.LocalAccount, target, workspace string, paths map[string]string) (Result, error) {
	return s.LaunchAuthorized(ctx, a, target, workspace, paths, nil)
}

// LaunchAuthorized checks a caller-owned account/binding snapshot immediately
// before starting. The hook runs after waiting for the launch lock and tool
// discovery, so a queued launch cannot use an already superseded account. The
// hook is a final validation boundary, not an atomic lock on the caller's store.
func (s *Service) LaunchAuthorized(ctx context.Context, a config.LocalAccount, target, workspace string, paths map[string]string, beforeStart func() error) (result Result, err error) {
	defer func() { err = RedactLaunchError(err, a.Key) }()
	if target == "claude-desktop" {
		return Result{}, toolcfg.ErrClaudeDesktopAutomaticAPIUnavailable
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := lockLaunch(ctx, &s.mu); err != nil {
		return Result{}, err
	}
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if err := config.ValidateLocalAccount(&a); err != nil {
		return Result{}, err
	}
	LaunchPhase(ctx, "tool", "检查程序与项目目录")
	var t Tool
	for _, x := range s.Tools(ctx, paths) {
		if x.ID == target {
			t = x
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	profileDir, err := filepath.Abs(filepath.Join(s.Dir, "instances", a.ID, a.Kind))
	if err != nil {
		return Result{}, err
	}
	before, err := captureFiles(isolatedProfileFiles(profileDir, a.Kind))
	if err != nil {
		return Result{}, err
	}
	LaunchPhase(ctx, "profile", "写入 API 与模型配置")
	p, err := s.prepare(ctx, a, t, workspace)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		if err != nil {
			if restoreErr := restoreFiles(before); restoreErr != nil {
				err = fmt.Errorf("启动工具失败且配置回滚失败：%w（原错误：%v）", restoreErr, err)
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	LaunchPhase(ctx, "authorize", "核对并保存连接授权")
	if beforeStart != nil {
		if err := beforeStart(); err != nil {
			return Result{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	LaunchPhase(ctx, "start", "创建工具进程")
	pid, err := s.Start(ctx, p)
	if err != nil {
		return Result{}, err
	}
	if !isDesktop(target) && pid == 0 {
		return Result{Target: target, Submitted: true, Uncertain: true, ProfileDir: p.ProfileDir, Workspace: p.Workspace, Message: "独立账号实例启动请求已提交；进程状态尚未确认，请先核对原工具，不会自动重发。"}, nil
	}
	return Result{Target: target, Submitted: true, PID: pid, ProfileDir: p.ProfileDir, Workspace: p.Workspace, Message: "已启动独立账号实例；不会替换系统默认配置。API 请求是否成功取决于服务商对所选模型及协议的支持。"}, nil
}

// A successful spawn is not proof that an upstream model can answer a request.
func startDirect(p Plan, terminal bool) (int, error) {
	return startDirectContext(context.Background(), p, terminal)
}

func startDirectContext(ctx context.Context, p Plan, terminal bool) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	cmd := exec.Command(p.Tool.Path, p.Args...)
	cmd.Dir, cmd.Env = p.Workspace, p.Environment
	configureProcess(cmd, terminal)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("启动 %s 失败：%w", p.Tool.Name, err)
	}
	LaunchPhase(ctx, "confirm", "等待进程启动确认", "检查进程是否立即退出")
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return 0, fmt.Errorf("%s 启动后立即退出：%w", p.Tool.Name, err)
		}
	case <-time.After(800 * time.Millisecond):
	}
	return cmd.Process.Pid, nil
}

func isDesktop(id string) bool { return strings.HasSuffix(id, "-desktop") }
