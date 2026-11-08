package console

import (
	"encoding/json"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
)

func TestClientImageMemberConfigurationRoundTrip(t *testing.T) {
	s, h := newTestServer(t)
	seedCodexAPIRuntime(t, s)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.LocalAccounts = append(c.LocalAccounts, config.LocalAccount{ID: "image-member", Name: "Image member", Kind: "api", BaseURL: "https://provider.example", Key: "synthetic-image-secret", Model: "gpt-image-2"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := localRequest(s, h, "POST", "/api/local/codex-api/clients", map[string]any{"id": runtimeRevokeClientID, "name": "Image pool", "accountId": runtimeRevokeAccountID, "imageAccountIds": []string{runtimeRevokeAccountID, "image-member"}})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	client, _ := s.d.Store.Get().APIClient(runtimeRevokeClientID)
	if len(client.ImageAccountIDs) != 2 {
		t.Fatal("image members not saved")
	}
	w = localRequest(s, h, "GET", "/api/local/codex-api", nil)
	if strings.Contains(w.Body.String(), "synthetic-image-secret") || strings.Contains(w.Body.String(), runtimeRevokeClientKey) || strings.Contains(w.Body.String(), client.KeyDigest) {
		t.Fatal("status exposed a credential")
	}
	var status struct {
		Clients []struct {
			ImageAccountIDs []string `json:"imageAccountIds"`
		}
	}
	if json.Unmarshal(w.Body.Bytes(), &status) != nil || len(status.Clients) != 1 || len(status.Clients[0].ImageAccountIDs) != 2 {
		t.Fatal("image member DTO missing")
	}
	for _, members := range [][]string{{"removed"}, {runtimeRevokeAccountID, runtimeRevokeAccountID}} {
		w = localRequest(s, h, "POST", "/api/local/codex-api/clients", map[string]any{"id": runtimeRevokeClientID, "name": "Bad pool", "accountId": runtimeRevokeAccountID, "imageAccountIds": members})
		if w.Code != 400 {
			t.Fatalf("invalid members accepted: %d %s", w.Code, w.Body)
		}
		current, _ := s.d.Store.Get().APIClient(runtimeRevokeClientID)
		if len(current.ImageAccountIDs) != 2 || current.Name != "Image pool" {
			t.Fatal("invalid edit mutated client")
		}
	}
}
