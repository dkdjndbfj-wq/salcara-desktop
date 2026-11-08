package codexaccounts

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func resetTestService(t *testing.T, consumer ResetCreditConsumer, refresher Refresher) (*Service, string) {
	t.Helper()
	s, err := New(Options{Path: filepath.Join(t.TempDir(), "vault.json"), Protector: &testProtector{}, ResetCreditConsumer: consumer, Refresher: refresher})
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseImport([]byte(`{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","account_id":"synthetic-provider","email":"fixture@example.test"}`))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.Import(context.Background(), p, "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("import: %v %+v", err, rows)
	}
	return s, rows[0].ID
}

func TestOfficialResetCreditConsumerSyntheticHTTPFixture(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if req.URL.String() != officialResetCreditConsumeURL || req.Method != http.MethodPost {
			return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: http.NoBody, Request: req}, nil
		}
		if req.Header.Get("Authorization") != "Bearer synthetic-access" || req.Header.Get("ChatGPT-Account-Id") != "synthetic-provider" {
			return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: http.NoBody, Request: req}, nil
		}
		var body map[string]string
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || !validRedeemRequestID(body["redeem_request_id"]) {
			return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: http.NoBody, Request: req}, nil
		}
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: http.NoBody, Request: req}, nil
	})
	consumer := NewResetCreditHTTPConsumer(transport, time.Second)
	s, id := resetTestService(t, consumer, nil)
	defer s.Close()
	request, err := s.PrepareResetCredit(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ConsumeResetCredit(context.Background(), id, request.RedeemRequestID)
	if err != nil || got.State != resetStateSucceeded || calls.Load() != 1 {
		t.Fatalf("consume: %+v %v calls=%d", got, err, calls.Load())
	}
	got, err = s.ConsumeResetCredit(context.Background(), id, request.RedeemRequestID)
	if err != nil || got.State != resetStateSucceeded || calls.Load() != 1 {
		t.Fatalf("idempotency: %+v %v calls=%d", got, err, calls.Load())
	}
}

func TestResetCreditConcurrentCallsAreCoalesced(t *testing.T) {
	var calls atomic.Int32
	consumer := ResetCreditConsumeFunc(func(ctx context.Context, _ *coreauth.Auth, _ string) error {
		calls.Add(1)
		select {
		case <-time.After(30 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	s, id := resetTestService(t, consumer, nil)
	defer s.Close()
	request, err := s.PrepareResetCredit(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make([]ResetCreditRequest, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = s.ConsumeResetCredit(context.Background(), id, request.RedeemRequestID)
		}(i)
	}
	wg.Wait()
	if calls.Load() != 1 || errs[0] != nil || errs[1] != nil || results[0].State != resetStateSucceeded || results[1].State != resetStateSucceeded {
		t.Fatalf("coalesce calls=%d results=%+v errors=%v", calls.Load(), results, errs)
	}
}

func TestResetCreditUnauthorizedRefreshesOnceAndReusesID(t *testing.T) {
	var calls, refreshes atomic.Int32
	consumer := ResetCreditConsumeFunc(func(_ context.Context, a *coreauth.Auth, redeemID string) error {
		calls.Add(1)
		if redeemID == "" {
			t.Fatal("missing redeem id")
		}
		if calls.Load() == 1 {
			return resetError("needs_login", ErrNeedsLogin)
		}
		if a.Metadata["access_token"] != "rotated-access" {
			t.Fatalf("refresh token not used")
		}
		return nil
	})
	refresher := RefreshFunc(func(_ context.Context, _ *coreauth.Auth) (*coreauth.Auth, error) {
		refreshes.Add(1)
		return &coreauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "rotated-access", "account_id": "synthetic-provider"}}, nil
	})
	s, id := resetTestService(t, consumer, refresher)
	defer s.Close()
	request, err := s.PrepareResetCredit(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ConsumeResetCredit(context.Background(), id, request.RedeemRequestID)
	if err != nil || got.State != resetStateSucceeded || calls.Load() != 2 || refreshes.Load() != 1 {
		t.Fatalf("401 retry: %+v %v calls=%d refreshes=%d", got, err, calls.Load(), refreshes.Load())
	}
}

func TestResetCreditRejectsAPIKeyMetadata(t *testing.T) {
	consumer := NewResetCreditHTTPConsumer(roundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("must not request"); return nil, nil }), time.Second)
	if err := consumer.ConsumeResetCredit(context.Background(), &coreauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "sk-test", "api_key": "sk-test"}}, "00000000-0000-4000-8000-000000000000"); ResetCreditErrorCode(err) != "api_key_unsupported" && ResetCreditErrorCode(err) != "invalid_credential" {
		t.Fatalf("api key error: %v code=%s", err, ResetCreditErrorCode(err))
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
