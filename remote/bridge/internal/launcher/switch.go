package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/atomicfile"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/toolcfg"
)

type SwitchInfo struct {
	Target     string `json:"target"`
	Name       string `json:"name"`
	ProfileDir string `json:"profileDir"`
	AppData    string `json:"appData"`
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model"`
	Message    string `json:"message"`
}

// PrepareSwitch validates only. No new profile is allocated, and no original
// settings or data are written until the user confirms the switch.
func (s *Service) PrepareSwitch(ctx context.Context, a config.LocalAccount, target, workspace string, paths map[string]string) (Plan, error) {
	if target == "claude-desktop" {
		return Plan{}, toolcfg.ErrClaudeDesktopAutomaticAPIUnavailable
	}
	if err := config.ValidateLocalAccount(&a); err != nil {
		return Plan{}, err
	}
	var t Tool
	for _, x := range s.Tools(ctx, paths) {
		if x.ID == target {
			t = x
			break
		}
	}
	if !ValidTarget(t.ID) || t.Kind != a.Kind {
		return Plan{}, errors.New("账号与所选工具协议不匹配")
	}
	if !t.Available || !fileExists(t.Path) {
		return Plan{}, errors.New("没有找到所选工具，请先安装或设置程序路径")
	}
	if workspace == "" {
		workspace = a.Workspace
	}
	if workspace == "" {
		workspace, _ = os.UserHomeDir()
	}
	st, err := os.Stat(workspace)
	if !filepath.IsAbs(workspace) || err != nil || !st.IsDir() {
		return Plan{}, errors.New("项目文件夹不存在或不是绝对路径")
	}
	p := Plan{Tool: t, Shared: true, Workspace: filepath.Clean(workspace), RuntimeDir: filepath.Join(s.Dir, "launch-scratch", target)}
	env := map[string]string{}
	drop := []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "SUB2API_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "CLAUDE_CODE_OAUTH_TOKEN", "ELECTRON_RUN_AS_NODE"}
	if a.Kind == "codex" {
		p.ProfileDir = filepath.Dir(toolcfg.CodexConfigPath())
		_, base, _ := config.APIBase(a.BaseURL)
		b, e := os.ReadFile(toolcfg.CodexConfigPath())
		if e != nil && !os.IsNotExist(e) {
			return Plan{}, e
		}
		if _, e := toolcfg.SwitchCodexTOML(string(b), base, a.Key, a.Model); e != nil {
			return Plan{}, e
		}
		env["CODEX_HOME"], env["OPENAI_API_KEY"] = p.ProfileDir, a.Key
		if isDesktop(t.ID) {
			p.AppData = originalAppData(t)
		} else {
			p.Args = []string{"resume", "--all"}
			if a.Model != "" {
				p.Args = append(p.Args, "--model", a.Model)
			}
		}
	} else {
		p.ProfileDir = filepath.Dir(toolcfg.ClaudeSettingsPath())
		env["CLAUDE_CONFIG_DIR"], env["ANTHROPIC_BASE_URL"] = p.ProfileDir, a.BaseURL
		if a.Model != "" {
			env["ANTHROPIC_MODEL"] = a.Model
		}
		if a.AuthMode == "api-key" {
			env["ANTHROPIC_API_KEY"] = a.Key
		} else {
			env["ANTHROPIC_AUTH_TOKEN"] = a.Key
		}
		if isDesktop(t.ID) {
			p.AppData = originalAppData(t)
		} else {
			p.Args = []string{"--resume"}
			if a.Model != "" {
				p.Args = append(p.Args, "--model", a.Model)
			}
		}
	}
	if err := normalizeCLI(&p); err != nil {
		return Plan{}, err
	}
	p.Environment = agents.LocalEnvironment(p.Tool.Path, env, drop)
	return p, nil
}

func (s *Service) PreviewSwitch(ctx context.Context, a config.LocalAccount, target, workspace string, paths map[string]string) (SwitchInfo, error) {
	p, err := s.PrepareSwitch(ctx, a, target, workspace, paths)
	if err != nil {
		return SwitchInfo{}, err
	}
	info := SwitchInfo{Target: target, Name: p.Tool.Name, ProfileDir: p.ProfileDir, AppData: p.AppData, Model: a.Model, Message: "沿用原目录，只更新 API 地址、密钥和模型；不新建账号数据目录，不改写会话、项目文件或数据库。请先结束正在执行的任务、保存编辑内容。"}
	if a.Kind == "codex" {
		b, _ := os.ReadFile(toolcfg.CodexConfigPath())
		info.Provider, _, _, _ = toolcfg.InspectCodexTOML(string(b))
		if info.Provider == "" {
			info.Provider = "openai"
		}
	}
	if !isDesktop(target) {
		info.Message += " 终端中的旧进程请先手动退出；新终端会打开原有会话的恢复列表。"
	}
	return info, nil
}

func switchPaths(p Plan) ([]string, string) {
	if p.Tool.ID == "claude-desktop" {
		return []string{filepath.Join(p.AppData, "config.json"), filepath.Join(p.AppData, "configLibrary", "_meta.json"), filepath.Join(p.AppData, "configLibrary", toolcfg.SwitchClaudeProviderID+".json")}, "claude-desktop"
	}
	if p.Tool.Kind == "codex" {
		return []string{filepath.Join(p.ProfileDir, "config.toml"), filepath.Join(p.ProfileDir, "auth.json")}, "codex"
	}
	return []string{filepath.Join(p.ProfileDir, "settings.json")}, "claude"
}

func captureFiles(paths []string) ([]savedFile, error) {
	files := make([]savedFile, 0, len(paths))
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		files = append(files, savedFile{Path: path, Existed: err == nil, Data: b})
	}
	return files, nil
}

