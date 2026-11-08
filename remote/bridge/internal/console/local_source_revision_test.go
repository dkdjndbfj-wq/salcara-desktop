package console

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/launcher"
)

func sourceRevisionFixture(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	s, h := newTestServer(t)
	s.d.Local.FindTools = func(context.Context, map[string]string) []launcher.Tool { return []launcher.Tool{} }
	s.d.Store.SetRuntimeGatewayKey("synthetic-runtime-source-revision-a")
	id := createLocal(t, s, h, "api", "source fixture", "same-head-aaaaaaaa-private-middle-same-tail", "https://source.example.test")
	return s, h, id
}

func listedSourceRevision(t *testing.T, s *Server, h http.Handler, id string) (string, string) {
	t.Helper()
	w := localRequest(s, h, http.MethodGet, "/api/local/accounts", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("account list returned %d", w.Code)
	}
	var out struct {
		Accounts []struct {
			ID             string `json:"id"`
			SourceRevision string `json:"sourceRevision"`
			KeyMasked      string `json:"keyMasked"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	c := s.d.Store.Get()
	a, _ := c.LocalAccount(id)
	for _, secret := range []string{a.Key, c.GatewayRuntimeKey, config.AccountFingerprint(a)} {
		if secret != "" && strings.Contains(w.Body.String(), secret) {
			t.Fatal("account list exposed a credential or raw fingerprint")
		}
	}
	for _, row := range out.Accounts {
		if row.ID == id {
			return row.SourceRevision, row.KeyMasked
		}
	}
	t.Fatal("synthetic account missing")
	return "", ""
}

func updateSourceFixture(t *testing.T, s *Server, id string, mutate func(*config.LocalAccount)) {
	t.Helper()
	if err := s.d.Store.Update(func(c *config.Config) error {
		for i := range c.LocalAccounts {
			if c.LocalAccounts[i].ID == id {
				mutate(&c.LocalAccounts[i])
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLocalSourceRevisionDetectsSameMaskKeyAndProcessReplacement(t *testing.T) {
	s, h, id := sourceRevisionFixture(t)
	before, maskBefore := listedSourceRevision(t, s, h, id)
	if !regexp.MustCompile(`^src-v1-[0-9a-f]{64}$`).MatchString(before) {
		t.Fatal("missing safe source revision")
	}
	if repeat, _ := listedSourceRevision(t, s, h, id); repeat != before {
		t.Fatal("unchanged source revision is not stable")
	}
	updateSourceFixture(t, s, id, func(a *config.LocalAccount) { a.Key = "same-head-bbbbbbbb-private-middle-same-tail" })
	after, maskAfter := listedSourceRevision(t, s, h, id)
	if maskAfter != maskBefore || after == before {
		t.Fatal("same masked key replacement escaped source detection")
	}
	s.d.Store.SetRuntimeGatewayKey("synthetic-runtime-source-revision-b")
	if restarted, _ := listedSourceRevision(t, s, h, id); restarted == after || restarted == "" {
		t.Fatal("revision survived a process identity replacement")
	}
	s.d.Store.SetRuntimeGatewayKey("")
	if unarmed, _ := listedSourceRevision(t, s, h, id); unarmed != "" {
		t.Fatal("unarmed runtime exposed a stable credential fingerprint")
	}
}

func TestLocalSourceRevisionTracksExecutionSourceNotPresentationFields(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		change  func(*config.LocalAccount)
		changed bool
	}{
		{"endpoint", func(a *config.LocalAccount) { a.BaseURL = "https://other.example.test" }, true},
		{"auth", func(a *config.LocalAccount) { a.AuthMode = "api-key" }, true},
		{"wire", func(a *config.LocalAccount) { a.Wire = "chat" }, true},
		{"model", func(a *config.LocalAccount) { a.Model = "other-model" }, true},
		{"allowlist", func(a *config.LocalAccount) { a.ModelAllowlist = []string{"coding-model"} }, true},
		{"provider", func(a *config.LocalAccount) { a.ProviderID = "other-provider" }, true},
		{"name", func(a *config.LocalAccount) { a.Name = "renamed fixture" }, false},
		{"last-used", func(a *config.LocalAccount) { a.LastUsedAt++ }, false},
		{"catalog-hint", func(a *config.LocalAccount) { a.Models = []string{"coding-model", "new-hint"} }, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			s, h, id := sourceRevisionFixture(t)
			before, _ := listedSourceRevision(t, s, h, id)
			updateSourceFixture(t, s, id, scenario.change)
			after, _ := listedSourceRevision(t, s, h, id)
			if (before != after) != scenario.changed {
				t.Fatal("unexpected source revision classification")
			}
		})
	}
}

func TestLocalSourceRevisionTracksOfficialCredentialIdentity(t *testing.T) {
	s, h, id := sourceRevisionFixture(t)
	updateSourceFixture(t, s, id, func(a *config.LocalAccount) {
		a.SourceType, a.ProviderID = "official_account", "openai"
		a.CredentialRef, a.CredentialEpoch = "synthetic-official-ref", 1
	})
	before, _ := listedSourceRevision(t, s, h, id)
	updateSourceFixture(t, s, id, func(a *config.LocalAccount) { a.CredentialEpoch = 2 })
	after, _ := listedSourceRevision(t, s, h, id)
	if after == before {
		t.Fatal("official identity replacement retained source revision")
	}
	updateSourceFixture(t, s, id, func(a *config.LocalAccount) { a.CredentialRef = "synthetic-other-official-ref" })
	if replaced, _ := listedSourceRevision(t, s, h, id); replaced == after {
		t.Fatal("official credential reference replacement retained source revision")
	}
}
