package agents

import (
	"context"
	"errors"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func TestClaudeAPIRequiredBeforeStartOrSpawn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings Settings
	}{
		{"missing", Settings{}},
		{"key only", Settings{ClaudeKey: "synthetic"}},
		{"address only", Settings{ClaudeRoot: "https://api.example.invalid"}},
		{"blank key", Settings{ClaudeRoot: "https://api.example.invalid", ClaudeKey: "  "}},
		{"invalid address", Settings{ClaudeRoot: "file:///synthetic", ClaudeKey: "synthetic"}},
		{"pairing without api", SettingsWithGateway(config.Config{RemoteDeviceOnly: true}, 47831)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Neither inherited credentials nor a native settings file are a fallback.
			t.Setenv("ANTHROPIC_API_KEY", "synthetic-inherited-not-selected")
			t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "synthetic-native-not-selected")
			a := newClaudeAgent(func(protocol.Event) { t.Error("unadmitted task emitted an event") }, func() Settings { return tc.settings }, "must-not-launch", t.TempDir())
			t.Cleanup(a.Close)
			for _, desktop := range []bool{false, true} {
				id, err := a.StartWithOptions(context.Background(), t.TempDir(), "unsent", "ask", TurnOptions{Desktop: desktop})
				if id != "" || !errors.Is(err, errClaudeAPIRequired) || protocol.ExecutionErrorCode(err) != "claude_api_required" {
					t.Fatal("new Claude task did not require an explicit API")
				}
			}
			p, err := a.spawn(context.Background(), "fixture", t.TempDir(), "", "ask", true)
			if p != nil || !errors.Is(err, errClaudeAPIRequired) {
				t.Fatal("resumed worker could fall back to a native login")
			}
			if len(a.live) != 0 || len(a.procs) != 0 || len(a.workers) != 0 || len(a.desktop) != 0 || len(a.fromPhone) != 0 {
				t.Fatal("missing API changed worker/session ownership")
			}
		})
	}
}

func TestClaudeAPIRequiredBeforeReusingOrRetiringExistingWorker(t *testing.T) {
	for _, continuing := range []bool{false, true} {
		t.Run(map[bool]string{false: "send", true: "continue"}[continuing], func(t *testing.T) {
			a, p, rec := newClaudeResultFixture(t)
			writes := 0
			p.stdin = &claudeDeliveryFixtureWriter{write: func(b []byte) (int, error) { writes++; return len(b), nil }}
			p.policy = "ask"
			info := protocol.SessionInfo{SessionKey: "claude:" + p.id, Tool: "claude", Status: "idle", LastTerminalTurnID: "terminal-fixture"}
			if !continuing {
				p.turns, p.turnIDs = 1, []string{"accepted-turn"}
				info.Status, info.ActiveTurnID = "running", "accepted-turn"
			}
			a.live[p.id] = info
			// Removing the selection cannot stop work already accepted under the old API.
			a.settings = func() Settings { return Settings{} }
			var err error
			if continuing {
				_, err = a.ContinueWithOptions(context.Background(), p.id, "unsent", "terminal-fixture", "10000000-0000-4000-8000-000000000090", TurnOptions{Model: "different"})
			} else {
				err = a.SendWithOptions(context.Background(), p.id, "unsent", TurnOptions{Model: "different"})
			}
			if !errors.Is(err, errClaudeAPIRequired) || writes != 0 || p.stopped || a.procs[p.id] != p || len(rec.all()) != 0 {
				t.Fatal("API removal admitted a turn, retired a worker or changed active work")
			}
			if a.continuedTerminal[p.id] != "" || a.live[p.id] != info {
				t.Fatal("API refusal left a continuation claim or altered session state")
			}
		})
	}
}

func TestClaudeAPIRequiredDoesNotChangeCodexNativeSettings(t *testing.T) {
	s := SettingsWithGateway(config.Config{RemoteDeviceOnly: true}, 47831)
	if validateClaudeAPI(s) == nil || s.codexProvider() != "" {
		t.Fatal("Claude API policy broadened into Codex native execution")
	}
	for _, s := range []Settings{
		{ClaudeRoot: "https://api.example.invalid", ClaudeKey: "synthetic"},
		{RelayRoot: "http://127.0.0.1:1", ClaudeKey: "synthetic", ClaudeAuthMode: "api-key"},
	} {
		if err := validateClaudeAPI(s); err != nil {
			t.Fatal("explicit API rejected")
		}
	}
}

func TestClaudeAPIRevocationBeforeSpawnLeavesNoDesktopOwnership(t *testing.T) {
	reads := 0
	a := newClaudeAgent(func(protocol.Event) { t.Error("failed startup published a task") }, func() Settings {
		reads++
		if reads == 1 {
			return Settings{ClaudeRoot: "http://127.0.0.1:1", ClaudeKey: "synthetic"}
		}
		return Settings{}
	}, "must-not-launch", t.TempDir())
	t.Cleanup(a.Close)
	id, err := a.StartWithOptions(context.Background(), t.TempDir(), "unsent", "ask", TurnOptions{Desktop: true})
	if id != "" || !errors.Is(err, errClaudeAPIRequired) || reads != 2 {
		t.Fatal("startup did not recheck its API before launching")
	}
	if len(a.live) != 0 || len(a.policies) != 0 || len(a.desktop) != 0 || len(a.fromPhone) != 0 || len(a.workers) != 0 {
		t.Fatal("failed API startup retained a phantom Desktop import or session")
	}
}
