package console

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/directrelay"
	"salcara/bridge/internal/hubclient"
)

type managementTransportFixture struct {
	s                *Server
	h                http.Handler
	relay            *directrelay.Relay
	dialer           websocket.Dialer
	url              string
	token            string
	hash             string
	revokePersisting chan struct{}
}

func newManagementTransportFixture(t *testing.T, dispatch bool) *managementTransportFixture {
	t.Helper()
	s, h := newTestServer(t)
	if err := s.d.Store.Update(func(c *config.Config) error { c.DirectRelayEnabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	identity, err := directrelay.LoadOrCreateTLSIdentity(filepath.Join(t.TempDir(), "identity.pem"), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	var server *httptest.Server
	revokePersisting := make(chan struct{})
	relay := directrelay.New(directrelay.Options{
		DeviceID: s.d.Store.Get().DeviceID, CertificateSHA256: identity.Fingerprint,
		SecureURLs: func() []string { return []string{"wss" + strings.TrimPrefix(server.URL, "https") + "/direct/ws"} },
		OnPaired: func(id, hash string) error {
			return s.d.Store.Update(func(c *config.Config) error { c.DirectPhoneID, c.DirectPhoneHash = id, hash; return nil })
		},
		OnRevoked: func() error {
			close(revokePersisting)
			return s.d.Store.Update(func(c *config.Config) error {
				c.DirectRelayEnabled, c.DirectPhoneID, c.DirectPhoneHash = false, "", ""
				return nil
			})
		},
	})
	server = httptest.NewUnstartedServer(relay.Handler())
	server.TLS = &tls.Config{Certificates: []tls.Certificate{identity.Certificate}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	s.directRelay = relay
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = relay.Close(); server.Close() })
	if dispatch {
		go s.runDirectCommands(ctx, relay)
	}
	qr, err := relay.GenerateQR()
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, _ := qr["fingerprint"].(string)
	return &managementTransportFixture{s: s, h: h, relay: relay, revokePersisting: revokePersisting,
		url: "wss" + strings.TrimPrefix(server.URL, "https") + "/direct/ws", token: qr["token"].(string),
		hash: directrelay.HashPhoneSecret("synthetic-management-transport-secret"),
		dialer: websocket.Dialer{HandshakeTimeout: 3 * time.Second, TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, // Exact QR fingerprint replaces public CA trust.
			VerifyConnection: func(state tls.ConnectionState) error {
				if len(state.PeerCertificates) == 0 {
					return errors.New("missing fixture certificate")
				}
				sum := sha256.Sum256(state.PeerCertificates[0].Raw)
				if hex.EncodeToString(sum[:]) != fingerprint {
					return errors.New("fixture certificate pin mismatch")
				}
				return nil
			},
		}},
	}
}

func (f *managementTransportFixture) connect(t *testing.T, token, phoneID, expected string) *websocket.Conn {
	t.Helper()
	conn, _, err := f.dialer.Dial(f.url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := conn.WriteJSON(map[string]string{"type": "auth", "token": token, "phoneId": phoneID, "phoneHash": f.hash}); err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := conn.ReadJSON(&response); err != nil || response["type"] != expected {
		t.Fatalf("auth type = %v, err=%v, want %s", response["type"], err, expected)
	}
	return conn
}

func managementTransportRequest(t *testing.T, conn *websocket.Conn, requestID string, command map[string]any, success bool) (map[string]any, string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := conn.WriteJSON(map[string]any{"type": "command", "requestId": requestID, "command": command}); err != nil {
		t.Fatal(err)
	}
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	if response["type"] != "response" || response["requestId"] != requestID {
		t.Fatal("management response lost request correlation")
	}
	if success && response["error"] != nil {
		t.Fatalf("management rejected: %v", response["error"])
	}
	if !success && response["error"] == nil {
		t.Fatal("forbidden management request succeeded")
	}
	result, _ := response["result"].(map[string]any)
	return result, string(raw)
}

func TestDirectManagementTLSPairCreateStatusReconnectAndRevoke(t *testing.T) {
	f := newManagementTransportFixture(t, true)
	unauthenticated, _, err := f.dialer.Dial(f.url, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = unauthenticated.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := unauthenticated.WriteJSON(map[string]any{"type": "command", "requestId": "unauthenticated-create", "command": directCreateFixture()}); err != nil {
		t.Fatal(err)
	}
	var rejected map[string]any
	if err := unauthenticated.ReadJSON(&rejected); err != nil || rejected["type"] != "error" {
		t.Fatalf("unauthenticated management was not rejected: err=%v", err)
	}
	_ = unauthenticated.Close()
	if len(f.s.d.Store.Get().LocalAccounts) != 0 {
		t.Fatal("unauthenticated command wrote a credential")
	}
	phone := f.connect(t, f.token, "phone-management-transport", "auth_ok")
	cfg := f.s.d.Store.Get()
	if cfg.DirectPhoneID != "phone-management-transport" || cfg.DirectPhoneHash != f.hash {
		t.Fatal("TLS pairing was not persisted")
	}
	created, raw := managementTransportRequest(t, phone, "create-wire-1", directCreateFixture(), true)
	if created["ok"] != true {
		t.Fatal("API create was not acknowledged")
	}
	cfg = f.s.d.Store.Get()
	if len(cfg.LocalAccounts) != 1 {
		t.Fatal("API create did not persist exactly one account")
	}
	account := cfg.LocalAccounts[0]
	for _, private := range []string{account.Key, account.ID, account.BaseURL, f.hash} {
		if strings.Contains(raw, private) {
			t.Fatal("create response disclosed private configuration")
		}
	}
	if !account.RemoteHubBindingsSet || len(account.RemoteHubURLs) != 0 {
		t.Fatal("direct create authorized a Hub")
	}
	setupState.mu.Lock()
	previous := setupState.job
	setupState.job = &setupJob{ID: "private-job-id", Owner: "another-paired-owner", State: "running", Tool: "codex", Log: "private-installer-log", Error: "private-environment-path"}
	setupState.mu.Unlock()
	t.Cleanup(func() { setupState.mu.Lock(); setupState.job = previous; setupState.mu.Unlock() })
	status, raw := managementTransportRequest(t, phone, "status-wire-1", map[string]any{"type": "computer.management.status", "controlSurface": "cli"}, true)
	if status["supported"] != true || status["busy"] != true || status["job"] != nil {
		t.Fatal("status exposed another owner job or lost busy state")
	}
	for _, private := range []string{"private-job-id", "another-paired-owner", "private-installer-log", "private-environment-path", account.Key, f.s.d.Store.Path()} {
		if strings.Contains(raw, private) {
			t.Fatal("status response disclosed private details")
		}
	}
	items, ok := status["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatal("missing management inventory")
	}
	for _, item := range items {
		for key := range item.(map[string]any) {
			if key == "path" || key == "source" || key == "download" {
				t.Fatal("status exposed local inventory metadata")
			}
		}
	}
	for i, command := range []map[string]any{
		{"type": "computer.agent.install", "controlSurface": "cli", "tool": "codex", "confirmed": false, "requestId": "install-wire-0001"},
		{"type": "computer.agent.install", "controlSurface": "cli", "tool": "arbitrary-command", "confirmed": true, "requestId": "install-wire-0002"},
	} {
		managementTransportRequest(t, phone, []string{"install-unconfirmed", "install-unknown"}[i], command, false)
	}
	setupState.mu.Lock()
	if setupState.job.ID != "private-job-id" {
		t.Error("rejected install replaced the current job")
	}
	setupState.mu.Unlock()
	_ = phone.Close()
	reconnected := f.connect(t, "", "phone-management-transport", "auth_ok")
	retried, _ := managementTransportRequest(t, reconnected, "create-wire-retry", directCreateFixture(), true)
	if retried["accountId"] != created["accountId"] || len(f.s.d.Store.Get().LocalAccounts) != 1 {
		t.Fatal("reconnect retry duplicated or changed the API")
	}
	hub := hubclient.New(hubclient.Options{Store: f.s.d.Store})
	if _, err := hub.Dispatch(context.Background(), directCreateFixture()); err == nil {
		t.Fatal("Hub dispatch accepted direct-only credential creation")
	}
	if got := localRequest(f.s, f.h, http.MethodPost, "/api/local/direct/revoke", map[string]any{}); got.Code != http.StatusOK {
		t.Fatalf("local revoke failed: %d", got.Code)
	}
	cfg = f.s.d.Store.Get()
	if cfg.DirectRelayEnabled || cfg.DirectPhoneID != "" || cfg.DirectPhoneHash != "" {
		t.Fatal("revoke retained direct authorization")
	}
	if stale, _, err := f.dialer.Dial(f.url, nil); err == nil {
		_ = stale.Close()
		t.Fatal("revoked and closed relay accepted another connection")
	}
	if len(cfg.LocalAccounts) != 1 || cfg.LocalAccounts[0].Key != account.Key {
		t.Fatal("revoke changed the saved API")
	}
}

func TestDirectManagementTLSQueuedCreateCannotRunAfterRevoke(t *testing.T) {
	f := newManagementTransportFixture(t, false)
	phone := f.connect(t, f.token, "phone-management-queued", "auth_ok")
	if err := phone.WriteJSON(map[string]any{"type": "command", "requestId": "queued-create-wire", "command": directCreateFixture()}); err != nil {
		t.Fatal(err)
	}
	var queued directrelay.Command
	select {
	case queued = <-f.relay.Commands():
	case <-time.After(3 * time.Second):
		t.Fatal("management command was not queued")
	}
	if got := localRequest(f.s, f.h, http.MethodPost, "/api/local/direct/revoke", map[string]any{}); got.Code != http.StatusOK {
		t.Fatalf("revoke failed: %d", got.Code)
	}
	ran := false
	_, err, admitted := f.relay.RunCurrentCommand(queued, func(ctx context.Context, command map[string]any) (any, error) {
		ran = true
		identity, validate := queued.BindingValidator()
		return f.s.dispatchDirectManagement(hubclient.WithDirectExecutionValidator(ctx, identity, validate), command)
	})
	if err != nil || admitted || ran {
		t.Fatalf("revoked queued operation was admitted: admitted=%v ran=%v err=%v", admitted, ran, err)
	}
	if len(f.s.d.Store.Get().LocalAccounts) != 0 {
		t.Fatal("revoked queued create persisted a credential")
	}
}

func TestDirectManagementTLSCreateAndRevokeDoNotInvertStoreAndRelayLocks(t *testing.T) {
	f := newManagementTransportFixture(t, false)
	phone := f.connect(t, f.token, "phone-management-lock-order", "auth_ok")
	if err := phone.WriteJSON(map[string]any{"type": "command", "requestId": "lock-order-create", "command": directCreateFixture()}); err != nil {
		t.Fatal(err)
	}
	var command directrelay.Command
	select {
	case command = <-f.relay.Commands():
	case <-time.After(3 * time.Second):
		t.Fatal("management command was not queued")
	}
	beforeCommit, resumeCheck := make(chan struct{}), make(chan struct{})
	commandDone := make(chan error, 1)
	go func() {
		_, err, admitted := f.relay.RunCurrentCommand(command, func(ctx context.Context, payload map[string]any) (any, error) {
			identity, validate := command.BindingValidator()
			checks := 0
			return f.s.dispatchDirectManagement(hubclient.WithDirectExecutionValidator(ctx, identity, func() error {
				checks++
				if checks == 2 {
					close(beforeCommit)
					<-resumeCheck
				}
				return validate()
			}), payload)
		})
		if !admitted {
			err = errors.New("fixture command was never admitted")
		}
		commandDone <- err
	}()
	select {
	case <-beforeCommit:
	case <-time.After(3 * time.Second):
		close(resumeCheck)
		t.Fatal("create did not reach authorization recheck")
	}
	revokeDone := make(chan error, 1)
	go func() { revokeDone <- f.relay.Revoke() }()
	select {
	case <-f.revokePersisting:
	case <-time.After(3 * time.Second):
		close(resumeCheck)
		t.Fatal("revoke did not reach persistence")
	}
	// The relay writer now owns its lock and is persisting revocation. The
	// create authorization recheck must not hold the Store lock at this point.
	close(resumeCheck)
	select {
	case err := <-commandDone:
		if err == nil {
			t.Fatal("create survived revocation during authorization recheck")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("create deadlocked against revocation persistence")
	}
	select {
	case err := <-revokeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("revoke failed to drain the admitted create")
	}
	if len(f.s.d.Store.Get().LocalAccounts) != 0 {
		t.Fatal("revoked create persisted a credential")
	}
}
