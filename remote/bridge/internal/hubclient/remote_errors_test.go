package hubclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/config"
)

func TestRemoteResponseErrorClassificationAndSafeMessages(t *testing.T) {
	for _, tc := range []struct {
		name, path, auth, body, code, state string
		status                              int
		device, claim                       bool
	}{
		{name: "legacy", status: 401, auth: "Bearer synthetic", code: "remote_authorization_denied", state: StateInvalidKey},
		{name: "device legacy rejection", status: 403, device: true, code: "device_credentials_invalid", state: StateDeviceAuthRequired},
		{name: "claim legacy rejection", path: "/device/register", status: 403, device: true, claim: true, code: "enrollment_claim_invalid", state: StateDeviceAuthRequired},
		{name: "enrollment", status: 403, body: `{"code":"device_enrollment_required","error":"secret-fixture"}`, code: "device_enrollment_required", state: StateDeviceAuthRequired},
		{name: "credentials required", status: 401, body: `{"code":"device_credentials_required"}`, code: "device_credentials_required", state: StateDeviceAuthRequired},
		{name: "source", status: 403, body: `{"code":"source_not_authorized"}`, code: "source_not_authorized", state: StateSourceUnauthorized},
		{name: "changed source", status: 403, body: `{"code":"source_policy_changed"}`, code: "source_policy_changed", state: StateSourceUnauthorized},
		{name: "pair", status: 403, body: `{"code":"pair_credentials_invalid"}`, code: "pair_credentials_invalid", state: StatePairRequired},
		{name: "disabled", status: 423, code: "user_disabled", state: StateRemoteBlocked},
		{name: "mixed credentials", status: 400, body: `{"code":"ambiguous_credentials"}`, code: "ambiguous_credentials", state: StateRemoteBlocked},
		{name: "invalid request", status: 400, body: `{"code":"invalid_request"}`, code: "invalid_request", state: StateRemoteBlocked},
		{name: "old unavailable", status: 503, code: "remote_unavailable", state: StateConnecting},
		{name: "unavailable", status: 503, body: `{"code":"remote_unavailable"}`, code: "remote_unavailable", state: StateConnecting},
		{name: "unsupported", status: 404, code: "remote_unsupported", state: StateConnecting},
		{name: "capacity", status: 429, code: "remote_rate_limited", state: StateConnecting},
		{name: "unknown code", status: 500, body: `{"code":"secret-fixture","error":"secret-fixture"}`, code: "remote_request_failed", state: StateConnecting},
		{name: "malformed body", status: 500, body: `{"code":"device_credentials_invalid"}secret-fixture`, code: "remote_request_failed", state: StateConnecting},
		{name: "oversized body", status: 500, body: `{"code":"device_credentials_invalid","error":"` + strings.Repeat("x", 4096) + `secret-fixture"}`, code: "remote_request_failed", state: StateConnecting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "https://example.test"+tc.path, nil)
			req.Header.Set("Authorization", tc.auth)
			if tc.device {
				req.Header.Set("X-Salcara-Device-Id", "synthetic-device")
			}
			if tc.claim {
				req.Header.Set("X-Salcara-Enrollment-Claim", "synthetic-claim")
			}
			err := checkResp(&http.Response{StatusCode: tc.status, Request: req, Body: io.NopCloser(strings.NewReader(tc.body))})
			if RemoteErrorCode(err) != tc.code || errors.Is(err, errUnauthorized) != (tc.status == 401 || tc.status == 403) {
				t.Fatalf("incorrect error classification: %#v", err)
			}
			state, code, message := remoteFailure(err, config.Config{RemoteDeviceOnly: tc.device})
			if state != tc.state || code != tc.code || strings.Contains(message, "secret-fixture") || strings.Contains(message, "返回 0") {
				t.Fatalf("incorrect status: %s %s %s", state, code, message)
			}
		})
	}
}

