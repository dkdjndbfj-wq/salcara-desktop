package console

import (
	"net/http"
	"reflect"
	"testing"

	"salcara/bridge/internal/config"
)

func TestAPIMemberCardCannotReplaceImplicitPrimary(t *testing.T) {
	tests := []struct {
		name, mode string
		enabled    bool
		configure  func(*config.NamedAPIClient, string, string)
	}{
		{"last image member", "image", false, func(c *config.NamedAPIClient, _, target string) { c.ImageAccountIDs = []string{target} }},
		{"last text member", "text", false, func(c *config.NamedAPIClient, _, target string) { c.AccountIDs, c.PoolStrategy = []string{target}, "" }},
		{"empty image add", "image", true, func(c *config.NamedAPIClient, _, _ string) { c.ImageAccountIDs = nil }},
		{"empty text add", "text", true, func(c *config.NamedAPIClient, _, _ string) { c.AccountIDs, c.PoolStrategy = nil, "" }},
		{"both rejects last image atomically", "both", false, func(c *config.NamedAPIClient, _, target string) { c.ImageAccountIDs = []string{target} }},
		{"both rejects empty text atomically", "both", true, func(c *config.NamedAPIClient, primary, _ string) {
			c.AccountIDs, c.PoolStrategy, c.ImageAccountIDs = nil, "", []string{primary}
		}},
		{"fixed text account", "text", false, func(c *config.NamedAPIClient, _, target string) {
			c.PoolStrategy, c.PoolAccountID = "single_account", target
		}},
	}
	for _, revised := range []bool{false, true} {
		contract := "legacy/"
		if revised {
			contract = "revision/"
		}
		for _, tc := range tests {
			t.Run(contract+tc.name, func(t *testing.T) {
				s, h, primary, target, id := memberRevisionFixture(t)
				if err := s.d.Store.Update(func(c *config.Config) error {
					for i := range c.CodexAPI.Clients {
						if c.CodexAPI.Clients[i].ID == id {
							tc.configure(&c.CodexAPI.Clients[i], primary, target)
							return config.ValidateAPIClient(c.CodexAPI.Clients[i])
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				body := map[string]any{"clientId": id, "accountId": target}
				if revised {
					body = memberRevisionRequest(t, s, h, target, id)
				}
				body["mode"], body["enabled"] = tc.mode, tc.enabled
				before := s.d.Store.Get()
				w := localRequest(s, h, http.MethodPost, "/api/local/codex-api/members", body)
				if w.Code != http.StatusConflict {
					t.Fatalf("card edit requires explicit client settings, got %d: %s", w.Code, w.Body)
				}
				if !reflect.DeepEqual(before, s.d.Store.Get()) {
					t.Fatal("rejected member edit changed effective account grants")
				}
			})
		}
	}
}

func TestAPIMemberCardNonLastChangesKeepOtherExecutionSources(t *testing.T) {
	for _, mode := range []string{"text", "image", "both"} {
		t.Run(mode, func(t *testing.T) {
			s, h, primary, target, id := memberRevisionFixture(t)
			if err := s.d.Store.Update(func(c *config.Config) error {
				for i := range c.CodexAPI.Clients {
					c.CodexAPI.Clients[i].ImageAccountIDs = []string{primary, target}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, enabled := range []bool{false, true} {
				before, _ := s.d.Store.Get().APIClient(id)
				body := memberRevisionRequest(t, s, h, target, id)
				body["mode"], body["enabled"] = mode, enabled
				w := localRequest(s, h, http.MethodPost, "/api/local/codex-api/members", body)
				if w.Code != http.StatusOK {
					t.Fatalf("ordinary member edit: %d %s", w.Code, w.Body)
				}
				after, _ := s.d.Store.Get().APIClient(id)
				expected := []string{primary}
				if enabled {
					expected = append(expected, target)
				}
				if mode != "image" {
					if !reflect.DeepEqual(after.AccountIDs, expected) {
						t.Fatal("wrong text grant")
					}
					before.AccountIDs = after.AccountIDs
				}
				if mode != "text" {
					if !reflect.DeepEqual(after.ImageAccountIDs, expected) {
						t.Fatal("wrong image grant")
					}
					before.ImageAccountIDs = after.ImageAccountIDs
				}
				before.AuthEpoch = after.AuthEpoch
				if !reflect.DeepEqual(before, after) {
					t.Fatal("card edit changed an unrelated client setting or execution source")
				}
			}
		})
	}
}
