package hubclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
)

func pendingPairStreamFixture(t *testing.T) (*Client, *config.Store, config.Config) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]string
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "ABCD2345", "ticket": strings.Repeat("a", 64), "expires_at": nowMS() + 60_000, "revision": 1, "attemptId": request["attemptId"]})
	}))
	t.Cleanup(server.Close)
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(cfg *config.Config) error { cfg.ConnectRemote(server.URL, server.URL+"/salcara-hub"); return nil }); err != nil {
		t.Fatal(err)
	}
	c := New(Options{Store: store, HTTP: server.Client()})
	c.status.State = StateConnected
	stream := store.Get()
	if err := c.acceptEventScope(stream, "before-claim"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.StartPair(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c, store, stream
}

func TestFirstQRKeepsStreamUntilClaimAndWaitsForFreshScope(t *testing.T) {
	c, store, stream := pendingPairStreamFixture(t)
	c.handleSSEFor(context.Background(), "hello", `{"eventScope":"before-claim"}`, stream)
	if len(c.wake) != 0 || c.Status().Pairing == nil {
		t.Fatal("pending first QR disconnected the computer before it could be scanned")
	}
	if c.eventScopeFor(store.Get()) != "" {
		t.Fatal("pre-claim scope was promoted into the pending identity")
	}
	pending := store.Get()
	data, _ := json.Marshal(map[string]any{"deviceId": pending.DeviceID, "paired": true, "revision": 2, "bindingId": pending.PhoneBindingID, "phoneHash": strings.Repeat("b", 64), "attemptId": pending.PhonePairAttempt})
	// The Hub sends hello before pair.status on this same ordered stream.
	c.handleSSEFor(context.Background(), "hello", `{"eventScope":"after-claim"}`, stream)
	c.handleSSEFor(context.Background(), "pair.status", string(data), stream)
	bound := store.Get()
	if bound.PhoneHash == "" || bound.PhonePairPending != "" || len(c.wake) != 1 {
		t.Fatal("confirmed QR did not persist the phone and request a fresh stream")
	}
	if c.canUpload(bound) || c.Status().Pairing != nil {
		t.Fatal("paired was published before the new stream obtained its scope")
	}
	guard := commandGuard{identity: config.RemoteIdentity(bound), deviceID: bound.DeviceID, bindingID: bound.PhoneBindingID, phoneHash: bound.PhoneHash, eventScope: "after-claim"}
	if err := c.validateCommandGuard(withCommandGuard(context.Background(), guard), "remote.connections.sync"); err == nil {
		t.Fatal("first phone command crossed the reconnect authorization gap")
	}
	// stream() resets the revision baseline before reading the fresh snapshot.
	c.lastPairRevision = -1
	c.handleSSEFor(context.Background(), "hello", `{"eventScope":"after-claim"}`, bound)
	c.handleSSEFor(context.Background(), "pair.status", string(data), bound)
	if !c.canUpload(bound) || c.Status().Pairing == nil || !c.Status().Pairing.Paired {
		t.Fatal("fresh scope and pair snapshot did not authorize the phone")
	}
	if err := c.validateCommandGuard(withCommandGuard(context.Background(), guard), "remote.connections.sync"); err != nil {
		t.Fatal("fresh phone command rejected:", err)
	}
	guard.eventScope = "before-claim"
	if err := c.validateCommandGuard(withCommandGuard(context.Background(), guard), "remote.connections.sync"); err == nil {
		t.Fatal("pre-claim scope authorized a command")
	}
	guard.eventScope, guard.phoneHash = "after-claim", strings.Repeat("c", 64)
	if err := c.validateCommandGuard(withCommandGuard(context.Background(), guard), "remote.connections.sync"); err == nil {
		t.Fatal("another phone authorized a command")
	}
}

func TestStaleScopeOnlyWaitsForLiveFirstLocalQR(t *testing.T) {
	for _, mutation := range []string{"expired", "revoked", "already-bound"} {
		t.Run(mutation, func(t *testing.T) {
			c, store, stream := pendingPairStreamFixture(t)
			if mutation == "revoked" {
				pending := store.Get()
				if matched, err := store.RevokePhoneBinding(pending.PhoneBindingID, pending.PhoneHash); !matched || err != nil {
					t.Fatal("local revoke failed", err)
				}
			}
			if err := store.Update(func(cfg *config.Config) error {
				switch mutation {
				case "expired":
					cfg.PhonePairExpires = nowMS() - 1
				case "already-bound":
					cfg.PhoneHash = strings.Repeat("b", 64)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			c.handleSSEFor(context.Background(), "hello", `{"eventScope":"stale"}`, stream)
			if len(c.wake) != 1 || c.canUpload(store.Get()) || c.eventScopeFor(store.Get()) != "" {
				t.Fatal("stale scope outside a live first QR was not rejected and reconnected")
			}
		})
	}
}

func TestKickDoesNotPublishPreviousStationAsConnected(t *testing.T) {
	c, store, _ := pendingPairStreamFixture(t)
	if err := store.Update(func(cfg *config.Config) error {
		cfg.ConnectRemote("http://127.0.0.1:1", "http://127.0.0.1:1/salcara-hub")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c.Kick()
	status := c.Status()
	if status.State != StateConnecting || status.HubURL != store.Get().EffectiveHubURL() || status.Pairing != nil {
		t.Fatal("new station inherited the previous stream's connected status")
	}
	if _, err := c.StartPair(context.Background()); err == nil || err.Error() != "电脑尚未连接中转站" {
		t.Fatal("new station allowed QR creation before connecting", err)
	}
}

func TestKickAfterLogoutStaysNotLoggedIn(t *testing.T) {
	c, store, _ := pendingPairStreamFixture(t)
	if err := store.Update(func(cfg *config.Config) error {
		cfg.AccountKey, cfg.RemoteDeviceOnly = "", false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c.status.Error = "previous connection failure"
	called := false
	c.o.OnStatus = func(status Status) {
		called = true
		if status.State != StateNotLoggedIn || c.Status().State != StateNotLoggedIn {
			t.Error("logout status callback did not see the cleared connection")
		}
	}
	c.Kick()
	if !called || c.Status().State != StateNotLoggedIn || c.Status().Error != "" {
		t.Fatal("logout appeared to be reconnecting or retained a stale error")
	}
}

func TestDelayedStreamResponseCannotRestoreRetiredConnection(t *testing.T) {
	for _, mutation := range []string{"station-switch", "same-station-kick", "cancelled"} {
		t.Run(mutation, func(t *testing.T) {
			c, store, _ := pendingPairStreamFixture(t)
			entered, release := make(chan struct{}), make(chan struct{})
			c.streamHTTP = &http.Client{Transport: sourceGrantRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				close(entered)
				<-release
				// Deliberately return success even after cancellation, modeling a
				// response already delivered by net/http just before Kick won.
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
			})}
			connected := false
			c.o.OnStatus = func(status Status) {
				if status.State == StateConnected {
					connected = true
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- c.stream(ctx) }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("stream request did not reach fixture")
			}
			switch mutation {
			case "station-switch":
				if err := store.Update(func(cfg *config.Config) error {
					cfg.ConnectRemote("http://127.0.0.1:1", "http://127.0.0.1:1/salcara-hub")
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				c.Kick()
			case "same-station-kick":
				c.Kick()
			case "cancelled":
				cancel()
				c.setState(StateConnecting, "")
			}
			close(release)
			select {
			case err := <-done:
				if err == nil || connected || c.Status().State == StateConnected {
					t.Fatal("retired stream published connected after its delayed response")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("retired stream did not finish")
			}
		})
	}
}
