package console

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
)

func TestLocalProfilesCannotFollowAccountSwitchOrCrossRoutes(t *testing.T) {
	s, _ := newTestServer(t)
	s.d.Store.SetRuntimeGatewayKey("synthetic-current-process")
	apply := func(id string) {
		t.Helper()
		if err := s.d.Store.Update(func(c *config.Config) error {
			if len(c.LocalAccounts) == 0 {
				for _, id := range []string{"fixture-A", "fixture-B"} {
					c.LocalAccounts = append(c.LocalAccounts, config.LocalAccount{ID: id, Name: id, Kind: "api", SourceType: "reverse_proxy", BaseURL: "https://synthetic.invalid", Key: "synthetic-private-" + id, Model: "gpt-fixture"})
				}
				c.RecordAppliedAPI("claude", c.APIForTool(c.LocalAccounts[0], "claude"), "fixture")
			}
			a, _ := c.LocalAccount(id)
			c.RecordAppliedAPI("codex", c.APIForTool(a, "codex"), "fixture")
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	apply("fixture-A")
	h := s.Handler()
	request := func(target, key string) int {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/gateway/"+target+"/v1/models", nil)
		r.Host, r.RemoteAddr = "127.0.0.1:47831", "127.0.0.1:43210"
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	old := s.d.Store.Get().LocalGatewayCredential("codex")
	if old == "" || old == s.d.Store.Get().GatewayCredential() {
		t.Fatal("unscoped process key published to profile")
	}
	if request("codex", old) != 200 || request("claude", old) != 401 || request("codex", s.d.Store.Get().GatewayCredential()) != 401 {
		t.Fatal("route-scoped admission failed")
	}
	apply("fixture-B")
	if request("codex", old) != 401 {
		t.Fatal("old profile followed source B")
	}
	apply("fixture-A")
	if request("codex", old) != 401 {
		t.Fatal("switching back revived old profile")
	}
	current := s.d.Store.Get().LocalGatewayCredential("codex")
	if request("codex", current) != 200 {
		t.Fatal("fresh application rejected")
	}
	for _, denied := range [][]string{{"gpt-fixture"}, nil} {
		if err := s.d.Store.Update(func(c *config.Config) error { c.LocalAccounts[0].ModelDenylist = denied; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if request("codex", current) != 401 || s.d.Store.Get().LocalGatewayCredential("codex") != "" {
		t.Fatal("restoring revoked account policy reactivated old profile without apply")
	}
	apply("fixture-A")
	if request("codex", current) != 401 {
		t.Fatal("reapplying identical account revived previous launch")
	}
	bytes, err := os.ReadFile(s.d.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bytes), "LaunchLease") || strings.Contains(string(bytes), "launchLease") || strings.Contains(string(bytes), old) || strings.Contains(string(bytes), s.d.Store.Get().ToolAPIApplied["codex"].LaunchLease) {
		t.Fatal("process authorization persisted")
	}
	reopened, err := config.Open(s.d.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	reopened.SetRuntimeGatewayKey("synthetic-restarted-process")
	if _, ok := reopened.Get().AuthorizedAppliedToolAccount("codex"); ok || reopened.Get().LocalGatewayCredential("codex") != "" {
		t.Fatal("restart adopted stored profile without new application")
	}
}
