package hubclient

import (
	"reflect"
	"testing"

	"salcara/bridge/internal/config"
)

func TestBuiltinClaudeAPIFamilyMatchesSharedWorkerWithoutGroupingCodexDesktop(t *testing.T) {
	accounts := []config.LocalAccount{
		{ID: "claude-applied", Name: "Claude applied", Kind: "api", BaseURL: "https://fixture.example", Key: "synthetic-claude-key"},
		{ID: "claude-phone", Name: "Claude phone", Kind: "api", BaseURL: "https://fixture.example", Key: "synthetic-phone-key"},
		{ID: "codex-phone", Name: "Codex phone", Kind: "api", BaseURL: "https://fixture.example", Key: "synthetic-codex-key"},
		{ID: "desktop-applied", Name: "Desktop applied", Kind: "api", BaseURL: "https://fixture.example", Key: "synthetic-desktop-key"},
	}
	cfg := config.Config{ComputerID: "fixture-pc", LocalAccounts: accounts,
		ToolAPISelections: map[string]string{"claude": accounts[0].ID, "codex-desktop": accounts[3].ID},
		RemoteAPI:         map[string]config.RemoteAPI{"codex": {AccountID: accounts[2].ID}}}
	for _, target := range []string{"claude", "codex-desktop"} {
		account, _ := cfg.SelectedToolAPI(target)
		cfg.RecordAppliedAPI(target, account, "fixture-provider")
	}
	c := statusFixture(t, cfg)
	var originalCodex map[string]agentAPIStatus
	for _, source := range []string{"computer", "phone"} {
		t.Run(source, func(t *testing.T) {
			if source == "phone" {
				if err := c.o.Store.Update(func(current *config.Config) error {
					return current.SetRemoteAPI("claude", accounts[1].ID, "")
				}); err != nil {
					t.Fatal(err)
				}
			}
			list := dispatchStatus(t, c)
			profiles := map[string]agentStatus{}
			families := map[string][]string{}
			for _, profile := range list {
				profiles[profile.ID] = profile
				if profile.APIFamily != "" {
					families[profile.APIFamily] = append(families[profile.APIFamily], profile.ID)
				}
			}
			if !reflect.DeepEqual(families, map[string][]string{"claude": {"claude-desktop", "claude"}}) {
				t.Fatal("built-in shared API metadata grouped an unrelated execution surface")
			}
			code, desktop := profiles["claude"], profiles["claude-desktop"]
			if code.API != desktop.API || code.API.Source != source || !code.API.Configured || code.API.AccountID == "" {
				t.Fatal("shared family metadata does not match the effective Claude worker API")
			}
			if desktop.Execution == nil || desktop.Execution.Tool != "claude" || desktop.Execution.Entrypoint != "claude-desktop" || desktop.ControlSurface != "desktop" {
				t.Fatal("API sharing changed the Desktop Code execution contract")
			}
			codex := map[string]agentAPIStatus{"codex": profiles["codex"].API, "codex-desktop": profiles["codex-desktop"].API}
			if codex["codex"].Name != accounts[2].Name || codex["codex"].Source != "phone" || codex["codex-desktop"].Name != accounts[3].Name || codex["codex-desktop"].Source != "computer" {
				t.Fatal("Codex worker override changed the Desktop applied API")
			}
			if originalCodex == nil {
				originalCodex = codex
			} else if !reflect.DeepEqual(originalCodex, codex) {
				t.Fatal("changing the shared Claude worker API changed Codex")
			}
		})
	}
}
