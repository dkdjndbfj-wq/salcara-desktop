package launcher

import (
	"context"
	"encoding/json"
	"path/filepath"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/toolcfg"
)

// --settings has higher precedence than user/project settings.env. Process
// environment alone is insufficient because Claude reapplies settings at start
// and on reload. A pure-env file avoids copying hooks or editing project files.
// Managed organization policy still takes precedence over this connection.
func (s *Service) startWithClaudeAPIPin(ctx context.Context, p Plan, a config.LocalAccount) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if p.RuntimeDir == "" {
		p.RuntimeDir = filepath.Join(s.Dir, "launch-scratch", p.Tool.ID)
	}
	if p.Tool.ID == "claude" {
		env := toolcfg.ClaudeAPIEnvironment(a.BaseURL, a.Key, a.AuthMode)
		if a.Model != "" {
			env["ANTHROPIC_MODEL"] = a.Model
		}
		contents, err := json.Marshal(map[string]any{"env": env})
		if err != nil {
			return 0, err
		}
		p.ClaudeSettingsPin, err = agents.WriteClaudeRuntimeSettings(string(contents))
		if err != nil {
			return 0, err
		}
		p.Args = append(p.Args, "--settings", p.ClaudeSettingsPin)
	}
	pid, err := s.Start(ctx, p)
	if err != nil {
		agents.RemoveClaudeRuntimeSettings(p.ClaudeSettingsPin)
	}
	return pid, err
}
