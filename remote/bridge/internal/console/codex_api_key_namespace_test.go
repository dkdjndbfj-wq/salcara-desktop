package console

import (
	"encoding/json"
	"net/http"
	"testing"

	"salcara/bridge/internal/config"
)

func TestCodexAPIKeyCheckUsesExecutionNamespacePolicy(t *testing.T) {
	s, h := newTestServer(t)
	seedCodexAPIRuntime(t, s)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.CodexAPI.Clients[0].ModelPrefix = "team/"
		c.CodexAPI.Clients[0].ModelAllowlist = []string{"gpt-fixture"}
		c.CodexAPI.Clients[0].ModelAliases = map[string]string{"picture": "gpt-fixture"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		model   string
		allowed bool
	}{
		{"team/picture", true}, {"team/gpt-fixture", true},
		{"picture", false}, {"gpt-fixture", false},
		{"team/team/picture", false}, {"other/picture", false},
		{"team/denied", false},
	} {
		t.Run(tt.model, func(t *testing.T) {
			w := localRequest(s, h, http.MethodPost, "/api/local/codex-api/key-check", map[string]string{"key": runtimeRevokeClientKey, "model": tt.model})
			var result struct {
				Authenticated bool  `json:"authenticated"`
				Allowed       *bool `json:"modelPolicyAllowed"`
				Verified      bool  `json:"upstreamVerified"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || !result.Authenticated || result.Allowed == nil || *result.Allowed != tt.allowed || result.Verified {
				t.Fatalf("model policy differs from execution: %d %s", w.Code, w.Body)
			}
		})
	}
}
