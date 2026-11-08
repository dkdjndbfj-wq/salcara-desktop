package console

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
)

func memberRevisionFixture(t *testing.T) (*Server, http.Handler, string, string, string) {
	t.Helper()
	s, h, primary := sourceRevisionFixture(t)
	target := createLocal(t, s, h, "api", "member fixture", "synthetic-member-secret", "https://member.example.test")
	w := localRequest(s, h, http.MethodPost, "/api/local/codex-api/clients", map[string]any{
		"name": "member test", "accountId": primary, "accountIds": []string{primary, target}, "imageAccountIds": []string{primary},
		"poolStrategy": "auto", "modelAllowlist": []string{"gpt-fixture"}, "modelAliases": map[string]string{"fixture": "gpt-fixture"}, "maxConcurrency": 3,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("create client: %d", w.Code)
	}
	var out struct{ ID string }
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.ID == "" {
		t.Fatal("missing client")
	}
	return s, h, primary, target, out.ID
}

func memberRevisionRequest(t *testing.T, s *Server, h http.Handler, id, clientID string) map[string]any {
	t.Helper()
	source, _ := listedSourceRevision(t, s, h, id)
	w := localRequest(s, h, http.MethodGet, "/api/local/codex-api", nil)
	if w.Code != http.StatusOK {
		t.Fatal("missing client status")
	}
	var out struct {
		Clients []struct{ ID, Revision string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	var revision string
	for _, row := range out.Clients {
		if row.ID == clientID {
			revision = row.Revision
		}
	}
	if !regexp.MustCompile(`^api-client-v1-[a-f0-9]{64}$`).MatchString(revision) {
		t.Fatal("missing opaque revision")
	}
	c := s.d.Store.Get()
	client, _ := c.APIClient(clientID)
	for _, secret := range []string{c.GatewayRuntimeKey, client.AuthEpoch, client.KeyDigest, c.UsageSourceRevision} {
		if secret != "" && strings.Contains(w.Body.String(), secret) {
			t.Fatal("client status exposed private material")
		}
	}
	return map[string]any{"clientId": clientID, "accountId": id, "mode": "text", "enabled": false, "expectedSourceRevision": source, "expectedClientRevision": revision}
}

func TestAPIMemberRevisionFreshCASOnlyChangesRequestedMember(t *testing.T) {
	s, h, primary, target, id := memberRevisionFixture(t)
	body := memberRevisionRequest(t, s, h, target, id)
	before, _ := s.d.Store.Get().APIClient(id)
	w := localRequest(s, h, http.MethodPost, "/api/local/codex-api/members", body)
	if w.Code != http.StatusOK {
		t.Fatalf("membership: %d %s", w.Code, w.Body)
	}
	after, _ := s.d.Store.Get().APIClient(id)
	if !reflect.DeepEqual(after.AccountIDs, []string{primary}) {
		t.Fatal("wrong text membership")
	}
	if before.AuthEpoch == after.AuthEpoch {
		t.Fatal("committed mutation did not retire epoch")
	}
	before.AccountIDs, before.AuthEpoch = after.AccountIDs, after.AuthEpoch
	if !reflect.DeepEqual(before, after) {
		t.Fatal("membership modified unrelated routing, images, model ACL, digest or concurrency")
	}
	if retry := localRequest(s, h, http.MethodPost, "/api/local/codex-api/members", body); retry.Code != http.StatusConflict {
		t.Fatal("consumed snapshot was accepted again")
	}
}

func TestAPIMemberRevisionRejectsSourceClientAndProcessChanges(t *testing.T) {
	tests := map[string]func(*config.Config, string, string){
		"source key": func(c *config.Config, target, _ string) {
			for i := range c.LocalAccounts {
				if c.LocalAccounts[i].ID == target {
					c.LocalAccounts[i].Key = "synthetic-new-key"
				}
			}
		},
		"source URL": func(c *config.Config, target, _ string) {
			for i := range c.LocalAccounts {
				if c.LocalAccounts[i].ID == target {
					c.LocalAccounts[i].BaseURL = "https://replacement.example.test"
				}
			}
		},
		"source identity": func(c *config.Config, target, _ string) {
			for i := range c.LocalAccounts {
				if c.LocalAccounts[i].ID == target {
					c.LocalAccounts[i].CredentialEpoch++
				}
			}
		},
		"source deleted": func(c *config.Config, target, _ string) {
			for i := range c.LocalAccounts {
				if c.LocalAccounts[i].ID == target {
					c.LocalAccounts = append(c.LocalAccounts[:i], c.LocalAccounts[i+1:]...)
					break
				}
			}
		},
		"client key": func(c *config.Config, _, id string) {
			for i := range c.CodexAPI.Clients {
				if c.CodexAPI.Clients[i].ID == id {
					c.CodexAPI.Clients[i].KeyDigest = config.APIClientDigest("synthetic-rotated")
				}
			}
		},
		"client disabled": func(c *config.Config, _, id string) {
			for i := range c.CodexAPI.Clients {
				if c.CodexAPI.Clients[i].ID == id {
					c.CodexAPI.Clients[i].Disabled = true
				}
			}
		},
		"client model": func(c *config.Config, _, id string) {
			for i := range c.CodexAPI.Clients {
				if c.CodexAPI.Clients[i].ID == id {
					c.CodexAPI.Clients[i].ModelAllowlist = []string{"different-model"}
				}
			}
		},
		"client pool": func(c *config.Config, _, id string) {
			for i := range c.CodexAPI.Clients {
				if c.CodexAPI.Clients[i].ID == id {
					c.CodexAPI.Clients[i].PoolStrategy = "random"
				}
			}
		},
		"client deleted": func(c *config.Config, _, _ string) { c.CodexAPI.Clients = nil },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			s, h, _, target, id := memberRevisionFixture(t)
			if name == "source identity" {
				updateSourceFixture(t, s, target, func(a *config.LocalAccount) {
					a.SourceType, a.ProviderID = "official_account", "openai"
					a.CredentialRef, a.CredentialEpoch = "synthetic-member-protected-ref", 1
				})
			}
			body := memberRevisionRequest(t, s, h, target, id)
			if err := s.d.Store.Update(func(c *config.Config) error { mutate(c, target, id); return nil }); err != nil {
				t.Fatal(err)
			}
			before := s.d.Store.Get()
			w := localRequest(s, h, http.MethodPost, "/api/local/codex-api/members", body)
			if w.Code != http.StatusConflict {
				t.Fatalf("stale %s accepted: %d", name, w.Code)
			}
			if !reflect.DeepEqual(before, s.d.Store.Get()) {
				t.Fatal("rejected write changed state")
			}
		})
	}
	t.Run("process", func(t *testing.T) {
		s, h, _, target, id := memberRevisionFixture(t)
		body := memberRevisionRequest(t, s, h, target, id)
		s.d.Store.SetRuntimeGatewayKey("synthetic-new-process")
		if w := localRequest(s, h, http.MethodPost, "/api/local/codex-api/members", body); w.Code != http.StatusConflict {
			t.Fatal("old process token accepted")
		}
	})
}

func TestAPIMemberRevisionPartialAndEmptyPreconditionsNeverUseLegacy(t *testing.T) {
	for _, which := range []string{"missing source", "missing client", "empty source", "empty client"} {
		t.Run(which, func(t *testing.T) {
			s, h, _, target, id := memberRevisionFixture(t)
			body := memberRevisionRequest(t, s, h, target, id)
			switch which {
			case "missing source":
				delete(body, "expectedSourceRevision")
			case "missing client":
				delete(body, "expectedClientRevision")
			case "empty source":
				body["expectedSourceRevision"] = ""
			case "empty client":
				body["expectedClientRevision"] = ""
			}
			if w := localRequest(s, h, http.MethodPost, "/api/local/codex-api/members", body); w.Code != http.StatusConflict {
				t.Fatalf("invalid precondition: %d", w.Code)
			}
		})
	}
	s, h, _, target, id := memberRevisionFixture(t)
	if w := localRequest(s, h, http.MethodPost, "/api/local/codex-api/members", map[string]any{"clientId": id, "accountId": target, "mode": "text", "enabled": false}); w.Code != http.StatusOK {
		t.Fatal("legacy caller changed")
	}
}

func TestAPIMemberRevisionABAAndFinalTransactionGuard(t *testing.T) {
	s, h, primary, target, id := memberRevisionFixture(t)
	// The target is not yet a member of either channel. Its source changing
	// away and back must retire the preview even though client.AuthEpoch is
	// not coupled to credentials that have not been added to that client.
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.CodexAPI.Clients[0].AccountIDs = []string{primary}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	body := memberRevisionRequest(t, s, h, target, id)
	old := s.d.Store.Get()
	original, _ := old.LocalAccount(target)
	updateSourceFixture(t, s, target, func(a *config.LocalAccount) { a.Key = "synthetic-intermediate" })
	updateSourceFixture(t, s, target, func(a *config.LocalAccount) { a.Key = original.Key })
	body["mode"], body["enabled"] = "image", true
	if w := localRequest(s, h, http.MethodPost, "/api/local/codex-api/members", body); w.Code != http.StatusConflict {
		t.Fatal("ABA key restore accepted stale preview")
	}

	fresh := s.d.Store.Get()
	account, _ := fresh.LocalAccount(target)
	client, _ := fresh.APIClient(id)
	source, revision := fresh.AccountSourceRevision(account), apiClientRevision(fresh, client)
	if checkAPIMemberRevisions(fresh, account, client, &source, &revision) != nil {
		t.Fatal("fresh guard failed")
	}
	// Simulate a change between admission and the update callback using the
	// exact shared guard called again inside Store.Update, not only at ingress.
	updateSourceFixture(t, s, target, func(a *config.LocalAccount) { a.Key = "synthetic-final-race" })
	before := s.d.Store.Get()
	err := s.d.Store.Update(func(c *config.Config) error {
		currentAccount, _ := c.LocalAccount(target)
		currentClient, _ := c.APIClient(id)
		if err := checkAPIMemberRevisions(*c, currentAccount, currentClient, &source, &revision); err != nil {
			return err
		}
		c.CodexAPI.Clients[0].AccountIDs = []string{primary}
		return nil
	})
	if !errors.Is(err, errCodexAPIMemberChanged) || !reflect.DeepEqual(before, s.d.Store.Get()) {
		t.Fatal("final transaction allowed stale confirmation")
	}
}
