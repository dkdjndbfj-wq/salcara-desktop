package apiservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/usageledger"
)

func namedSocketServer(t *testing.T, s *Service) string {
	t.Helper()
	server := httptest.NewServer(s)
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http") + "/codex-api/v1/responses"
}
func namedSocket(t *testing.T, address, key string) *websocket.Conn {
	t.Helper()
	c, response, err := websocket.DefaultDialer.Dial(address, http.Header{"Authorization": {"Bearer " + key}})
	if err != nil {
		if response != nil {
			response.Body.Close()
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}
func namedSocketEvent(t *testing.T, c *websocket.Conn, kind string) map[string]any {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(4 * time.Second))
	for {
		var event map[string]any
		if err := c.ReadJSON(&event); err != nil {
			t.Fatal(err)
		}
		if event["type"] == kind {
			return event
		}
		if event["type"] == "error" {
			t.Fatalf("unexpected socket failure: %v", event)
		}
	}
}
func sendNamedFrame(t *testing.T, c *websocket.Conn, frame map[string]any) {
	t.Helper()
	if err := c.WriteJSON(frame); err != nil {
		t.Fatal(err)
	}
}
func waitNamedSocketReleased(t *testing.T, s *Service) {
	t.Helper()
	deadline := time.After(4 * time.Second)
	for {
		active := false
		for _, state := range s.Snapshot() {
			active = active || state.Inflight != 0
		}
		if !active {
			return
		}
		select {
		case <-deadline:
			t.Fatal("socket admission not released")
		case <-time.After(time.Millisecond):
		}
	}
}
func namedCompleted(id string) string {
	return fmt.Sprintf(`data: {"type":"response.completed","response":{"id":%q,"object":"response","status":"completed","model":"gpt-fixture","output":[{"type":"reasoning","encrypted_content":"synthetic-encrypted-history"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"synthetic-tool-output"}]}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`+"\n\n", id)
}

func TestNamedWebSocketAliasesPoliciesCompleteHistoryAndOneUsagePerTurn(t *testing.T) {
	s, live := fixtureService(t)
	live.change(func(c *config.Config) {
		c.CodexAPI.Clients[0].ModelAliases["gpt-fixture"] = "gpt-denied"
		c.CodexAPI.Clients[0].ModelAllowPrefixes = []string{"gpt-"}
		c.CodexAPI.Clients[0].ModelDenyPrefixes = []string{"gpt-denied"}
		c.LocalAccounts[0].ModelDenylist = []string{"gpt-account-denied"}
	})
	var calls atomic.Int32
	s.Official = func(_ context.Context, a config.LocalAccount, body []byte, stream, compact bool, current func() bool) (*http.Response, error) {
		count := calls.Add(1)
		if a.ID != "account-A" || a.CredentialRef != "credential-A" || !stream || compact || !current() {
			t.Error("socket changed protected account/authorization")
		}
		var input map[string]any
		if json.Unmarshal(body, &input) != nil || input["model"] != "gpt-fixture" || input["previous_response_id"] != nil {
			t.Errorf("incorrect alias or local history forwarding: %s", body)
		}
		if count == 2 {
			for _, fragment := range []string{"first private prompt", "synthetic-encrypted-history", "synthetic-tool-output", "next private prompt"} {
				if !strings.Contains(string(body), fragment) {
					t.Error("lost full connection history:", fragment)
				}
			}
		}
		return successfulResponse(io.NopCloser(strings.NewReader(namedCompleted(fmt.Sprintf("named-response-%d", count)))), true), nil
	}
	observed := make(chan usageledger.Observation, 8)
	s.Observe = func(o usageledger.Observation) { observed <- o }
	address := namedSocketServer(t, s)
	c := namedSocket(t, address, syntheticClientKey)
	for _, model := range []string{"gpt-denied", "gpt-account-denied", "gpt-fixture"} {
		sendNamedFrame(t, c, map[string]any{"type": "response.create", "model": model, "generate": false, "input": []any{}})
		if event := namedSocketEvent(t, c, "error"); event["error"].(map[string]any)["code"] != "invalid_model" {
			t.Fatal("policy ignored", event)
		}
	}
	sendNamedFrame(t, c, map[string]any{"type": "response.create", "model": "friendly", "generate": false, "input": "first private prompt"})
	warm := namedSocketEvent(t, c, "response.completed")["response"].(map[string]any)
	if calls.Load() != 0 {
		t.Fatal("warmup executed provider")
	}
	select {
	case <-observed:
		t.Fatal("warmup/denied model created provider usage")
	default:
	}
	sendNamedFrame(t, c, map[string]any{"type": "response.create", "previous_response_id": warm["id"], "input": []any{}})
	first := namedSocketEvent(t, c, "response.completed")["response"].(map[string]any)
	sendNamedFrame(t, c, map[string]any{"type": "response.create", "previous_response_id": first["id"], "input": "next private prompt"})
	namedSocketEvent(t, c, "response.completed")
	for i := 0; i < 2; i++ {
		select {
		case o := <-observed:
			if o.ClientID != "client-A" || o.AccountID != "account-A" || o.Route != "codex-api/v1/responses" || o.State != "completed" || !o.UsageFinal || o.Tokens.Input == nil || *o.Tokens.Input != 7 {
				t.Fatalf("socket usage lost or duplicated: %+v", o)
			}
		case <-time.After(4 * time.Second):
			t.Fatal("socket turn missing observation")
		}
	}
	c.Close()
	waitNamedSocketReleased(t, s)
	select {
	case <-observed:
		t.Fatal("upgrade/disconnect envelope duplicated real turns")
	default:
	}
	stats := s.Snapshot()
	if len(stats) != 1 || stats[0].Requests != 1 || stats[0].Failed != 0 || stats[0].LastStatus != 101 {
		t.Fatalf("upgrade counted as inference/HTTP error: %+v", stats)
	}
	other := namedSocket(t, address, syntheticClientKey)
	sendNamedFrame(t, other, map[string]any{"type": "response.create", "model": "friendly", "previous_response_id": first["id"], "input": "cross socket"})
	if event := namedSocketEvent(t, other, "error"); event["error"].(map[string]any)["code"] != "previous_response_not_found" || calls.Load() != 2 {
		t.Fatal("history crossed sockets")
	}
}

func TestNamedWebSocketUpgradeRequiresAuthorizationAndClientAdmission(t *testing.T) {
	s, live := fixtureService(t)
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		t.Fatal("handshake called inference")
		return nil, nil
	}
	address := namedSocketServer(t, s)
	for _, test := range []struct {
		headers http.Header
		status  int
	}{
		{http.Header{"Authorization": {"Bearer wrong"}}, 401},
		{http.Header{"Authorization": {"Bearer " + syntheticClientKey}, "Origin": {"https://other.invalid"}}, 403},
	} {
		c, response, err := websocket.DefaultDialer.Dial(address, test.headers)
		if c != nil {
			c.Close()
		}
		if err == nil || response == nil || response.StatusCode != test.status {
			t.Fatalf("unauthorized upgrade: response=%v error=%v", response, err)
		}
		response.Body.Close()
	}
	c := namedSocket(t, address, syntheticClientKey)
	blocked, response, err := websocket.DefaultDialer.Dial(address, http.Header{"Authorization": {"Bearer " + syntheticClientKey}})
	if blocked != nil {
		blocked.Close()
	}
	if err == nil || response == nil || response.StatusCode != 429 {
		t.Fatal("socket bypassed per-client concurrency", response, err)
	}
	response.Body.Close()
	other := namedSocket(t, address, syntheticKeyB)
	other.Close()
	c.Close()
	waitNamedSocketReleased(t, s)
	live.change(func(c *config.Config) { c.CodexAPI.Enabled = false })
	disabled, response, err := websocket.DefaultDialer.Dial(address, http.Header{"Authorization": {"Bearer " + syntheticClientKey}})
	if disabled != nil {
		disabled.Close()
	}
	if err == nil || response == nil || response.StatusCode != 503 {
		t.Fatal("disabled service upgraded")
	}
	response.Body.Close()
}

