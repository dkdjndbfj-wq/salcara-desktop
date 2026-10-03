package launcher

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"

	"salcara/bridge/internal/config"
)

var sessionID = regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)

// Resume opens the exact original session. It never creates/forks a thread or
// rewrites API config, and never silently falls back to a new session.
func (s *Service) Resume(ctx context.Context, a config.LocalAccount, target, id, cwd string, paths map[string]string) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if (target != "codex" && target != "claude") || !sessionID.MatchString(id) {
		return Result{}, errors.New("会话编号或恢复工具无效")
	}
	p, err := s.PrepareSwitch(ctx, a, target, cwd, paths)
	if err != nil {
		return Result{}, err
	}
	// Preserve a normalized npm shim's JS prefix, but replace only resume args.
	matched := false
	for i, arg := range p.Args {
		if (target == "codex" && arg == "resume") || (target == "claude" && arg == "--resume") {
			prefix := append([]string{}, p.Args[:i]...)
			if target == "codex" {
				p.Args = append(prefix, "resume", id, "--model", a.Model)
			} else {
				p.Args = append(prefix, "--resume", id, "--model", a.Model)
			}
			matched = true
			break
		}
	}
	if !matched || strings.Join(p.Args, " ") == "" {
		return Result{}, errors.New("无法解析原工具恢复入口")
	}
	if err := os.MkdirAll(p.RuntimeDir, 0o700); err != nil {
		return Result{}, err
	}
	pid, err := s.Start(ctx, p)
	if err != nil {
		return Result{}, err
	}
	return Result{Target: target, PID: pid, ProfileDir: p.ProfileDir, Workspace: p.Workspace, Message: "已请求在原目录恢复这一个会话，没有新建或迁移会话。若工具提示找不到记录，请检查原会话文件，不要改用新建。"}, nil
}
