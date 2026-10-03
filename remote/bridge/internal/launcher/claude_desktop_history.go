package launcher

import (
	"context"
	"errors"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/toolcfg"
)

// ReadOnlyHistory never starts/stops Claude, changes deployment mode, runs a
// model, or opens cloud account APIs. Resolve the current namespace on every
// command, under the same mutex used by API configuration/restarts.
func (s *ClaudeDesktop3PService) ReadOnlyHistory(ctx context.Context, paths map[string]string) (*agents.ClaudeDesktopHistory, error) {
	if s == nil || s.Local == nil || s.Platform == nil {
		return nil, errors.New("Claude Desktop 本地历史服务不可用")
	}
	s.Local.mu.Lock()
	defer s.Local.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var tool Tool
	for _, candidate := range s.Local.Tools(ctx, paths) {
		if candidate.ID == "claude-desktop" {
			tool = candidate
			break
		}
	}
	if !tool.Available || !fileExists(tool.Path) {
		return nil, errors.New("未检测到 Claude Desktop")
	}
	platform, err := s.Platform(ctx, tool)
	if err != nil {
		return nil, errors.New("Claude Desktop 本地历史环境无法验证")
	}
	if compatible, _ := toolcfg.Claude3PVersionSupport(platform.Version); !compatible || platform.Managed {
		return nil, errors.New("Claude Desktop 当前版本或组织策略未授权本地历史读取")
	}
	root, err := toolcfg.ResolveClaudeDesktopHistoryRoot(platform.Root)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return agents.NewClaudeDesktopHistory(root, platform.Version)
}
