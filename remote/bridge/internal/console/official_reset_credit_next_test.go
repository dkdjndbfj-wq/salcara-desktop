package console

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"salcara/bridge/internal/codexaccounts"
)

type officialResetEnvelope struct {
	OK      bool                             `json:"ok"`
	Request codexaccounts.ResetCreditRequest `json:"request"`
	Code    string                           `json:"code"`
}

func TestOfficialResetCreditPrepareNextRequiresFreshObservationAndCAS(t *testing.T) {
	var calls atomic.Int32
	fetcher := codexaccounts.UsageFetchFunc(func(context.Context, *coreauth.Auth) (codexaccounts.UsageFetchResult, error) {
		credits := int64(2)
		return codexaccounts.UsageFetchResult{ProviderAccountID: "fixture-provider-id", Snapshot: codexaccounts.UsageSnapshot{ResetCreditsAvailable: &credits}}, nil
	})
	consumer := codexaccounts.ResetCreditConsumeFunc(func(context.Context, *coreauth.Auth, string) error { calls.Add(1); return nil })
	s, h := officialConsoleWithUsageAndReset(t, fetcher, consumer)
	id := importOfficialConsole(t, s, h)
	request := func(route string, body map[string]string, wantStatus int) officialResetEnvelope {
		t.Helper()
		w := localRequest(s, h, "POST", "/api/local/codex/reset-credit/"+route, body)
		var envelope officialResetEnvelope
		if w.Code != wantStatus || json.Unmarshal(w.Body.Bytes(), &envelope) != nil {
			t.Fatalf("%s: %d %s", route, w.Code, w.Body)
		}
		assertNoOfficialSecrets(t, w.Body.String())
		return envelope
	}
	first := request("prepare", map[string]string{"id": id}, http.StatusOK)
	if !first.OK || first.Request.State != "prepared" || calls.Load() != 0 {
		t.Fatalf("prepare consumed: %+v calls=%d", first, calls.Load())
	}
	first = request("consume", map[string]string{"id": id, "redeemRequestId": first.Request.RedeemRequestID}, http.StatusOK)
	if !first.OK || first.Request.State != "succeeded" || calls.Load() != 1 {
		t.Fatalf("first consume: %+v calls=%d", first, calls.Load())
	}
	legacy := request("prepare", map[string]string{"id": id}, http.StatusOK)
	if legacy.Request.RedeemRequestID != first.Request.RedeemRequestID || legacy.Request.State != "succeeded" {
		t.Fatalf("legacy prepare changed semantics: %+v", legacy)
	}
	nextBody := map[string]string{"id": id, "previousRedeemRequestId": first.Request.RedeemRequestID}
	blocked := request("prepare", nextBody, http.StatusBadRequest)
	if blocked.OK || blocked.Code != "usage_refresh_required" || calls.Load() != 1 {
		t.Fatalf("missing usage accepted: %+v", blocked)
	}
	usage := localRequest(s, h, "POST", "/api/local/codex/usage/refresh", map[string]string{"id": id})
	if usage.Code != http.StatusOK || !strings.Contains(usage.Body.String(), `"verified":true`) {
		t.Fatalf("usage: %d %s", usage.Code, usage.Body)
	}
	second := request("prepare", nextBody, http.StatusOK)
	if !second.OK || second.Request.State != "prepared" || second.Request.RedeemRequestID == first.Request.RedeemRequestID || calls.Load() != 1 {
		t.Fatalf("next prepare: %+v calls=%d", second, calls.Load())
	}
	retry := request("prepare", nextBody, http.StatusOK)
	if retry.Request.RedeemRequestID != second.Request.RedeemRequestID {
		t.Fatalf("CAS retry duplicated next: %+v", retry)
	}
	old := request("consume", map[string]string{"id": id, "redeemRequestId": first.Request.RedeemRequestID}, http.StatusOK)
	if old.OK || old.Code != "account_changed" || calls.Load() != 1 {
		t.Fatalf("old handle consumed next: %+v calls=%d", old, calls.Load())
	}
	second = request("consume", map[string]string{"id": id, "redeemRequestId": second.Request.RedeemRequestID}, http.StatusOK)
	if !second.OK || second.Request.State != "succeeded" || calls.Load() != 2 {
		t.Fatalf("second consume: %+v calls=%d", second, calls.Load())
	}
	retry = request("prepare", nextBody, http.StatusOK)
	if retry.Request.RedeemRequestID != second.Request.RedeemRequestID || retry.Request.State != "succeeded" {
		t.Fatalf("completed successor skipped CAS: %+v", retry)
	}
}

func TestOfficialResetCreditNextDoesNotUnlockUnknownResult(t *testing.T) {
	var calls atomic.Int32
	fetcher := codexaccounts.UsageFetchFunc(func(context.Context, *coreauth.Auth) (codexaccounts.UsageFetchResult, error) {
		credits := int64(2)
		return codexaccounts.UsageFetchResult{ProviderAccountID: "fixture-provider-id", Snapshot: codexaccounts.UsageSnapshot{ResetCreditsAvailable: &credits}}, nil
	})
	s, h := officialConsoleWithUsageAndReset(t, fetcher, codexaccounts.ResetCreditConsumeFunc(func(context.Context, *coreauth.Auth, string) error { calls.Add(1); return context.DeadlineExceeded }))
	id := importOfficialConsole(t, s, h)
	prepared := localRequest(s, h, "POST", "/api/local/codex/reset-credit/prepare", map[string]string{"id": id})
	var first officialResetEnvelope
	if json.Unmarshal(prepared.Body.Bytes(), &first) != nil || !first.OK {
		t.Fatalf("prepare: %s", prepared.Body)
	}
	consumed := localRequest(s, h, "POST", "/api/local/codex/reset-credit/consume", map[string]string{"id": id, "redeemRequestId": first.Request.RedeemRequestID})
	if !strings.Contains(consumed.Body.String(), `"state":"unknown"`) {
		t.Fatalf("unknown: %s", consumed.Body)
	}
	localRequest(s, h, "POST", "/api/local/codex/usage/refresh", map[string]string{"id": id})
	blocked := localRequest(s, h, "POST", "/api/local/codex/reset-credit/prepare", map[string]string{"id": id, "previousRedeemRequestId": first.Request.RedeemRequestID})
	if blocked.Code != http.StatusBadRequest || !strings.Contains(blocked.Body.String(), `"code":"result_unknown"`) || calls.Load() != 1 {
		t.Fatalf("unknown next: %d %s calls=%d", blocked.Code, blocked.Body, calls.Load())
	}
	localRequest(s, h, "POST", "/api/local/codex/reset-credit/consume", map[string]string{"id": id, "redeemRequestId": first.Request.RedeemRequestID})
	if calls.Load() != 1 {
		t.Fatalf("unknown replay: %d", calls.Load())
	}
}
