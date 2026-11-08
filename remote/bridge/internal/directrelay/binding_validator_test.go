package directrelay

import (
	"context"
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func newBindingValidatorRelay(t *testing.T) (*Relay, *httptest.Server) {
	t.Helper()
	var server *httptest.Server
	relay := New(Options{
		DeviceID:          "validator-desktop",
		PairedPhoneID:     "validator-phone",
		PairedPhoneHash:   HashPhoneSecret("synthetic-validator-secret"),
		CertificateSHA256: strings.Repeat("e", 64),
		SecureURLs: func() []string {
			return []string{"wss" + strings.TrimPrefix(server.URL, "https") + "/direct/ws"}
		},
	})
	server = httptest.NewTLSServer(relay.Handler())
	t.Cleanup(server.Close)
	t.Cleanup(func() { _ = relay.Close() })
	return relay, server
}

func connectBindingValidatorPhone(t *testing.T, server *httptest.Server, token string) *websocket.Conn {
	t.Helper()
	conn := dialRelayTest(t, server)
	t.Cleanup(func() { _ = conn.Close() })
	writeDirectAuth(t, conn, token, "validator-phone", HashPhoneSecret("synthetic-validator-secret"))
	if typ := readDirectType(t, conn); typ != "auth_ok" {
		t.Fatalf("validator phone failed authentication: %s", typ)
	}
	return conn
}

func captureBindingValidator(t *testing.T, relay *Relay, conn *websocket.Conn) (string, func() error, context.Context) {
	t.Helper()
	if err := conn.WriteJSON(map[string]any{"type": "command", "requestId": "binding-validator", "command": map[string]any{"type": "session.side-question.tools", "action": "open"}}); err != nil {
		t.Fatal(err)
	}
	var command Command
	select {
	case command = <-relay.Commands():
	case <-time.After(2 * time.Second):
		t.Fatal("binding validator command did not arrive")
	}
	if identity, validate := command.BindingValidator(); identity != "" || validate == nil || validate() == nil {
		t.Fatal("unadmitted command obtained a binding capability")
	}
	var identity string
	var validate func() error
	var commandCtx context.Context
	_, err, admitted := relay.RunCurrentCommand(command, func(ctx context.Context, _ map[string]any) (any, error) {
		commandCtx = ctx
		identity, validate = command.BindingValidator()
		if identity == "" || validate == nil || validate() != nil {
			t.Fatal("admitted command failed to capture binding capability")
		}
		return nil, nil
	})
	if err != nil || !admitted {
		t.Fatalf("validator capture failed admission: %v, %v", err, admitted)
	}
	if _, err := hex.DecodeString(identity); err != nil || len(identity) != 64 {
		t.Fatalf("binding identity is not opaque: %q", identity)
	}
	if lateIdentity, lateValidate := command.BindingValidator(); lateIdentity != "" || lateValidate == nil || lateValidate() == nil {
		t.Fatal("completed callback allowed a new binding capability to be captured")
	}
	if validate() != nil {
		t.Fatal("captured validator expired with its callback")
	}
	return identity, validate, commandCtx
}

func TestBindingValidatorRejectsEmptyCommand(t *testing.T) {
	if identity, validate := (Command{}).BindingValidator(); identity != "" || validate == nil || validate() == nil {
		t.Fatal("empty command obtained a binding capability")
	}
}

func TestBindingValidatorSurvivesReconnectButNotRevokeRebindOrClose(t *testing.T) {
	relay, server := newBindingValidatorRelay(t)
	conn := connectBindingValidatorPhone(t, server, "")
	identity, validate, commandCtx := captureBindingValidator(t, relay, conn)
	_ = conn.Close()
	waitRelayDisconnected(t, relay)
	select {
	case <-commandCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("disconnected command context was not cancelled")
	}
	if err := validate(); err != nil {
		t.Fatalf("ordinary disconnect revoked temporary conversation: %v", err)
	}
	conn = connectBindingValidatorPhone(t, server, "")
	reconnectedID, reconnectedValidate, reconnectedCtx := captureBindingValidator(t, relay, conn)
	if reconnectedID != identity || validate() != nil {
		t.Fatal("same phone reconnect changed temporary ownership")
	}
	// Replacing a still-connected socket also preserves the paired identity.
	replacement := connectBindingValidatorPhone(t, server, "")
	replacementID, replacementValidate, _ := captureBindingValidator(t, relay, replacement)
	if replacementID != identity || reconnectedValidate() != nil || reconnectedCtx.Err() == nil {
		t.Fatal("socket replacement did not separate connection and binding lifetime")
	}
	if err := relay.Revoke(); err != nil {
		t.Fatal(err)
	}
	for _, check := range []func() error{validate, reconnectedValidate, replacementValidate} {
		if check() == nil {
			t.Fatal("revoked phone retained temporary conversation authority")
		}
	}
	qr, err := relay.GenerateQR()
	if err != nil {
		t.Fatal(err)
	}
	rebound := connectBindingValidatorPhone(t, server, qr["token"].(string))
	reboundID, reboundValidate, _ := captureBindingValidator(t, relay, rebound)
	if reboundID == identity || validate() == nil {
		t.Fatal("re-pairing the same credentials resurrected old temporary ownership")
	}
	if err := relay.Close(); err != nil {
		t.Fatal(err)
	}
	if reboundValidate() == nil {
		t.Fatal("relay shutdown retained temporary conversation authority")
	}
}

func TestBindingValidatorIdentityCannotCrossRelayRuntimes(t *testing.T) {
	first, firstServer := newBindingValidatorRelay(t)
	second, secondServer := newBindingValidatorRelay(t)
	firstID, firstValidate, _ := captureBindingValidator(t, first, connectBindingValidatorPhone(t, firstServer, ""))
	secondID, secondValidate, _ := captureBindingValidator(t, second, connectBindingValidatorPhone(t, secondServer, ""))
	if firstID == secondID {
		t.Fatal("identical persisted pairing reused ownership across relay runtimes")
	}
	if err := first.Revoke(); err != nil {
		t.Fatal(err)
	}
	if firstValidate() == nil || secondValidate() != nil {
		t.Fatal("validator checked the wrong relay instance")
	}
}
