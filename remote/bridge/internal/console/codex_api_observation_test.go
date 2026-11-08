package console

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/usageledger"
)

func TestAPIClientEnabledNarrowWriteAndStaleRevision(t *testing.T) {
	s, h := newTestServer(t)
	seedCodexAPIRuntime(t, s)
	c := s.d.Store.Get()
	before, _ := c.APIClient(runtimeRevokeClientID)
	request := map[string]any{"id": before.ID, "enabled": false, "expectedRevision": apiClientRevision(c, before)}
	w := localRequest(s, h, "POST", "/api/local/codex-api/clients/enabled", request)
	after, _ := s.d.Store.Get().APIClient(before.ID)
	if w.Code != 200 || !after.Disabled || codexAPIAdmissionStatus(s, runtimeRevokeClientKey) != http.StatusUnauthorized {
		t.Fatal("disable failed", w.Code, w.Body)
	}
	after.Disabled, after.AuthEpoch = before.Disabled, before.AuthEpoch
	if !reflect.DeepEqual(after, before) {
		t.Fatal("toggle overwrote routing settings")
	}
	request["enabled"] = true
	if w = localRequest(s, h, "POST", "/api/local/codex-api/clients/enabled", request); w.Code != 409 {
		t.Fatal("stale enabled request accepted", w.Code)
	}
	c = s.d.Store.Get()
	after, _ = c.APIClient(before.ID)
	request["expectedRevision"] = apiClientRevision(c, after)
	if w = localRequest(s, h, "POST", "/api/local/codex-api/clients/enabled", request); w.Code != 200 || codexAPIAdmissionStatus(s, runtimeRevokeClientKey) != 200 {
		t.Fatal("fresh reenable failed", w.Code)
	}
}

func TestAPIClientEnabledSaveFailureKeepsOriginalPolicy(t *testing.T) {
	s, h := newTestServer(t)
	seedCodexAPIRuntime(t, s)
	c := s.d.Store.Get()
	row, _ := c.APIClient(runtimeRevokeClientID)
	blockCodexAPIConfigSaves(t, s)
	w := localRequest(s, h, "POST", "/api/local/codex-api/clients/enabled", map[string]any{"id": row.ID, "enabled": false, "expectedRevision": apiClientRevision(c, row)})
	after, _ := s.d.Store.Get().APIClient(row.ID)
	if w.Code != 500 || !reflect.DeepEqual(row, after) {
		t.Fatal("failed toggle changed state", w.Code)
	}
}

func TestAPIObservationRoutesRequireConsoleAndStrictQueries(t *testing.T) {
	s, h := newTestServer(t)
	if w := do(h, "GET", "/api/local/codex-api/requests", "127.0.0.1:47831", nil, ""); w.Code != 401 {
		t.Fatal("request history bypassed console auth", w.Code)
	}
	for _, q := range []string{"?limit=0", "?limit=101", "?limit=1&limit=2", "?clientId=../bad", "?other=1", "?limit=1;clientId=x", "?clientId=%zz"} {
		if w := localRequest(s, h, "GET", "/api/local/codex-api/requests"+q, nil); w.Code != 400 {
			t.Fatal("bad query accepted", q, w.Code)
		}
	}
	for _, b := range []map[string]any{{"id": "fixture", "expectedRevision": "x"}, {"id": "fixture", "enabled": nil, "expectedRevision": "x"}, {"id": "fixture", "enabled": true}, {"id": "fixture", "enabled": true, "expectedRevision": "x", "accountId": "other"}} {
		if w := localRequest(s, h, "POST", "/api/local/codex-api/clients/enabled", b); w.Code != 400 {
			t.Fatal("ambiguous toggle accepted", w.Code)
		}
	}
}

func TestAPIRecentRequestsFilterActualAccountsAndPreserveUnknown(t *testing.T) {
	s, h := newTestServer(t)
	seedCodexAPIRuntime(t, s)
	if err := s.d.Store.Update(func(c *config.Config) error { c.LocalAccounts[0].Name = "实际账号"; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, client := range []string{runtimeRevokeClientID, "other-client", ""} {
		s.observeUsage(usageledger.Observation{StartedAt: time.Now(), AccountID: runtimeRevokeAccountID, ClientID: client, Route: "codex-api/responses", Model: "gpt-fixture", State: "interrupted", HTTPStatus: 200, UpstreamHTTPStatus: 200, FailureStage: "stream", Attempts: []usageledger.Attempt{{AccountID: runtimeRevokeAccountID, State: "interrupted", HTTPStatus: 200, Reason: "stream_interrupted"}}})
	}
	flushConsoleLedger(t, s)
	w := localRequest(s, h, "GET", "/api/local/codex-api/requests?clientId="+runtimeRevokeClientID, nil)
	var out struct {
		Records      []usageledger.Record `json:"records"`
		AccountNames map[string]string    `json:"accountNames"`
		Total        int                  `json:"total"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Total != 1 || len(out.Records) != 1 {
		t.Fatal("client filter failed", w.Code, w.Body)
	}
	r := out.Records[0]
	if r.FirstByteMS != nil || r.State != "interrupted" || out.AccountNames[r.AccountID] != "实际账号" || len(r.Attempts) != 1 {
		t.Fatal("evidence/unknown changed", w.Body)
	}
	if strings.Contains(w.Body.String(), runtimeRevokeAccountID) || strings.Contains(w.Body.String(), runtimeRevokeClientKey) {
		t.Fatal("private IDs or credentials leaked")
	}
	w = localRequest(s, h, "GET", "/api/local/codex-api/requests", nil)
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Total != 2 {
		t.Fatal("included non-service requests", w.Body)
	}
}

func TestAPIImageSettingsExposeSavedAndRestrictedRuntime(t *testing.T) {
	s, h := newTestServer(t)
	seedCodexAPIRuntime(t, s)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.CodexAPI.ImagesMode = "enabled"
		c.CodexAPI.MaxConcurrentImageRequests = 4
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	blockCodexAPIConfigSaves(t, s)
	w := localRequest(s, h, "POST", "/api/local/codex-api/images", map[string]any{"mode": "disabled", "maxConcurrentRequests": 1})
	if w.Code != 500 {
		t.Fatal(w.Code)
	}
	w = localRequest(s, h, "GET", "/api/local/codex-api/images", nil)
	var out config.CodexAPIImageSettings
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Mode != "enabled" || out.MaxConcurrentRequests != 4 || out.EffectiveMode != "disabled" || out.EffectiveMaxConcurrentRequests != 1 || !out.RuntimeRestricted || !out.Serving {
		t.Fatal("saved/runtime confusion", w.Body)
	}
}
