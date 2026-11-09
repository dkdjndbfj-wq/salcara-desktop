package launcher

import (
	"context"
	"errors"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/toolcfg"
)

type configurationGuardKey struct{}
type nativeConfigurationStopKey struct{}

// WithNativeConfigurationStopForTest injects an owned fake stop boundary.
// Tests must not enumerate or terminate the user's real native tasks.
func WithNativeConfigurationStopForTest(ctx context.Context, stop func(context.Context, Plan) error) context.Context {
	return context.WithValue(ctx, nativeConfigurationStopKey{}, stop)
}

func stopManagedConfigurationCLI(ctx context.Context, p Plan) error {
	if stop, ok := ctx.Value(nativeConfigurationStopKey{}).(func(context.Context, Plan) error); ok {
		return stop(ctx, p)
	}
	return StopManagedCLI(ctx, p)
}

// WithConfigurationGuard marks the safe stop/apply transaction and supplies
// the source/selection CAS to run immediately before each write and launch.
func WithConfigurationGuard(ctx context.Context, check func() error) context.Context {
	return context.WithValue(ctx, configurationGuardKey{}, check)
}

func configurationGuard(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if check, ok := ctx.Value(configurationGuardKey{}).(func() error); ok {
		return check()
	}
	return nil
}

func safeConfigurationChange(ctx context.Context) bool {
	return ctx.Value(configurationGuardKey{}) != nil
}

// StopConfigurationFamily runs before catalog refresh can persist selection
// hints. Configure repeats this check immediately before native writes.
func (s *Service) StopConfigurationFamily(ctx context.Context, a config.LocalAccount, target, workspace string, paths map[string]string) error {
	if err := lockLaunch(ctx, &s.mu); err != nil {
		return err
	}
	defer s.mu.Unlock()
	p, err := s.PrepareSwitch(ctx, a, target, workspace, paths)
	if err != nil {
		return err
	}
	return s.stopNativeConfigurationFamily(ctx, p, paths)
}

func (s *ClaudeDesktop3PService) StopForConfigurationChange(ctx context.Context, paths map[string]string) error {
	if err := lockLaunch(ctx, &s.Local.mu); err != nil {
		return err
	}
	defer s.Local.mu.Unlock()
	tool, platform, _, _, err := s.inspect(ctx, paths)
	if err != nil {
		return err
	}
	return s.Local.stopNativeConfigurationFamily(ctx, s.plan(tool, platform.Root), paths)
}

// ValidateConfigurationChange is a read-only preflight. Invalid platform,
// native profile, protocol or mode consent must never stop an existing task.
func (s *ClaudeDesktop3PService) ValidateConfigurationChange(ctx context.Context, a config.LocalAccount, paths map[string]string, allowModeChange bool) error {
	if err := lockLaunch(ctx, &s.Local.mu); err != nil {
		return err
	}
	defer s.Local.mu.Unlock()
	a.Kind, a.Target = "claude", "claude-desktop"
	if err := config.ValidateLocalAccount(&a); err != nil {
		return err
	}
	if ((a.Wire != "" && a.Wire != "auto" && a.Wire != "anthropic") || (a.Protocol != "" && a.Protocol != "anthropic")) && !claudeDesktopLoopbackGateway(a) {
		return errors.New("Claude Desktop 需要 Anthropic Messages 接口")
	}
	_, _, state, _, err := s.inspect(ctx, paths)
	if err != nil {
		return err
	}
	id := state.ProfileID
	if id == "" {
		id = config.NewUUID()
	}
	_, err = toolcfg.PrepareClaude3P(state, toolcfg.Claude3PGateway{Name: a.Name, BaseURL: a.BaseURL, Key: a.Key, AuthMode: a.AuthMode, Models: a.Models, CatalogOverride: a.CatalogOverride}, id, allowModeChange)
	return err
}

func (s *Service) stopNativeConfigurationFamily(ctx context.Context, p Plan, paths map[string]string) error {
	if err := configurationGuard(ctx); err != nil {
		return err
	}
	if err := stopManagedConfigurationCLI(ctx, p); err != nil {
		return err
	}
	desktop := p
	if p.Tool.ID == "codex" {
		// Codex CLI and Desktop share config.toml/auth.json. A CLI click must
		// also wait for the original GUI to finish flushing those files.
		desktop.Tool = Tool{}
		for _, tool := range s.Tools(ctx, paths) {
			if tool.ID == "codex-desktop" && tool.Available {
				desktop.Tool = tool
				break
			}
		}
		if desktop.Tool.ID == "" {
			return configurationGuard(ctx)
		}
		desktop.AppData = originalAppData(desktop.Tool)
	}
	if isDesktop(desktop.Tool.ID) {
		if s.Stop == nil {
			return errors.New("无法确认原桌面工具安全退出；配置未更改")
		}
		if err := s.Stop(ctx, desktop); err != nil {
			return err
		}
	}
	return configurationGuard(ctx)
}
