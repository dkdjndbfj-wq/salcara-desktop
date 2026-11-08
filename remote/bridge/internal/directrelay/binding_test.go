package directrelay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestPhoneBindingRequiresStableSafeIdentity(t *testing.T) {
	hash := sha256.Sum256([]byte("synthetic-secret"))
	credential := hex.EncodeToString(hash[:])
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{id: "phone-1", want: true},
		{id: "Phone_1.test", want: true},
		{id: "", want: false},
		{id: "phone 1", want: false},
		{id: "phone\n1", want: false},
		{id: "phone/1", want: false},
		{id: strings.Repeat("a", 129), want: false},
	} {
		t.Run(tc.id, func(t *testing.T) {
			if got := validPhoneBinding(tc.id, credential); got != tc.want {
				t.Fatalf("validPhoneBinding(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}

func TestDirectPairingPersistsBindingAndRejectsDifferentPhoneOnReconnect(t *testing.T) {
	var server *httptest.Server
	var persistedMu sync.Mutex
	var persistedID, persistedHash string
	relay := New(Options{
		DeviceID:          "desktop-1",
		CertificateSHA256: strings.Repeat("a", 64),
		OnPaired: func(id, hash string) error {
			persistedMu.Lock()
			defer persistedMu.Unlock()
			persistedID, persistedHash = id, hash
			return nil
		},
		OnRevoked: func() error {
			persistedMu.Lock()
			defer persistedMu.Unlock()
			persistedID, persistedHash = "", ""
			return nil
		},
		SecureURLs: func() []string {
			return []string{"wss" + strings.TrimPrefix(server.URL, "https") + "/direct/ws"}
		},
	})
	server = httptest.NewTLSServer(relay.Handler())
	defer server.Close()

	qr, err := relay.GenerateQR()
	if err != nil {
		t.Fatal(err)
	}
	if qr["fingerprint"] != strings.Repeat("a", 64) {
		t.Fatalf("QR fingerprint = %#v, want pinned server certificate fingerprint", qr["fingerprint"])
	}
	token, ok := qr["token"].(string)
	if !ok || token == "" {
		t.Fatalf("QR token = %#v, want non-empty one-time token", qr["token"])
	}
	phoneHash := HashPhoneSecret("synthetic-phone-binding-secret")

	conn := dialRelayTest(t, server)
	writeDirectAuth(t, conn, token, "phone-1", phoneHash)
	if got := readDirectType(t, conn); got != "auth_ok" {
		t.Fatalf("first pairing response = %q, want auth_ok", got)
	}
	persistedMu.Lock()
	pairingPersisted := persistedID == "phone-1" && persistedHash == phoneHash
	persistedMu.Unlock()
	if !pairingPersisted {
		t.Fatal("pairing callback did not persist the accepted phone identity")
	}
	conn.Close()
	waitRelayDisconnected(t, relay)

	wrongPhone := dialRelayTest(t, server)
	writeDirectAuth(t, wrongPhone, "", "different-phone", phoneHash)
	if got := readDirectType(t, wrongPhone); got != "error" {
		t.Fatalf("different phone reconnect response = %q, want error", got)
	}
	wrongPhone.Close()
	if relay.IsConnected() {
		t.Fatal("reconnect from a different phone identity was accepted")
	}

	correctPhone := dialRelayTest(t, server)
	writeDirectAuth(t, correctPhone, "", "phone-1", phoneHash)
	if got := readDirectType(t, correctPhone); got != "auth_ok" {
		t.Fatalf("matching phone reconnect response = %q, want auth_ok", got)
	}
	if err := relay.Revoke(); err != nil {
		t.Fatalf("Revoke() = %v", err)
	}
	persistedMu.Lock()
	pairingCleared := persistedID == "" && persistedHash == ""
	persistedMu.Unlock()
	if !pairingCleared || relay.IsPaired() || relay.IsConnected() {
		t.Fatal("revoke did not clear runtime and persisted phone binding")
	}
	correctPhone.Close()
}

func TestPairingPersistenceFailureRejectsConnectionAndConsumesToken(t *testing.T) {
	var server *httptest.Server
	relay := New(Options{
		CertificateSHA256: strings.Repeat("b", 64),
		OnPaired:          func(string, string) error { return errors.New("synthetic disk error") },
		SecureURLs: func() []string {
			return []string{"wss" + strings.TrimPrefix(server.URL, "https") + "/direct/ws"}
		},
	})
	server = httptest.NewTLSServer(relay.Handler())
	defer server.Close()
	qr, err := relay.GenerateQR()
	if err != nil {
		t.Fatal(err)
	}
	token := qr["token"].(string)
	conn := dialRelayTest(t, server)
	writeDirectAuth(t, conn, token, "phone-1", HashPhoneSecret("synthetic-phone-secret"))
	if got := readDirectType(t, conn); got != "error" {
		t.Fatalf("pairing persistence failure response = %q, want error", got)
	}
	if relay.IsPaired() || relay.IsConnected() {
		t.Fatal("pairing persistence failure left the relay authorized")
	}
	if _, exists := relay.pendingTokens[token]; exists {
		t.Fatal("failed persistence left a usable pairing token")
	}
}

func TestOnlyLatestPairingQRCanClaimAndBoundPhoneCannotBeReplaced(t *testing.T) {
	var server *httptest.Server
	relay := New(Options{
		DeviceID:          "desktop-1",
		CertificateSHA256: strings.Repeat("d", 64),
		SecureURLs: func() []string {
			return []string{"wss" + strings.TrimPrefix(server.URL, "https") + "/direct/ws"}
		},
	})
	server = httptest.NewTLSServer(relay.Handler())
	defer server.Close()

	first, err := relay.GenerateQR()
	if err != nil {
		t.Fatal(err)
	}
	latest, err := relay.GenerateQR()
	if err != nil {
		t.Fatal(err)
	}
	secretHash := HashPhoneSecret("synthetic-first-phone-secret")
	staleConn := dialRelayTest(t, server)
	writeDirectAuth(t, staleConn, first["token"].(string), "stale-phone", secretHash)
	if got := readDirectType(t, staleConn); got != "error" {
		t.Fatalf("stale QR auth response = %q, want error", got)
	}
	if relay.IsPaired() {
		t.Fatal("stale QR paired a phone after a newer QR was generated")
	}

	currentConn := dialRelayTest(t, server)
	writeDirectAuth(t, currentConn, latest["token"].(string), "phone-1", secretHash)
	if got := readDirectType(t, currentConn); got != "auth_ok" {
		t.Fatalf("latest QR auth response = %q, want auth_ok", got)
	}
	if got := len(relay.pendingTokens); got != 0 {
		t.Fatalf("pending tokens after pairing = %d, want 0", got)
	}
	if _, err := relay.GenerateQR(); !errors.Is(err, errPhoneAlreadyPaired) {
		t.Fatalf("GenerateQR() for already paired device = %v, want explicit revoke requirement", err)
	}
	if err := relay.Revoke(); err != nil {
		t.Fatal(err)
	}
}

func TestDirectCommandCarriesValidatedRequestID(t *testing.T) {
	var server *httptest.Server
	relay := New(Options{
		PairedPhoneID:     "phone-1",
		PairedPhoneHash:   HashPhoneSecret("synthetic-phone-secret"),
		CertificateSHA256: strings.Repeat("c", 64),
		SecureURLs: func() []string {
			return []string{"wss" + strings.TrimPrefix(server.URL, "https") + "/direct/ws"}
		},
	})
	server = httptest.NewTLSServer(relay.Handler())
	defer server.Close()
	conn := dialRelayTest(t, server)
	writeDirectAuth(t, conn, "", "phone-1", HashPhoneSecret("synthetic-phone-secret"))
	if got := readDirectType(t, conn); got != "auth_ok" {
		t.Fatalf("auth response = %q, want auth_ok", got)
	}
	if err := conn.WriteJSON(map[string]any{"type": "command", "requestId": "turn-42", "action": "status"}); err != nil {
		t.Fatal(err)
	}
	var accepted Command
	select {
	case command := <-relay.Commands():
		accepted = command
		if !relay.IsCurrentCommand(command) {
			t.Fatal("newly received command is not current")
		}
		if relay.SendResponse(command, map[string]any{"early": true}, nil) {
			t.Fatal("response was accepted before guarded dispatch completed")
		}
		if _, err, admitted := relay.RunCurrentCommand(command, func(_ context.Context, payload map[string]any) (any, error) {
			if payload["action"] != "status" {
				t.Fatalf("guarded payload = %#v, want action preserved", payload)
			}
			return nil, nil
		}); !admitted || err != nil {
			t.Fatalf("newly received command failed guarded access: error=%v admitted=%v", err, admitted)
		}
		if _, err, admitted := relay.RunCurrentCommand(command, func(context.Context, map[string]any) (any, error) {
			t.Fatal("command handle was admitted more than once")
			return nil, nil
		}); admitted || !errors.Is(err, errCommandAlreadyStarted) {
			t.Fatalf("reused command handle result: error=%v admitted=%v", err, admitted)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for direct command")
	}
	if !relay.SendResponse(accepted, map[string]any{"status": "ok"}, nil) {
		t.Fatal("SendResponse rejected a valid command response")
	}
	if relay.SendResponse(accepted, map[string]any{"duplicate": true}, nil) {
		t.Fatal("duplicate response for a completed command was accepted")
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var response map[string]any
	if err := conn.ReadJSON(&response); err != nil {
		t.Fatalf("read correlated response: %v", err)
	}
	if response["type"] != "response" || response["requestId"] != "turn-42" || response["result"].(map[string]any)["status"] != "ok" {
		t.Fatalf("response = %#v, want correlated result", response)
	}

	// Replacing the authenticated socket must not retarget an in-flight reply
	// that was produced for the previous connection generation.
	replacement := dialRelayTest(t, server)
	writeDirectAuth(t, replacement, "", "phone-1", HashPhoneSecret("synthetic-phone-secret"))
	if got := readDirectType(t, replacement); got != "auth_ok" {
		t.Fatalf("replacement auth response = %q, want auth_ok", got)
	}
	if relay.SendEvent(accepted.EventTarget(), "stale", map[string]any{"generation": accepted.sessionGeneration}) {
		t.Fatal("event from the replaced connection was accepted")
	}
	if relay.SendResponse(accepted, map[string]any{"stale": true}, nil) {
		t.Fatal("response from old session generation was accepted after reconnect")
	}

	if err := replacement.WriteJSON(map[string]any{"type": "command", "requestId": "turn-42", "action": "status"}); err != nil {
		t.Fatal(err)
	}
	var replacementCommand Command
	select {
	case replacementCommand = <-relay.Commands():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for replacement command")
	}
	if _, err, admitted := relay.RunCurrentCommand(replacementCommand, func(context.Context, map[string]any) (any, error) {
		return nil, nil
	}); !admitted || err != nil {
		t.Fatalf("replacement command was not admitted: error=%v admitted=%v", err, admitted)
	}
	if !relay.SendResponse(replacementCommand, map[string]any{"status": "current"}, nil) {
		t.Fatal("response for the current generation was rejected")
	}
	_ = replacement.SetReadDeadline(time.Now().Add(2 * time.Second))
	var replacementResponse map[string]any
	if err := replacement.ReadJSON(&replacementResponse); err != nil {
		t.Fatalf("read replacement response: %v", err)
	}
	if replacementResponse["type"] != "response" || replacementResponse["result"].(map[string]any)["status"] != "current" {
		t.Fatalf("replacement response = %#v, want current-generation result", replacementResponse)
	}
	for _, messageType := range []string{"event", "response"} {
		if err := replacement.WriteJSON(map[string]any{"type": messageType, "requestId": "client-forged", "action": "status"}); err != nil {
			t.Fatalf("write client %s frame: %v", messageType, err)
		}
		select {
		case command := <-relay.Commands():
			t.Fatalf("client %s frame was forwarded as a command: %#v", messageType, command)
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err := conn.WriteJSON(map[string]any{"type": "command", "requestId": "bad\nrequest", "action": "status"}); err != nil {
		t.Fatal(err)
	}
	select {
	case command := <-relay.Commands():
		t.Fatalf("invalid request ID command was forwarded: %#v", command)
	case <-time.After(100 * time.Millisecond):
	}
	if err := relay.Revoke(); err != nil {
		t.Fatal(err)
	}
	if relay.IsCurrentCommand(accepted) {
		t.Fatal("command queued before revocation remained executable")
	}
	conn.Close()
}

func TestCommandsCannotCrossRelayInstances(t *testing.T) {
	phoneHash := HashPhoneSecret("synthetic-shared-binding-secret")
	newConnectedRelay := func(requestID string) (*Relay, *websocket.Conn, Command) {
		t.Helper()
		var server *httptest.Server
		relay := New(Options{
			DeviceID:        "same-desktop",
			PairedPhoneID:   "same-phone",
			PairedPhoneHash: phoneHash,
			SecureURLs: func() []string {
				return []string{"wss" + strings.TrimPrefix(server.URL, "https") + "/direct/ws"}
			},
		})
		server = httptest.NewTLSServer(relay.Handler())
		t.Cleanup(server.Close)
		conn := dialRelayTest(t, server)
		writeDirectAuth(t, conn, "", "same-phone", phoneHash)
		if got := readDirectType(t, conn); got != "auth_ok" {
			t.Fatalf("auth response = %q, want auth_ok", got)
		}
		if err := conn.WriteJSON(map[string]any{"type": "command", "requestId": requestID, "action": "status"}); err != nil {
			t.Fatalf("write command: %v", err)
		}
		select {
		case command := <-relay.Commands():
			return relay, conn, command
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for direct command")
			return nil, nil, Command{}
		}
	}

	oldRelay, _, oldCommand := newConnectedRelay("old-relay-command")
	currentRelay, currentConn, currentCommand := newConnectedRelay("current-relay-command")
	if oldCommand.sessionGeneration != currentCommand.sessionGeneration {
		t.Fatalf("test requires colliding generations, old=%d current=%d", oldCommand.sessionGeneration, currentCommand.sessionGeneration)
	}
	if currentRelay.IsCurrentCommand(oldCommand) {
		t.Fatal("command from a replaced Relay instance was treated as current")
	}
	if _, err, admitted := currentRelay.RunCurrentCommand(oldCommand, func(context.Context, map[string]any) (any, error) {
		t.Fatal("command from a replaced Relay instance reached dispatch")
		return nil, nil
	}); admitted || err != nil {
		t.Fatalf("cross-instance command was admitted: error=%v admitted=%v", err, admitted)
	}
	if currentRelay.SendResponse(oldCommand, map[string]any{"stale": true}, nil) {
		t.Fatal("response from a replaced Relay instance was routed to the current phone")
	}
	if currentRelay.SendEvent(oldCommand.EventTarget(), "stale", map[string]any{"source": "old-relay"}) {
		t.Fatal("event target from another Relay instance was accepted")
	}
	var attempts sync.WaitGroup
	var admitted, dispatched atomic.Int32
	for i := 0; i < 16; i++ {
		attempts.Add(1)
		go func() {
			defer attempts.Done()
			_, _, ok := currentRelay.RunCurrentCommand(currentCommand, func(context.Context, map[string]any) (any, error) {
				dispatched.Add(1)
				return nil, nil
			})
			if ok {
				admitted.Add(1)
			}
		}()
	}
	attempts.Wait()
	if admitted.Load() != 1 || dispatched.Load() != 1 {
		t.Fatalf("concurrent command admission ran %d callbacks for %d admitted handles, want one", dispatched.Load(), admitted.Load())
	}

	if !currentRelay.SendResponse(currentCommand, map[string]any{"status": "ok"}, nil) {
		t.Fatal("response for the current Relay instance was rejected")
	}
	if !currentRelay.SendEvent(currentCommand.EventTarget(), "status", map[string]any{"state": "ready"}) {
		t.Fatal("event for the current connection was rejected")
	}
	_ = currentConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var response map[string]any
	if err := currentConn.ReadJSON(&response); err != nil {
		t.Fatalf("read current response: %v", err)
	}
	if response["type"] != "response" || response["requestId"] != "current-relay-command" {
		t.Fatalf("current response = %#v", response)
	}
	var event map[string]any
	if err := currentConn.ReadJSON(&event); err != nil {
		t.Fatalf("read current event: %v", err)
	}
	if event["type"] != "event" || event["event"] != "status" {
		t.Fatalf("current event = %#v", event)
	}
	_ = oldRelay.Revoke()
	_ = currentRelay.Revoke()
}

// A phone reconnect can replace the authenticated socket while a session.start
// is still running on the desktop. The old callback must be cancelled and its
// result must never be delivered to the replacement socket (or any other
// transport). The caller can then explicitly recover the session by listing
// or opening it on the new generation.
func TestReconnectCancelsAdmittedSessionStartAndDropsOldResult(t *testing.T) {
	var server *httptest.Server
	phoneHash := HashPhoneSecret("synthetic-session-start-reconnect-secret")
	relay := New(Options{
		DeviceID:        "desktop-session-start-reconnect",
		PairedPhoneID:   "phone-session-start-reconnect",
		PairedPhoneHash: phoneHash,
		SecureURLs: func() []string {
			return []string{"wss" + strings.TrimPrefix(server.URL, "https") + "/direct/ws"}
		},
	})
	server = httptest.NewTLSServer(relay.Handler())
	defer server.Close()

	oldConn := dialRelayTest(t, server)
	writeDirectAuth(t, oldConn, "", "phone-session-start-reconnect", phoneHash)
	if got := readDirectType(t, oldConn); got != "auth_ok" {
		t.Fatalf("old auth response = %q, want auth_ok", got)
	}
	if err := oldConn.WriteJSON(map[string]any{
		"type": "command", "requestId": "start-old-generation",
		"command": map[string]any{
			"type": "session.start", "tool": "codex", "cwd": "C:/workspace", "prompt": "fixture",
		},
	}); err != nil {
		t.Fatal(err)
	}
	var oldCommand Command
	select {
	case oldCommand = <-relay.Commands():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for session.start command")
	}

	started := make(chan struct{})
	canceled := make(chan struct{})
	runDone := make(chan struct {
		err      error
		admitted bool
	}, 1)
	go func() {
		_, err, admitted := relay.RunCurrentCommand(oldCommand, func(ctx context.Context, payload map[string]any) (any, error) {
			if payload["type"] != "session.start" {
				t.Errorf("payload type = %#v, want session.start", payload["type"])
			}
			close(started)
			<-ctx.Done()
			close(canceled)
			return nil, ctx.Err()
		})
		runDone <- struct {
			err      error
			admitted bool
		}{err: err, admitted: admitted}
	}()
	<-started

	// Re-authentication retires the old generation before publishing the new
	// one. This is the boundary that must cancel a running start.
	newConn := dialRelayTest(t, server)
	writeDirectAuth(t, newConn, "", "phone-session-start-reconnect", phoneHash)
	if got := readDirectType(t, newConn); got != "auth_ok" {
		t.Fatalf("new auth response = %q, want auth_ok", got)
	}
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("reconnect did not cancel the old session.start")
	}
	result := <-runDone
	if !result.admitted || !errors.Is(result.err, context.Canceled) {
		t.Fatalf("old session.start result = admitted:%v err:%v, want admitted with context cancellation", result.admitted, result.err)
	}
	if relay.SendResponse(oldCommand, map[string]any{"sessionKey": "must-not-route"}, nil) {
		t.Fatal("old session.start result was delivered after reconnect")
	}

	// The replacement generation remains usable. Because the interrupted start
	// result is unknown, the phone can first list sessions and recover the new
	// session key from current local Agent state. A same-named request ID is
	// intentionally allowed because correlation is scoped to the socket and
	// generation, not to a global client history.
	if err := newConn.WriteJSON(map[string]any{
		"type": "command", "requestId": "start-old-generation",
		"command": map[string]any{
			"type": "sessions.list", "tool": "codex", "controlSurface": "cli",
		},
	}); err != nil {
		t.Fatal(err)
	}
	var replacement Command
	select {
	case replacement = <-relay.Commands():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for replacement command")
	}
	recovered, err, admitted := relay.RunCurrentCommand(replacement, func(context.Context, map[string]any) (any, error) {
		return map[string]any{"sessions": []map[string]any{{"sessionKey": "codex:recovered", "title": "Recovered task"}}}, nil
	})
	if err != nil || !admitted {
		t.Fatalf("replacement command failed: err=%v admitted=%v", err, admitted)
	}
	if !relay.SendResponse(replacement, recovered, nil) {
		t.Fatal("replacement response was rejected")
	}
	_ = newConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var response map[string]any
	if err := newConn.ReadJSON(&response); err != nil {
		t.Fatalf("read replacement response: %v", err)
	}
	if response["type"] != "response" || response["requestId"] != "start-old-generation" {
		t.Fatalf("replacement response = %#v", response)
	}
	responseResult, ok := response["result"].(map[string]any)
	sessions, sessionsOK := responseResult["sessions"].([]any)
	if !ok || !sessionsOK || len(sessions) != 1 {
		t.Fatalf("fresh session listing did not recover the generated key: %#v", response)
	}
	row, rowOK := sessions[0].(map[string]any)
	if !rowOK || row["sessionKey"] != "codex:recovered" {
		t.Fatalf("fresh session listing did not recover the generated key: %#v", response)
	}
}

func TestRevokeCancelsAndDrainsAdmittedCommand(t *testing.T) {
	var server *httptest.Server
	phoneHash := HashPhoneSecret("synthetic-phone-secret")
	relay := New(Options{
		DeviceID:        "desktop-1",
		PairedPhoneID:   "phone-1",
		PairedPhoneHash: phoneHash,
		SecureURLs: func() []string {
			return []string{"wss" + strings.TrimPrefix(server.URL, "https") + "/direct/ws"}
		},
	})
	server = httptest.NewTLSServer(relay.Handler())
	defer server.Close()
	conn := dialRelayTest(t, server)
	writeDirectAuth(t, conn, "", "phone-1", phoneHash)
	if got := readDirectType(t, conn); got != "auth_ok" {
		t.Fatalf("auth response = %q, want auth_ok", got)
	}
	if err := conn.WriteJSON(map[string]any{"type": "command", "requestId": "revoke-race", "action": "status"}); err != nil {
		t.Fatal(err)
	}
	var command Command
	select {
	case command = <-relay.Commands():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for direct command")
	}

	started := make(chan struct{})
	canceled := make(chan struct{})
	finish := make(chan struct{})
	runDone := make(chan error, 1)
	go func() {
		_, err, admitted := relay.RunCurrentCommand(command, func(ctx context.Context, _ map[string]any) (any, error) {
			close(started)
			<-ctx.Done()
			close(canceled)
			<-finish
			return nil, ctx.Err()
		})
		if !admitted {
			runDone <- errors.New("current command was not admitted")
			return
		}
		runDone <- err
	}()
	<-started

	revokeDone := make(chan error, 1)
	go func() { revokeDone <- relay.Revoke() }()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("revocation did not cancel the in-flight command context")
	}
	select {
	case err := <-revokeDone:
		t.Fatalf("Revoke() returned before admitted command drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(finish)
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("command result error = %v, want context cancellation", err)
	}
	if err := <-revokeDone; err != nil {
		t.Fatalf("Revoke() = %v", err)
	}
	if _, err, admitted := relay.RunCurrentCommand(command, func(context.Context, map[string]any) (any, error) {
		t.Error("revoked command callback ran")
		return nil, nil
	}); admitted || err != nil {
		t.Fatalf("revoked command was admitted after Revoke(): result=%v error=%v admitted=%v", nil, err, admitted)
	}
}

// Revoke must also drain an admitted callback whose socket disappeared before
// the revoke request. The current connection pointer is cleared by readLoop in
// that case, so a session-local wait alone would return too early.
func TestRevokeDrainsAdmittedCommandFromDisconnectedSession(t *testing.T) {
	var server *httptest.Server
	phoneHash := HashPhoneSecret("synthetic-disconnected-revoke-secret")
	relay := New(Options{
		DeviceID:        "desktop-disconnected-revoke",
		PairedPhoneID:   "phone-disconnected-revoke",
		PairedPhoneHash: phoneHash,
		SecureURLs: func() []string {
			return []string{"wss" + strings.TrimPrefix(server.URL, "https") + "/direct/ws"}
		},
	})
	server = httptest.NewTLSServer(relay.Handler())
	defer server.Close()
	conn := dialRelayTest(t, server)
	writeDirectAuth(t, conn, "", "phone-disconnected-revoke", phoneHash)
	if got := readDirectType(t, conn); got != "auth_ok" {
		t.Fatalf("auth response = %q, want auth_ok", got)
	}
	if err := conn.WriteJSON(map[string]any{"type": "command", "requestId": "disconnected-revoke", "action": "status"}); err != nil {
		t.Fatal(err)
	}
	var command Command
	select {
	case command = <-relay.Commands():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for direct command")
	}

	started := make(chan struct{})
	canceled := make(chan struct{})
	finish := make(chan struct{})
	runDone := make(chan error, 1)
	go func() {
		_, err, admitted := relay.RunCurrentCommand(command, func(ctx context.Context, _ map[string]any) (any, error) {
			close(started)
			<-ctx.Done()
			close(canceled)
			<-finish
			return nil, ctx.Err()
		})
		if !admitted {
			runDone <- errors.New("disconnected command was not admitted")
			return
		}
		runDone <- err
	}()
	<-started
	_ = conn.Close()
	waitRelayDisconnected(t, relay)

	revokeDone := make(chan error, 1)
	go func() { revokeDone <- relay.Revoke() }()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("disconnect/revoke did not cancel the admitted command")
	}
	select {
	case err := <-revokeDone:
		t.Fatalf("Revoke() returned before disconnected command drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(finish)
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("command result error = %v, want context cancellation", err)
	}
	if err := <-revokeDone; err != nil {
		t.Fatalf("Revoke() = %v", err)
	}
}

func TestCloseCancelsAndDrainsAdmittedCommandWithoutRevokingBinding(t *testing.T) {
	var server *httptest.Server
	var revoked atomic.Int32
	phoneHash := HashPhoneSecret("synthetic-close-secret")
	relay := New(Options{
		DeviceID:        "desktop-close",
		PairedPhoneID:   "phone-close",
		PairedPhoneHash: phoneHash,
		OnRevoked:       func() error { revoked.Add(1); return nil },
		SecureURLs: func() []string {
			return []string{"wss" + strings.TrimPrefix(server.URL, "https") + "/direct/ws"}
		},
	})
	server = httptest.NewTLSServer(relay.Handler())
	defer server.Close()
	conn := dialRelayTest(t, server)
	writeDirectAuth(t, conn, "", "phone-close", phoneHash)
	if got := readDirectType(t, conn); got != "auth_ok" {
		t.Fatalf("auth response = %q, want auth_ok", got)
	}
	if err := conn.WriteJSON(map[string]any{"type": "command", "requestId": "close-race", "action": "status"}); err != nil {
		t.Fatal(err)
	}
	var command Command
	select {
	case command = <-relay.Commands():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for direct command")
	}

	started := make(chan struct{})
	canceled := make(chan struct{})
	finish := make(chan struct{})
	runDone := make(chan error, 1)
	go func() {
		_, err, admitted := relay.RunCurrentCommand(command, func(ctx context.Context, _ map[string]any) (any, error) {
			close(started)
			<-ctx.Done()
			close(canceled)
			<-finish
			return nil, ctx.Err()
		})
		if !admitted {
			runDone <- errors.New("current command was not admitted")
			return
		}
		runDone <- err
	}()
	<-started

	closeDone := make(chan error, 1)
	go func() { closeDone <- relay.Close() }()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("Close() did not cancel the admitted command")
	}
	select {
	case err := <-closeDone:
		t.Fatalf("Close() returned before admitted command drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(finish)
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("command result error = %v, want context cancellation", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if err := relay.Close(); err != nil {
		t.Fatalf("second Close() = %v", err)
	}
	if !relay.IsPaired() || relay.IsConnected() {
		t.Fatal("Close() must preserve the saved binding and disconnect the session")
	}
	if got := revoked.Load(); got != 0 {
		t.Fatalf("Close() called OnRevoked %d times, want 0", got)
	}
	if relay.IsCurrentCommand(command) {
		t.Fatal("closed relay still considers the command current")
	}
	if _, _, err := relay.issuePairingToken(); !errors.Is(err, errRelayClosed) {
		t.Fatalf("issuePairingToken() after Close() = %v, want errRelayClosed", err)
	}
}

func TestCloseDrainsQueuedCommandsAndRejectsNewConnections(t *testing.T) {
	var server *httptest.Server
	phoneHash := HashPhoneSecret("synthetic-queued-close-secret")
	relay := New(Options{
		DeviceID:        "desktop-queued-close",
		PairedPhoneID:   "phone-queued-close",
		PairedPhoneHash: phoneHash,
	})
	server = httptest.NewTLSServer(relay.Handler())
	defer server.Close()
	conn := dialRelayTest(t, server)
	writeDirectAuth(t, conn, "", "phone-queued-close", phoneHash)
	if got := readDirectType(t, conn); got != "auth_ok" {
		t.Fatalf("auth response = %q, want auth_ok", got)
	}
	if err := conn.WriteJSON(map[string]any{"type": "command", "requestId": "queued-close", "action": "status"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(relay.commands) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(relay.commands) != 1 {
		t.Fatalf("queued direct commands = %d, want 1", len(relay.commands))
	}
	if err := relay.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if got := len(relay.commands); got != 0 {
		t.Fatalf("Close() left %d queued command payloads, want none", got)
	}
	if !relay.IsPaired() {
		t.Fatal("Close() unexpectedly cleared the paired phone binding")
	}
	response, err := server.Client().Get(server.URL + "/direct/ws")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("connection request after Close() status = %d, want 503", response.StatusCode)
	}
}

func TestCloseWaitsForCommandFromAlreadyDisconnectedSession(t *testing.T) {
	var server *httptest.Server
	phoneHash := HashPhoneSecret("synthetic-disconnected-close-secret")
	relay := New(Options{
		DeviceID:        "desktop-disconnected-close",
		PairedPhoneID:   "phone-disconnected-close",
		PairedPhoneHash: phoneHash,
	})
	server = httptest.NewTLSServer(relay.Handler())
	defer server.Close()
	conn := dialRelayTest(t, server)
	writeDirectAuth(t, conn, "", "phone-disconnected-close", phoneHash)
	if got := readDirectType(t, conn); got != "auth_ok" {
		t.Fatalf("auth response = %q, want auth_ok", got)
	}
	if err := conn.WriteJSON(map[string]any{"type": "command", "requestId": "disconnected-close", "action": "status"}); err != nil {
		t.Fatal(err)
	}
	var command Command
	select {
	case command = <-relay.Commands():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for direct command")
	}
	started := make(chan struct{})
	finish := make(chan struct{})
	runDone := make(chan error, 1)
	go func() {
		_, err, admitted := relay.RunCurrentCommand(command, func(ctx context.Context, _ map[string]any) (any, error) {
			close(started)
			<-ctx.Done()
			<-finish
			return nil, ctx.Err()
		})
		if !admitted {
			runDone <- errors.New("current command was not admitted")
			return
		}
		runDone <- err
	}()
	<-started
	_ = conn.Close()
	waitRelayDisconnected(t, relay)

	closeDone := make(chan error, 1)
	go func() { closeDone <- relay.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close() returned before disconnected command callback drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(finish)
	if err := <-runDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("command result error = %v, want context cancellation", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close() = %v", err)
	}
}

func dialRelayTest(t *testing.T, server *httptest.Server) *websocket.Conn {
	t.Helper()
	transport, ok := server.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("httptest TLS transport unavailable")
	}
	dialer := websocket.Dialer{TLSClientConfig: transport.TLSClientConfig}
	url := "wss" + strings.TrimPrefix(server.URL, "https") + "/direct/ws"
	conn, response, err := dialer.Dial(url, nil)
	if err != nil {
		if response != nil {
			t.Fatalf("dial direct relay: %v (HTTP %d)", err, response.StatusCode)
		}
		t.Fatalf("dial direct relay: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func writeDirectAuth(t *testing.T, conn *websocket.Conn, token, phoneID, phoneHash string) {
	t.Helper()
	if err := conn.WriteJSON(map[string]string{"type": "auth", "token": token, "phoneId": phoneID, "phoneHash": phoneHash}); err != nil {
		t.Fatalf("write auth: %v", err)
	}
}

func readDirectType(t *testing.T, conn *websocket.Conn) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read direct response: %v", err)
	}
	var message struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &message); err != nil {
		t.Fatalf("decode direct response: %v", err)
	}
	return message.Type
}

func waitRelayDisconnected(t *testing.T, relay *Relay) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !relay.IsConnected() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("relay session did not close")
}
