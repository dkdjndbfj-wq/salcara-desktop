package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/localusage"
)

func TestUsageOverviewReadsEverySavedAPIWithoutLeakingKeys(t *testing.T) {
	// Keep this computer's own Codex / Claude Code records out of the fixture.
	localUsageOnce.Do(func() {})
	localUsage = &localusage.Scanner{ClaudeHome: t.TempDir(), CodexHome: t.TempDir()}
	var hits atomic.Int32
	relaySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/v1/usage" || r.URL.Query().Get("days") != "7" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") == "Bearer sk-official" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"remaining":12.5,"unit":"USD","daily_usage":[{"date":"2026-10-01","total_tokens":1000,"actual_cost":0.2}]}`))
	}))
	defer relaySrv.Close()

	s, h := newTestServer(t)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.LocalAccounts = []config.LocalAccount{
			{ID: "a", Name: "主力", Kind: "api", BaseURL: relaySrv.URL + "/v1", Key: "sk-secret-a"},
			{ID: "dup", Name: "重复", Kind: "api", BaseURL: relaySrv.URL, Key: "sk-secret-a"},
			{ID: "b", Name: "官方", Kind: "api", BaseURL: relaySrv.URL, Key: "sk-official"},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	host := "127.0.0.1:47831"
	ck := do(h, "GET", "/", host, nil, "").Result().Cookies()[0]
	w := do(h, "GET", "/api/usage/overview?days=7", host, map[string]string{"Cookie": ck.Name + "=" + ck.Value}, "")
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "sk-secret-a") || strings.Contains(w.Body.String(), "sk-official") {
		t.Fatal("API keys must never be returned")
	}
	var out struct {
		Days    int           `json:"days"`
		Sources []usageSource `json:"sources"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Days != 7 || len(out.Sources) != 2 {
		t.Fatalf("want 2 deduplicated sources for 7 days, got %+v", out)
	}
	if !out.Sources[0].OK || out.Sources[0].Usage["remaining"] != 12.5 {
		t.Fatalf("relay source not read: %+v", out.Sources[0])
	}
	if out.Sources[1].OK || out.Sources[1].Error == "" {
		t.Fatalf("provider without usage endpoint should report an error: %+v", out.Sources[1])
	}
	// A second read within the cache window does not hit the providers again.
	before := hits.Load()
	do(h, "GET", "/api/usage/overview?days=7", host, map[string]string{"Cookie": ck.Name + "=" + ck.Value}, "")
	if hits.Load() != before {
		t.Fatal("expected cached result")
	}
}