func TestNamedWebSocketRestoredAuthorizationNeverRevivesOldConnection(t *testing.T) {
	for _, change := range []string{"service", "client", "policy", "prefix", "alias", "key", "remove", "runtime", "account-key", "account-policy", "account-remove"} {
		t.Run(change, func(t *testing.T) {
			s, store := storedService(t)
			s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
				t.Fatal("revoked/warmup socket executed inference")
				return nil, nil
			}
			address := namedSocketServer(t, s)
			c := namedSocket(t, address, syntheticClientKey)
			restoreAPIAuthorization(t, store, change)
			sendNamedFrame(t, c, map[string]any{"type": "response.create", "model": "friendly", "generate": false, "input": []any{}})
			if event := namedSocketEvent(t, c, "error"); event["error"].(map[string]any)["code"] != "configuration_changed" {
				t.Fatal("ABA revived old socket", event)
			}
			c.Close()
			waitNamedSocketReleased(t, s)
			fresh := namedSocket(t, address, syntheticClientKey)
			sendNamedFrame(t, fresh, map[string]any{"type": "response.create", "model": "friendly", "generate": false, "input": []any{}})
			namedSocketEvent(t, fresh, "response.completed")
		})
	}
}

func TestNamedWebSocketFailedUpgradeProducesOneObservation(t *testing.T) {
	s, live := fixtureService(t)
	initial := live.get()
	var gets atomic.Int32
	s.Config = func() config.Config {
		value := initial.Clone()
		if gets.Add(1) >= 4 {
			value.LocalAccounts = nil
		}
		return value
	}
	observed := make(chan usageledger.Observation, 4)
	s.Observe = func(o usageledger.Observation) { observed <- o }
	address := namedSocketServer(t, s)
	c, response, err := websocket.DefaultDialer.Dial(address, http.Header{"Authorization": {"Bearer " + syntheticClientKey}})
	if c != nil {
		c.Close()
	}
	if err == nil || response == nil || response.StatusCode != 409 {
		t.Fatal("invalid source upgraded", response, err)
	}
	response.Body.Close()
	select {
	case o := <-observed:
		if o.HTTPStatus != 409 || o.State != "rejected" {
			t.Fatalf("wrong rejected upgrade: %+v", o)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("failed upgrade missing observation")
	}
	select {
	case <-observed:
		t.Fatal("failed upgrade double counted")
	default:
	}
}

func TestNamedWebSocketDisconnectCancelsPendingProtectedExecution(t *testing.T) {
	s, _ := fixtureService(t)
	entered, cancelled := make(chan struct{}), make(chan struct{})
	s.Official = func(ctx context.Context, _ config.LocalAccount, _ []byte, _, _ bool, _ func() bool) (*http.Response, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		return nil, errors.New("synthetic cancelled provider")
	}
	c := namedSocket(t, namedSocketServer(t, s), syntheticClientKey)
	sendNamedFrame(t, c, map[string]any{"type": "response.create", "model": "friendly", "input": "synthetic"})
	awaitDone(t, entered)
	c.Close()
	awaitDone(t, cancelled)
	waitNamedSocketReleased(t, s)
}

func TestNamedWebSocketDrainsStartedStreamAndRejectsNextTurnAfterRevocation(t *testing.T) {
	s, store := storedService(t)
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	var calls atomic.Int32
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		calls.Add(1)
		return successfulResponse(reader, true), nil
	}
	c := namedSocket(t, namedSocketServer(t, s), syntheticClientKey)
	sendNamedFrame(t, c, map[string]any{"type": "response.create", "model": "friendly", "input": "private synthetic input"})
	firstWritten := make(chan struct{})
	go func() {
		defer close(firstWritten)
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"synthetic partial output\"}\n\n")
	}()
	namedSocketEvent(t, c, "response.output_text.delta")
	awaitDone(t, firstWritten)
	restoreAPIAuthorization(t, store, "client")
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, _ = io.WriteString(writer, namedCompleted("drained-named-response"))
		_ = writer.Close()
	}()
	response := namedSocketEvent(t, c, "response.completed")["response"].(map[string]any)
	awaitDone(t, finished)
	sendNamedFrame(t, c, map[string]any{"type": "response.create", "previous_response_id": response["id"], "input": "new turn rejected"})
	if event := namedSocketEvent(t, c, "error"); event["error"].(map[string]any)["code"] != "configuration_changed" || calls.Load() != 1 {
		t.Fatal("revocation replayed or silently switched the started stream", event)
	}
}

func TestNamedWebSocketLateFailureClearsCompletedContinuation(t *testing.T) {
	s, _ := fixtureService(t)
	var calls atomic.Int32
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		calls.Add(1)
		stream := namedCompleted("late-failed-response") + "data: {\"type\":\"error\",\"error\":{\"message\":\"synthetic failure\"}}\n\n"
		return successfulResponse(io.NopCloser(strings.NewReader(stream)), true), nil
	}
	c := namedSocket(t, namedSocketServer(t, s), syntheticClientKey)
	sendNamedFrame(t, c, map[string]any{"type": "response.create", "model": "friendly", "input": "synthetic"})
	namedSocketEvent(t, c, "response.completed")
	namedSocketEvent(t, c, "error")
	sendNamedFrame(t, c, map[string]any{"type": "response.create", "previous_response_id": "late-failed-response", "input": "cannot continue failed chain"})
	if event := namedSocketEvent(t, c, "error"); event["error"].(map[string]any)["code"] != "previous_response_not_found" || calls.Load() != 1 {
		t.Fatal("failed stream left completed history reusable", event, calls.Load())
	}
}