func TestHubIdentityErrorsAreSeparateFromModelAPIKeys(t *testing.T) {
	state, code, message := remoteFailure(ErrHubIdentityChanged, config.Config{})
	if state != StateDeviceAuthRequired || code != "hub_identity_changed" || !strings.Contains(message, "本地 API、项目和对话已保留") {
		t.Fatal("changed Hub identity did not request explicit registration", state, code, message)
	}
	state, code, message = remoteFailure(ErrHubIdentityVerification, config.Config{})
	if state != StateConnecting || code != "remote_unavailable" || !strings.Contains(message, "未发送电脑凭证") {
		t.Fatal("temporary identity failure was treated as a revoked API", state, code, message)
	}
}

func TestSourceHTTPFailuresNeverInvalidateDeviceOrPhoneCredentials(t *testing.T) {
	for _, group := range []struct {
		codes  []string
		status int
		state  string
	}{
		{[]string{"source_agent_denied", "source_agent_mismatch", "source_binding_mismatch", "source_challenge_expired", "source_challenge_invalid", "source_challenge_mismatch", "source_command_invalid", "source_command_mismatch", "source_command_unsupported", "source_control_surface_mismatch", "source_credential_mismatch", "source_grant_expired", "source_grant_invalid", "source_grant_required", "source_guard_unsupported", "source_issuer_denied", "source_metadata_required", "source_model_mismatch", "source_policy_changed", "source_policy_stale", "source_scope_denied", "source_station_denied", "source_acquisition_unsupported"}, 403, StateSourceUnauthorized},
		{[]string{"source_policy_unavailable", "source_challenge_unavailable"}, 503, StateConnecting},
		{[]string{"source_challenge_capacity"}, 429, StateConnecting},
		{[]string{"source_acquisition_pending"}, 409, StateConnecting},
	} {
		for _, code := range group.codes {
			t.Run(code, func(t *testing.T) {
				req := httptest.NewRequest("POST", "https://example.test/v1/bridge/source/grant", nil)
				req.Header.Set("X-Salcara-Device-Id", "synthetic")
				err := checkResp(&http.Response{StatusCode: group.status, Request: req, Body: io.NopCloser(strings.NewReader(`{"code":"` + code + `","error":"secret-fixture"}`))})
				state, gotCode, message := remoteFailure(err, config.Config{RemoteDeviceOnly: true})
				if state != group.state || gotCode != code || strings.Contains(message, "secret-fixture") || strings.Contains(message, "重新绑定") || strings.Contains(message, "登记已失效") {
					t.Fatal("source verification was confused with a revoked device or phone", state, gotCode, message)
				}
			})
		}
	}
}

