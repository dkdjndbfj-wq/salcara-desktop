package gateway

import (
	"strings"

	"salcara/bridge/internal/config"
)

func validGatewayTarget(target string) bool {
	if family, direct := strings.CutPrefix(target, "direct-"); direct {
		return config.ValidRemoteFamily(family) || config.IsAdditionalTool(family)
	}
	switch target {
	case "codex", "claude", "claude-desktop", "remote-codex", "remote-claude",
		"deepseek-harness", "hermes", "remote-deepseek-harness", "remote-hermes":
		return true
	}
	return false
}