func restoreFiles(files []savedFile) error {
	for _, f := range files {
		if f.Existed {
			if err := atomicfile.WriteFileKeepMode(f.Path, f.Data, 0o600); err != nil {
				return err
			}
		} else if err := os.Remove(f.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// Switch closes only the original desktop window gracefully. It never force-kills
// tasks, copies a profile, or edits sessions. Failure rolls back to the immediately
// preceding credentials, not to some older account's credentials.
func (s *Service) Switch(ctx context.Context, a config.LocalAccount, target, workspace string, paths map[string]string) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.PrepareSwitch(ctx, a, target, workspace, paths)
	if err != nil {
		return Result{}, err
	}
	files, kind := switchPaths(p)
	previous, err := captureFiles(files)
	if err != nil {
		return Result{}, err
	}
	manifest := s.snapshotPath(kind)
	var originals []savedFile
	oldManifest, e := os.ReadFile(manifest)
	if e == nil {
		if json.Unmarshal(oldManifest, &originals) != nil || len(originals) != len(previous) {
			return Result{}, errors.New("配置备份无法读取，请检查 default-backups")
		}
		for i, f := range previous {
			if originals[i].Path != f.Path {
				return Result{}, errors.New("原工具数据路径已改变，已停止切号；请先检查或恢复上一份配置备份")
			}
			if originals[i].AppliedHash != "" && (!f.Existed || digest(f.Data) != originals[i].AppliedHash) {
				originals[i].ChangedExternally = true
			}
		}
	} else if os.IsNotExist(e) {
		originals = append([]savedFile{}, previous...)
	} else {
		return Result{}, e
	}
	if err := os.MkdirAll(p.RuntimeDir, 0o700); err != nil {
		return Result{}, err
	}
	// Prove backups can be persisted before asking the original app to quit.
	if err := saveSnapshot(manifest, originals); err != nil {
		return Result{}, err
	}
	if isDesktop(target) {
		if s.Stop == nil {
			return Result{}, errors.New("没有可用的安全退出方式，请先手动关闭原工具")
		}
		if err := s.Stop(ctx, p); err != nil {
			return Result{}, err
		}
	}
	// Capture after graceful quit as the original app may flush settings on exit.
	latest, err := captureFiles(files)
	if err != nil {
		return Result{}, err
	}
	for i := range latest {
		if previous[i].Existed != latest[i].Existed || digest(previous[i].Data) != digest(latest[i].Data) {
			originals[i].ChangedExternally = true
		}
	}
	previous = latest
	rollback := func(cause error) (Result, error) {
		if e := restoreFiles(previous); e != nil {
			return Result{}, fmt.Errorf("切号失败，自动回滚未完成，请检查 default-backups；没有删除会话：%w", e)
		}
		for i, f := range previous {
			if f.Existed {
				originals[i].AppliedHash = digest(f.Data)
			} else {
				originals[i].AppliedHash = ""
			}
		}
		if e := saveSnapshot(manifest, originals); e != nil {
			return Result{}, errors.New("凭据已回滚，但备份状态保存失败，请检查 default-backups")
		}
		return Result{}, fmt.Errorf("切号未完成，原凭据已回滚，原会话和文件未改动；可手动重开原工具：%w", cause)
	}
	_, v1, _ := config.APIBase(a.BaseURL)
	if target == "claude-desktop" {
		err = toolcfg.ApplySwitchClaudeDesktop(p.AppData, a.Name, a.BaseURL, a.Key, a.AuthMode, a.Model)
	} else if a.Kind == "codex" {
		err = toolcfg.ApplySwitchCodex(p.ProfileDir, v1, a.Key, a.Model)
		if err == nil {
			err = toolcfg.ApplyCodexCatalog(p.ProfileDir, a.CatalogOverride, catalogModels(a), a.Name)
		}
	} else {
		err = toolcfg.ApplyLocalClaude(files[0], a.BaseURL, a.Key, a.Model, a.AuthMode)
	}
	if err != nil {
		return rollback(err)
	}
	for i := range originals {
		b, e := os.ReadFile(originals[i].Path)
		if e != nil {
			return rollback(e)
		}
		originals[i].AppliedHash = digest(b)
	}
	if err := saveSnapshot(manifest, originals); err != nil {
		return rollback(err)
	}
	pid, err := s.Start(ctx, p)
	if err != nil {
		return rollback(err)
	}
	msg := "已切换并打开原工具，沿用原数据目录；聊天记录、项目文件未删除或迁移。"
	if !isDesktop(target) {
		msg += " 请从恢复列表选择原会话继续。"
	}
	msg += " 启动成功不代表服务商模型已经通过实际请求验证。"
	return Result{Target: target, PID: pid, ProfileDir: p.ProfileDir, Workspace: p.Workspace, Message: msg}, nil
}

func defaultAppData(t Tool) string {
	key, name := "CODEX_ELECTRON_USER_DATA_PATH", "Codex"
	if t.Kind == "claude" {
		key, name = "CLAUDE_USER_DATA_DIR", "Claude"
	}
	if path := os.Getenv(key); path != "" {
		if abs, err := filepath.Abs(path); err == nil {
			return abs
		}
	}
	root, _ := os.UserConfigDir()
	return filepath.Join(root, name)
}

// catalogModels is the list shown in the tool's own model picker: the API's
// catalog with the chosen default first.
func catalogModels(a config.LocalAccount) []string {
	out := []string{}
	if a.Model != "" {
		out = append(out, a.Model)
	}
	return append(out, a.Models...)
}
