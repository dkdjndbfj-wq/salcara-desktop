package directrelay

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestValidPhoneBindingRequiresBoundedIDAndSHA256Hex(t *testing.T) {
	hash := sha256.Sum256([]byte("synthetic-binding"))
	good := hex.EncodeToString(hash[:])
	for _, tc := range []struct {
		name, id, hash string
		want           bool
	}{
		{name: "valid", id: "phone-1", hash: good, want: true},
		{name: "missing id", id: "", hash: good},
		{name: "oversized id", id: string(make([]byte, 129)), hash: good},
		{name: "padded id", id: " phone-1 ", hash: good},
		{name: "short hash", id: "phone-1", hash: "deadbeef"},
		{name: "non hex", id: "phone-1", hash: string(make([]byte, 64))},
		{name: "uppercase", id: "phone-1", hash: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validPhoneBinding(tc.id, tc.hash); got != tc.want {
				t.Fatalf("validPhoneBinding() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWebSocketUpgradeRejectsForeignOrigin(t *testing.T) {
	relay := New(Options{DeviceID: "device-test", DeviceName: "test"})
	server := httptest.NewTLSServer(relay.Handler())
	defer server.Close()

	url := "wss" + server.URL[len("https"):] + "/direct/ws"
	transport := server.Client().Transport.(*http.Transport)
	dialer := websocket.Dialer{Subprotocols: []string{"salcara-direct-v1"}, TLSClientConfig: transport.TLSClientConfig}
	_, response, err := dialer.Dial(url, map[string][]string{"Origin": {"https://attacker.example"}})
	if err == nil {
		t.Fatal("foreign browser origin unexpectedly upgraded")
	}
	if response == nil || response.StatusCode != 403 {
		t.Fatalf("foreign origin response = %#v, err = %v; want HTTP 403", response, err)
	}
}

func TestDirectWebSocketRequiresTLS(t *testing.T) {
	relay := New(Options{DeviceID: "device-test", DeviceName: "test"})
	server := httptest.NewServer(relay.Handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/direct/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("plaintext request status = %d, want %d", response.StatusCode, http.StatusUpgradeRequired)
	}
	if relay.IsPaired() || relay.IsConnected() {
		t.Fatal("plaintext request changed relay auth state")
	}
}

func TestDirectPingRequiresTLS(t *testing.T) {
	relay := New(Options{DeviceID: "device-test", DeviceName: "test"})
	server := httptest.NewServer(relay.Handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/direct/ping")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("plaintext ping status = %d, want %d", response.StatusCode, http.StatusUpgradeRequired)
	}
}

func TestPreAuthenticationMessageSizeIsBounded(t *testing.T) {
	relay := New(Options{DeviceID: "device-test", DeviceName: "test"})
	server := httptest.NewTLSServer(relay.Handler())
	defer server.Close()
	transport := server.Client().Transport.(*http.Transport)
	dialer := websocket.Dialer{TLSClientConfig: transport.TLSClientConfig}
	url := "wss" + server.URL[len("https"):] + "/direct/ws"
	conn, _, err := dialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(strings.Repeat("x", maxAuthMessageBytes+1))); err != nil {
		t.Fatalf("write oversized auth frame: %v", err)
	}
	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Fatal("oversized unauthenticated frame was accepted")
	}
	if relay.IsPaired() || relay.IsConnected() {
		t.Fatal("oversized unauthenticated frame changed auth state")
	}
}

func TestAuthenticatedCommandMessageSizeIsBounded(t *testing.T) {
	phoneID := strings.Repeat("a", 64)
	phoneHash := HashPhoneSecret("synthetic-authenticated-frame-secret")
	relay := New(Options{DeviceID: "device-test", DeviceName: "test", PairedPhoneID: phoneID, PairedPhoneHash: phoneHash})
	server := httptest.NewTLSServer(relay.Handler())
	defer server.Close()
	transport := server.Client().Transport.(*http.Transport)
	dialer := websocket.Dialer{TLSClientConfig: transport.TLSClientConfig}
	url := "wss" + server.URL[len("https"):] + "/direct/ws"
	conn, _, err := dialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteJSON(map[string]string{"type": "auth", "phoneId": phoneID, "phoneHash": phoneHash}); err != nil {
		t.Fatalf("write auth frame: %v", err)
	}
	var auth map[string]any
	if err := conn.ReadJSON(&auth); err != nil || auth["type"] != "auth_ok" {
		t.Fatalf("direct auth response = %#v, err=%v", auth, err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(strings.Repeat("x", maxMessageBytes+1))); err != nil {
		t.Fatalf("write oversized authenticated frame: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Fatal("oversized authenticated frame was accepted")
	}
	select {
	case command := <-relay.Commands():
		t.Fatalf("oversized authenticated frame reached command dispatch: %#v", command.payload)
	case <-time.After(25 * time.Millisecond):
	}
	if relay.IsConnected() {
		t.Fatal("relay retained a connection after an oversized authenticated frame")
	}
}

func TestUnauthenticatedConnectionsAreCappedBeforeUpgrade(t *testing.T) {
	relay := New(Options{DeviceID: "device-test", DeviceName: "test"})
	for i := 0; i < maxUnauthenticatedConnections; i++ {
		relay.authSlots <- struct{}{}
	}
	server := httptest.NewTLSServer(relay.Handler())
	defer server.Close()
	response, err := server.Client().Get(server.URL + "/direct/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusServiceUnavailable)
	}
}

func TestSecureURLsRejectPlaintextAndMalformedDestinations(t *testing.T) {
	relay := New(Options{SecureURLs: func() []string {
		return []string{
			"ws://192.168.1.2:47832/direct/ws",
			"wss://192.168.1.2:47832/direct/ws?token=leak",
			"wss://user:pass@192.168.1.2:47832/direct/ws",
			"wss://192.168.1.2:47832/other",
			"wss://192.168.1.2:47832/direct/ws",
		}
	}})
	got := relay.lanURLs()
	if len(got) != 1 || got[0] != "wss://192.168.1.2:47832/direct/ws" {
		t.Fatalf("lanURLs() = %#v, want one configured WSS direct endpoint", got)
	}
}

func TestRefreshingPairingQRInvalidatesOlderToken(t *testing.T) {
	relay := New(Options{
		CertificateSHA256: strings.Repeat("a", sha256.Size*2),
		SecureURLs:        func() []string { return []string{"wss://127.0.0.1:47832/direct/ws"} },
	})
	first, err := relay.GenerateQR()
	if err != nil {
		t.Fatal(err)
	}
	second, err := relay.GenerateQR()
	if err != nil {
		t.Fatal(err)
	}
	firstToken := first["token"].(string)
	secondToken := second["token"].(string)
	if firstToken == secondToken {
		t.Fatal("successive QR generations reused a pairing token")
	}
	if len(relay.pendingTokens) != 1 || relay.pendingTokens[secondToken].IsZero() {
		t.Fatalf("pending pairing tokens = %#v, want only the latest QR token", relay.pendingTokens)
	}
}

func TestDirectListenerDoesNotExposeManagementRoutes(t *testing.T) {
	relay := New(Options{
		CertificateSHA256: strings.Repeat("a", sha256.Size*2),
		SecureURLs:        func() []string { return []string{"wss://192.168.1.2:47832/direct/ws"} },
	})
	server := httptest.NewServer(relay.Handler())
	defer server.Close()

	for _, target := range []struct{ method, path string }{
		{http.MethodPost, "/direct/qr"},
		{http.MethodPost, "/direct/revoke"},
	} {
		request, err := http.NewRequest(target.method, server.URL+target.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("%s %s status = %d, want 404", target.method, target.path, response.StatusCode)
		}
	}
	if got := len(relay.pendingTokens); got != 0 {
		t.Fatalf("unavailable management route minted %d tokens", got)
	}
}

func TestGenerateQRDoesNotMintTokenWithoutConfiguredWSS(t *testing.T) {
	for _, urls := range [][]string{
		nil,
		{"ws://192.168.1.2:47832/direct/ws"},
		{"wss://192.168.1.2:47832/other"},
	} {
		relay := New(Options{SecureURLs: func() []string { return urls }})
		if _, err := relay.GenerateQR(); err == nil {
			t.Fatalf("GenerateQR() unexpectedly succeeded for URLs %#v", urls)
		}
		if got := len(relay.pendingTokens); got != 0 {
			t.Fatalf("invalid endpoint config minted %d tokens", got)
		}
	}
}