func TestRemoteRegistrationAndStreamFailuresPreserveOrdinaryAPI(t *testing.T) {
	for _, tc := range []struct{ code, state, stage string }{
		{"device_credentials_invalid", StateDeviceAuthRequired, "register"},
		{"enrollment_claim_invalid", StateDeviceAuthRequired, "register"},
		{"source_not_authorized", StateSourceUnauthorized, "stream"},
		{"device_credentials_invalid", StateDeviceAuthRequired, "stream"},
		{"pair_credentials_invalid", StatePairRequired, "stream"},
		{"user_disabled", StateRemoteBlocked, "register"},
		{"remote_unavailable", StateConnecting, "stream"},
	} {
		t.Run(tc.stage+"/"+tc.code, func(t *testing.T) {
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer synthetic-model-key" {
					t.Error("ordinary API key changed")
					w.WriteHeader(401)
					return
				}
				w.Write([]byte(`{"ok":true}`))
			}))
			defer model.Close()
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				stage := ""
				switch r.URL.Path {
				case "/salcara-hub/v1/device/register":
					stage = "register"
				case "/salcara-hub/v1/bridge/stream":
					stage = "stream"
				default:
					w.WriteHeader(404)
					return
				}
				if r.Header.Get("Authorization") != "" || r.Header.Get("X-Salcara-Device-Id") == "" {
					t.Error("device-only remote authentication mixed with the model API")
				}
				if stage == tc.stage {
					status := 403
					if tc.code == "user_disabled" {
						status = 423
					}
					if tc.code == "remote_unavailable" {
						status = 503
					}
					w.WriteHeader(status)
					w.Write([]byte(`{"code":"` + tc.code + `","error":"secret-fixture"}`))
					return
				}
				w.Write([]byte(`{}`))
			}))
			defer hub.Close()
			c, store := newTestClient(t, hub.URL, "synthetic-legacy-key", nil)
			if err := store.Update(func(cfg *config.Config) error {
				cfg.RemoteDeviceOnly = true
				cfg.LocalAccounts = []config.LocalAccount{{ID: "ordinary", Key: "synthetic-model-key", BaseURL: model.URL, Model: "fixture"}}
				cfg.ActiveCodexAccount = "ordinary"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := store.Get()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); c.Run(ctx) }()
			defer func() { cancel(); <-done }()
			waitFor(t, "classified remote failure", func() bool { s := c.Status(); return s.State == tc.state && s.ErrorCode == tc.code })
			cfg := store.Get()
			if cfg.AccountKey != before.AccountKey || cfg.ActiveCodexAccount != before.ActiveCodexAccount || !reflect.DeepEqual(cfg.LocalAccounts, before.LocalAccounts) {
				t.Fatal("remote rejection changed ordinary API selection or credentials")
			}
			req, _ := http.NewRequest("POST", cfg.LocalAccounts[0].BaseURL+"/responses", strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+cfg.LocalAccounts[0].Key)
			resp, err := model.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 || strings.Contains(c.Status().Error, "secret-fixture") {
				t.Fatal("ordinary API became unavailable or private server text leaked")
			}
		})
	}
}

func TestLateRemoteFailureCannotRepaintRetiredConnection(t *testing.T) {
	for _, change := range []string{"same-site-kick", "station-switch", "A-B-A"} {
		t.Run(change, func(t *testing.T) {
			c, store := newTestClient(t, "https://one.test", "synthetic", nil)
			cfg := store.Get()
			epoch := c.connectionEpoch
			if change != "same-site-kick" {
				if err := store.Update(func(next *config.Config) error { next.HubURL = "https://two.test/salcara-hub"; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			c.Kick()
			if change == "A-B-A" {
				if err := store.Update(func(next *config.Config) error { next.HubURL = cfg.HubURL; return nil }); err != nil {
					t.Fatal(err)
				}
				c.Kick()
			}
			c.setState(StateConnected, "")
			if _, accepted := c.publishConnectionFailure(cfg, epoch, &RemoteHTTPError{Code: "device_credentials_invalid", StatusCode: 403}); accepted || c.Status().State != StateConnected || c.Status().ErrorCode != "" {
				t.Fatal("retired HTTP failure overwrote the current connection")
			}
		})
	}
}

func TestDelayedUnauthorizedResponseAfterKickReconnectsWithoutAuthError(t *testing.T) {
	c, _ := newTestClient(t, "https://one.test", "synthetic", nil)
	entered, release := make(chan struct{}), make(chan struct{})
	var attempts, authErrors atomic.Int32
	c.o.HTTP.Transport = sourceGrantRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		status, body := 200, `{}`
		if strings.HasSuffix(r.URL.Path, "/register") && attempts.Add(1) == 1 {
			close(entered)
			<-release
			status, body = 401, `{"code":"device_credentials_invalid"}`
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	c.streamHTTP.Transport = sourceGrantRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	c.o.OnStatus = func(s Status) {
		if s.State == StateDeviceAuthRequired || s.State == StateInvalidKey {
			authErrors.Add(1)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("registration did not start")
	}
	c.Kick()
	close(release)
	waitFor(t, "new registration", func() bool { return attempts.Load() >= 2 })
	if authErrors.Load() != 0 {
		t.Fatal("old unauthorized response marked the new connection invalid")
	}
}
