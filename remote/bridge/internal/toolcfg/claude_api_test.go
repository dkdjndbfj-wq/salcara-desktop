package toolcfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestApplyLocalClaudeRetiresCompetingAPIRoutingAndPreservesUserSettings(t *testing.T) {
	for _, mode := range []string{"api-key", "bearer"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "settings.json")
			original := []byte(`{"permissions":{"allow":["Read"]},"hooks":{"Stop":[]},"env":{"CLAUDE_CODE_USE_BEDROCK":"1","CLAUDE_CODE_USE_VERTEX":"1","ANTHROPIC_CUSTOM_HEADERS":"Authorization: stale","ANTHROPIC_PROFILE":"old","USER_SETTING":"preserved"}}`)
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			credentials := filepath.Join(dir, ".credentials.json")
			if err := os.WriteFile(credentials, []byte("original-login"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := ApplyLocalClaude(path, "http://127.0.0.1:1234/gateway/claude/", "synthetic-key", "selected-model", mode); err != nil {
				t.Fatal(err)
			}
			var actual, before map[string]any
			data, err := os.ReadFile(path)
			if err != nil || json.Unmarshal(data, &actual) != nil || json.Unmarshal(original, &before) != nil {
				t.Fatal("invalid settings")
			}
			for _, key := range []string{"permissions", "hooks"} {
				if !reflect.DeepEqual(actual[key], before[key]) {
					t.Fatalf("changed %s", key)
				}
			}
			env := actual["env"].(map[string]any)
			for key, value := range ClaudeAPIEnvironment("http://127.0.0.1:1234/gateway/claude/", "synthetic-key", mode) {
				if env[key] != value {
					t.Fatalf("API field not pinned: %s", key)
				}
			}
			if env["USER_SETTING"] != "preserved" || env["ANTHROPIC_MODEL"] != "selected-model" || actual["model"] != "selected-model" {
				t.Fatal("unrelated settings or selected model changed")
			}
			data, err = os.ReadFile(credentials)
			if err != nil || string(data) != "original-login" {
				t.Fatal("saved login changed")
			}
			data, err = os.ReadFile(path + BackupSuffix)
			if err != nil || string(data) != string(original) {
				t.Fatal("original backup not exact")
			}
		})
	}
}
