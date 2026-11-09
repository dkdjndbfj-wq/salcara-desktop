package launcher

import (
	"context"
	"errors"
	"os"
	"regexp"

	"salcara/bridge/internal/config"
)

var sessionID = regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)

// Resume opens the exact original session. It never creates/forks a thread or
// rewrites API config, and never silently falls back to a new session.
func (s *Service) Resume(ctx context.Context, a config.LocalAccount, target, id, cwd string, paths map[string]string) (result Result, launchErr error) {
	defer func() { launchErr = RedactLaunchError(launchErr, a.Key) }()
	if err := lockLaunch(ctx, &s.mu); err != nil {
		return Result{}, err
	}
	defer s.mu.Unlock()
	LaunchPhase(ctx, "tool", "检查程序与项目目录")
	if (target != "codex" && target != "claude") || !sessionID.MatchString(id) {
		return Result{}, errors.New("会话编号或恢复工具无效")
	}
	if err := config.ValidateLocalAccount(&a); err != nil {
		return Result{}, err
	}
	p, err := s.PrepareSwitch(ctx, a, target, cwd, paths)
	if err != nil {
		return Result{}, err
	}
	// Normal launches enter the Agent directly. Only this explicit history
	// action requests a resume; preserve a normalized npm shim's JS prefix.
	prefix := append([]string(nil), p.Args...)
	for i, arg := range p.Args {
		if arg == "--model" {
			prefix = append([]string(nil), p.Args[:i]...)
			break
		}
	}
	if target == "codex" {
		p.Args = append(prefix, "resume", id)
	} else {
		p.Args = append(prefix, "--resume", id)
	}
	if a.Model != "" {
		p.Args = append(p.Args, "--model", a.Model)
	}
	if err := os.MkdirAll(p.RuntimeDir, 0o700); err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	LaunchPhase(ctx, "start", "创建工具进程")
	pid, err := s.startWithClaudeAPIPin(ctx, p, a)
	if err != nil {
		return Result{}, err
	}
	if target == "claude" && pid == 0 {
		return Result{Target: target, ProfileDir: p.ProfileDir, Workspace: p.Workspace, Message: "原会话恢复请求已提交，API 配置已保留；进程状态尚未确认，请检查终端后再重试。"}, nil
	}
	return Result{Target: target, PID: pid, ProfileDir: p.ProfileDir, Workspace: p.Workspace, Message: "已在原目录启动会话恢复"}, nil
}
