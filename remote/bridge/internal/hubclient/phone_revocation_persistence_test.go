package hubclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/gateway"
	"salcara/bridge/internal/protocol"
)

func phoneRevocationStore(t *testing.T) *config.Store {
	t.Helper()
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	store.SetRuntimeGatewayKey("synthetic-phone-revocation-runtime")
	if err := store.Update(func(cfg *config.Config) error {
		cfg.ConnectRemote("https://hub.fixture.test", "https://hub.fixture.test/salcara-hub")
		cfg.PhoneBindingID, cfg.PhoneHash = strings.Repeat("a", 64), strings.Repeat("b", 64)
		cfg.LocalAccounts = []config.LocalAccount{{ID: "source", Kind: "api", SourceType: "api_key", BaseURL: "https://api.fixture.test", Key: "synthetic-upstream-key", Wire: "responses", Model: "gpt-fixture"}}
		cfg.RemoteAPI = map[string]config.RemoteAPI{"codex": {AccountID: "source", Model: "gpt-fixture"}, "claude": {AccountID: "source", Model: "gpt-fixture"}}
		cfg.RecordAppliedAPI("codex", cfg.APIForTool(cfg.LocalAccounts[0], "codex"), "openai")
		cfg.RemoteStationAuthorization = &config.StationAuthorization{Identity: config.RemoteIdentity(*cfg), Revision: "r1", Station: protocol.StationPolicy{Version: 1, APIBases: []string{"https://api.fixture.test"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

func blockPhoneConfigWrites(t *testing.T, store *config.Store) func() {
	t.Helper()
	path := store.Path()
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".saved", path); err != nil {
			t.Fatal(err)
		}
	}
}

func publishPhoneSnapshot(c *Client, cfg config.Config, paired bool, revision int) {
	data, _ := json.Marshal(map[string]any{"deviceId": cfg.DeviceID, "paired": paired, "revision": revision, "bindingId": cfg.PhoneBindingID, "phoneHash": cfg.PhoneHash})
	c.acceptPairState(cfg, string(data))
}

func TestPhoneRevocationSaveFailureRejectsExistingGatewayWorker(t *testing.T) {
	store := phoneRevocationStore(t)
	c := New(Options{Store: store})
	before := store.Get()
	c.publishPair(PairStatus{Paired: true}, eventIdentity(before))
	key := before.RemoteGatewayCredential("remote-codex")
	if key == "" {
		t.Fatal("fixture did not authorize a remote worker")
	}
	if before.RemoteGatewayCredential("remote-claude") == "" || before.LocalGatewayCredential("codex") == "" {
		t.Fatal("fixture did not authorize both remote routes and the independent local tool")
	}
	h := gateway.New(nil, func(target string) (config.LocalAccount, bool) { return store.Get().GatewaySourceAccount(target) })
	h.KeyForTarget = func(target string) string { return store.Get().RemoteGatewayCredential(target) }
	h.AuthorizationRevision = func(target string) string { return store.Get().GatewayAuthorizationRevision(target) }
	var upstreamCalls atomic.Int32
	h.Client.Transport = sourceGrantRoundTripFunc(func(*http.Request) (*http.Response, error) {
		upstreamCalls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"fixture","object":"response","status":"completed","output":[]}`))}, nil
	})
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/gateway/remote-codex/v1/responses", strings.NewReader(`{"model":"gpt-fixture","input":"synthetic"}`))
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request(); w.Code != http.StatusOK || upstreamCalls.Load() != 1 {
		t.Fatalf("authorized fixture failed: status=%d calls=%d", w.Code, upstreamCalls.Load())
	}
	restore := blockPhoneConfigWrites(t, store)
	defer restore()
	publishPhoneSnapshot(c, before, false, 2)
	if c.canUpload(store.Get()) {
		t.Fatal("revoked phone can still upload")
	}
	for _, target := range []string{"remote-codex", "remote-claude"} {
		if store.Get().RemoteGatewayCredential(target) != "" {
			t.Errorf("revoked phone retained a %s credential", target)
		}
	}
	if w := request(); w.Code != http.StatusUnauthorized || upstreamCalls.Load() != 1 {
		t.Fatalf("revoked worker reached upstream after disk failure: status=%d calls=%d", w.Code, upstreamCalls.Load())
	}
	if got := store.Get().LocalGatewayCredential("codex"); got != before.LocalGatewayCredential("codex") {
		t.Fatal("phone revocation changed the independent local tool")
	}
}

func TestPhoneRevocationSaveFailureCannotReplayOldPair(t *testing.T) {
	store := phoneRevocationStore(t)
	c := New(Options{Store: store})
	before := store.Get()
	c.publishPair(PairStatus{Paired: true}, eventIdentity(before))
	restore := blockPhoneConfigWrites(t, store)
	defer restore()
	publishPhoneSnapshot(c, before, false, 2)
	// A stale station can report its old pair after the stream reconnects.
	c.Kick()
	publishPhoneSnapshot(c, before, true, 3)
	if c.canUpload(store.Get()) || phoneMatches(store.Get(), before.PhoneBindingID, before.PhoneHash) {
		t.Fatal("old station snapshot reopened revoked phone after disk failure")
	}
	if len(store.Get().PendingPhoneRevokes) == 0 {
		t.Fatal("revocation was not retained for persistence and station cleanup")
	}
}

func TestPhoneRevocationPendingSaveRecoversWithoutRevivingBinding(t *testing.T) {
	for _, scenario := range []string{"phone-revoke", "local-revoke", "unclaimed-pair", "unrelated-update"} {
		t.Run(scenario, func(t *testing.T) {
			store := phoneRevocationStore(t)
			if scenario == "unclaimed-pair" {
				if err := store.Update(func(cfg *config.Config) error { cfg.PhoneHash = ""; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			client := &http.Client{Transport: sourceGrantRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("offline fixture"))}, nil
			})}
			c := New(Options{Store: store, HTTP: client})
			before := store.Get()
			c.publishPair(PairStatus{Paired: true}, eventIdentity(before))
			restore := blockPhoneConfigWrites(t, store)
			if scenario == "phone-revoke" || scenario == "unrelated-update" {
				publishPhoneSnapshot(c, before, false, 2)
			} else if err := c.RevokePair(context.Background()); err == nil || !strings.Contains(err.Error(), "尚未保存") {
				t.Fatalf("local revoke did not report pending persistence: %v", err)
			}
			if !store.PhoneRevocationPending() || c.canUpload(store.Get()) || store.Get().RemoteGatewayCredential("remote-codex") != "" {
				t.Fatal("save failure lost the pending revoke or left execution authorized")
			}
			retired := store.Get()
			if err := store.Update(func(cfg *config.Config) error { cfg.DeviceName = "not committed"; return nil }); err == nil {
				t.Fatal("fixture did not block an ordinary write")
			}
			if store.Get().PhoneBindingID != retired.PhoneBindingID || store.Get().PhoneHash != "" || !store.PhoneRevocationPending() {
				t.Fatal("failed ordinary write restored the revoked binding")
			}
			if scenario == "unclaimed-pair" && len(retired.PendingPhoneRevokes) != 0 {
				t.Fatal("unclaimed pair created unsafe unconditional cleanup")
			}
			restore()
			if scenario == "unrelated-update" {
				if err := store.Update(func(cfg *config.Config) error { cfg.DeviceName = "saved name"; return nil }); err != nil {
					t.Fatal(err)
				}
			} else {
				c.retryPhoneRevokes(context.Background())
			}
			if store.PhoneRevocationPending() {
				t.Fatal("recovered disk did not persist the pending revocation")
			}
			reopened, err := config.Open(store.Path())
			if err != nil || reopened.Get().PhoneBindingID != retired.PhoneBindingID || reopened.Get().PhoneHash != "" || len(reopened.Get().PendingPhoneRevokes) != len(retired.PendingPhoneRevokes) {
				t.Fatal("restart lost the persisted revocation or offline cleanup")
			}
			if matched, err := store.RevokePhoneBinding(before.PhoneBindingID, before.PhoneHash); matched || err != nil || store.Get().PhoneBindingID != retired.PhoneBindingID {
				t.Fatal("delayed duplicate revoke changed a newer binding")
			}
		})
	}
}

func TestPhoneRevocationCleanupDoesNotOverwriteRepairedPhone(t *testing.T) {
	store := phoneRevocationStore(t)
	before := store.Get()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	client := &http.Client{Transport: sourceGrantRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/ingress/manifest") {
			return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}
		var body map[string]string
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["bindingId"] != before.PhoneBindingID || body["phoneHash"] != before.PhoneHash {
			t.Error("cleanup did not target the exact retired generation")
		}
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
	})}
	c := New(Options{Store: store, HTTP: client})
	restore := blockPhoneConfigWrites(t, store)
	publishPhoneSnapshot(c, before, false, 2)
	restore()
	go func() { c.retryPhoneRevokes(context.Background()); close(done) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not start after persistence recovered")
	}
	if err := store.Update(func(cfg *config.Config) error {
		cfg.PhoneBindingID, cfg.PhoneHash = strings.Repeat("c", 64), strings.Repeat("d", 64)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	paired := store.Get()
	c.publishPair(PairStatus{Paired: true}, eventIdentity(paired))
	unblock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not finish")
	}
	reopened, err := config.Open(store.Path())
	if err != nil || reopened.Get().PhoneHash != paired.PhoneHash || reopened.Get().PhoneBindingID != paired.PhoneBindingID || len(reopened.Get().PendingPhoneRevokes) != 0 || !c.canUpload(store.Get()) {
		t.Fatal("old cleanup overwrote the new phone pairing")
	}
}

func TestPairClaimSaveFailureDoesNotBecomePhoneRevocation(t *testing.T) {
	store := phoneRevocationStore(t)
	if err := store.Update(func(cfg *config.Config) error {
		cfg.PhonePairPending, cfg.PhonePairAttempt = config.RemoteIdentity(*cfg), strings.Repeat("e", 64)
		cfg.PhonePairExpires = nowMS() + time.Minute.Milliseconds()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c := New(Options{Store: store})
	before := store.Get()
	restore := blockPhoneConfigWrites(t, store)
	publishPhoneSnapshot(c, before, true, 2)
	if current := store.Get(); current.PhoneBindingID != before.PhoneBindingID || current.PhoneHash != before.PhoneHash || len(current.PendingPhoneRevokes) != 0 || store.PhoneRevocationPending() {
		t.Fatal("a paired snapshot with a failed local save revoked all station pairings")
	}
	if c.canUpload(store.Get()) {
		t.Fatal("unpersisted QR claim authorized event upload")
	}
	restore()
	publishPhoneSnapshot(c, before, true, 2)
	if !c.canUpload(store.Get()) || store.Get().PhonePairPending != "" {
		t.Fatal("same pending claim could not recover after the disk became writable")
	}
}
