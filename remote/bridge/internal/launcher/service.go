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
}

type Result struct {
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
	tools = s.applyLocations(tools, paths)
	s.rememberLocations(tools)
	return tools
}

func (s *Service) Prepare(a config.LocalAccount, t Tool, workspace string) (Plan, error) {
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
		return Plan{}, fmt.Errorf("没有找到 %s，请先安装或设置程序路径", toolNames[t.ID])
	}
	if a.Model == "" {
		return Plan{}, errors.New("请先为这个账号选择或填写默认模型")
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
	p := Plan{Tool: t, Workspace: filepath.Clean(workspace), ProfileDir: filepath.Join(root, a.Kind), AppData: filepath.Join(root, t.ID+"-app")}
	_, v1, _ := config.APIBase(a.BaseURL)
	env := map[string]string{}
	drop := []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "SUB2API_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "CLAUDE_CODE_OAUTH_TOKEN", "ELECTRON_RUN_AS_NODE"}
	if a.Kind == "codex" {
		if err := toolcfg.ApplyLocalCodex(p.ProfileDir, v1, a.Key, a.Model); err != nil {
			return Plan{}, err
		}
		env["CODEX_HOME"], env["OPENAI_API_KEY"] = p.ProfileDir, a.Key
		if t.ID == "codex-desktop" {
			env["CODEX_ELECTRON_USER_DATA_PATH"] = p.AppData
			p.Args = []string{"--user-data-dir=" + p.AppData}
		}
	} else {
		if err := toolcfg.ApplyLocalClaude(filepath.Join(p.ProfileDir, "settings.json"), a.BaseURL, a.Key, a.Model, a.AuthMode); err != nil {
			return Plan{}, err
		}
		env["CLAUDE_CONFIG_DIR"], env["ANTHROPIC_BASE_URL"], env["ANTHROPIC_MODEL"] = p.ProfileDir, a.BaseURL, a.Model
		if a.AuthMode == "api-key" {
			env["ANTHROPIC_API_KEY"] = a.Key
		} else {
			env["ANTHROPIC_AUTH_TOKEN"] = a.Key
		}
		p.Args = []string{"--model", a.Model, "--settings", filepath.Join(p.ProfileDir, "settings.json")}
	}
	if err := os.MkdirAll(p.AppData, 0o700); err != nil {
		return Plan{}, err
	}
	if err := normalizeCLI(&p); err != nil {
		return Plan{}, err
	}
	p.Environment = agents.LocalEnvironment(p.Tool.Path, env, drop)
	return p, nil
}

func normalizeCLI(p *Plan) error {
	if isDesktop(p.Tool.ID) {
		return nil
	}
	ext := strings.ToLower(filepath.Ext(p.Tool.Path))
	if ext != ".cmd" && ext != ".bat" {
		return nil
	}
	// npm shims feed arguments back through cmd.exe. Run their JS entry directly
	// so model identifiers are never interpreted as shell commands.
	pkg := "@anthropic-ai/claude-code/cli.js"
	if p.Tool.Kind == "codex" {
		pkg = "@openai/codex/bin/codex.js"
	}
	dir := filepath.Dir(p.Tool.Path)
	for _, root := range []string{filepath.Join(dir, "node_modules"), filepath.Dir(dir)} {
		entry := filepath.Join(root, filepath.FromSlash(pkg))
		if fileExists(entry) {
			node, ok := agents.LocalExecutable("node", "")
			if !ok {
				return errors.New("这个 npm 工具需要 Node.js，请先安装 Node.js 或使用官方原生 CLI")
			}
			p.Tool.Path = node
			p.Args = append([]string{entry}, p.Args...)
			return nil
		}
	}
	return errors.New("无法解析这个 CMD/BAT 工具入口，请选择原生可执行文件或标准 npm 安装的 CLI")
}

func (s *Service) Launch(ctx context.Context, a config.LocalAccount, target, workspace string, paths map[string]string) (Result, error) {
	if target == "claude-desktop" {
		return Result{}, toolcfg.ErrClaudeDesktopAutomaticAPIUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var t Tool
	for _, x := range s.Tools(ctx, paths) {
		if x.ID == target {
			t = x
			break
		}
	}
	p, err := s.Prepare(a, t, workspace)
	if err != nil {
		return Result{}, err
	}
	pid, err := s.Start(ctx, p)
	if err != nil {
		return Result{}, err
	}
	return Result{Target: target, PID: pid, ProfileDir: p.ProfileDir, Workspace: p.Workspace, Message: "已启动独立账号实例；不会替换系统默认配置。API 请求是否成功取决于服务商对所选模型及协议的支持。"}, nil
}

// A successful spawn is not proof that an upstream model can answer a request.
func startDirect(p Plan, terminal bool) (int, error) {
	cmd := exec.Command(p.Tool.Path, p.Args...)
	cmd.Dir, cmd.Env = p.Workspace, p.Environment
	configureProcess(cmd, terminal)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("启动 %s 失败，请检查程序路径和权限", p.Tool.Name)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return 0, fmt.Errorf("%s 启动后立即退出，请检查版本、路径或查看工具自己的日志", p.Tool.Name)
		}
	case <-time.After(800 * time.Millisecond):
	}
	return cmd.Process.Pid, nil
}

func isDesktop(id string) bool { return strings.HasSuffix(id, "-desktop") }
