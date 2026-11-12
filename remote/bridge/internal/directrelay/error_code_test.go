package directrelay

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type uncertainApprovalFixtureError struct{}

func (uncertainApprovalFixtureError) Error() string              { return "审批回执未确认" }
func (uncertainApprovalFixtureError) ExecutionErrorCode() string { return "command_delivery_uncertain" }

func TestDirectErrorResponseKeepsTypedOutcome(t *testing.T) {
	for _, typed := range []bool{false, true} {
		t.Run(fmt.Sprint(typed), func(t *testing.T) {
			var server *httptest.Server
			relay := New(Options{PairedPhoneID: "phone-1", PairedPhoneHash: HashPhoneSecret("synthetic-phone-secret"), CertificateSHA256: strings.Repeat("c", 64), SecureURLs: func() []string { return []string{"wss" + strings.TrimPrefix(server.URL, "https") + "/direct/ws"} }})
			server = httptest.NewTLSServer(relay.Handler())
			defer server.Close()
			conn := dialRelayTest(t, server)
			defer conn.Close()
			writeDirectAuth(t, conn, "", "phone-1", HashPhoneSecret("synthetic-phone-secret"))
			if got := readDirectType(t, conn); got != "auth_ok" {
				t.Fatal(got)
			}
			if err := conn.WriteJSON(map[string]any{"type": "command", "requestId": "approval-original-operation", "action": "approve"}); err != nil {
				t.Fatal(err)
			}
			select {
			case command := <-relay.Commands():
				_, err, admitted := relay.RunCurrentCommand(command, func(context.Context, map[string]any) (any, error) {
					if typed {
						return nil, fmt.Errorf("response: %w", uncertainApprovalFixtureError{})
					}
					return nil, errors.New("明确拒绝")
				})
				if !admitted || err == nil || !relay.SendResponse(command, nil, err) {
					t.Fatal("error response not delivered")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("missing command")
			}
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			var response map[string]any
			if err := conn.ReadJSON(&response); err != nil {
				t.Fatal(err)
			}
			if response["requestId"] != "approval-original-operation" || response["type"] != "response" || response["error"] == nil || response["result"] != nil {
				t.Fatal("error response changed identity/outcome", response)
			}
			if typed {
				if response["code"] != "command_delivery_uncertain" {
					t.Fatal("lost typed uncertainty", response)
				}
			} else if _, ok := response["code"]; ok {
				t.Fatal("invented error code", response)
			}
		})
	}
}
