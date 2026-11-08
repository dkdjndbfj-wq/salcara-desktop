package console

import (
	"encoding/json"
	"net/http"
	"testing"

	"salcara/bridge/internal/config"
)

func TestLocalPoolPolicyRoundTripAndLegacyPreservation(t *testing.T) {
	s, h := newTestServer(t)
	id := createLocal(t, s, h, "api", "Pool", "synthetic-pool-key", "https://proxy.test/v1")
	body := map[string]any{"id": id, "enabled": true, "group": "coding", "strategy": "custom", "priority": 7, "weight": 3, "backup": true, "maxConcurrency": 4, "quotaReservePercent": 0}
	w := localRequest(s, h, http.MethodPost, "/api/local/accounts/pool", body)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	a, _ := s.d.Store.Get().LocalAccount(id)
	if a.PoolStrategy != "custom" || a.Priority != 7 || a.Weight != 3 || !a.PoolBackup || a.MaxConcurrency != 4 {
		t.Fatalf("pool policy not saved: %+v", a)
	}
	var response struct {
		Account map[string]any `json:"account"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Account["poolStrategy"] != "custom" || response.Account["poolBackup"] != true {
		t.Fatalf("masked policy missing: %#v", response.Account)
	}

	// An older caller that knows only enabled/group/reserve must not erase the
	// custom scheduler policy already stored on the account.
	w = localRequest(s, h, http.MethodPost, "/api/local/accounts/pool", map[string]any{"id": id, "enabled": true, "group": "coding", "quotaReservePercent": 0})
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	a, _ = s.d.Store.Get().LocalAccount(id)
	if a.PoolStrategy != "custom" || a.Priority != 7 || a.Weight != 3 || !a.PoolBackup || a.MaxConcurrency != 4 {
		t.Fatalf("legacy pool update erased newer policy: %+v", a)
	}
	if err := config.ValidateLocalAccount(&a); err != nil {
		t.Fatal(err)
	}
}
