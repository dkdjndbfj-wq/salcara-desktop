package console

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/directrelay"
	"salcara/bridge/internal/hubclient"
	"salcara/bridge/internal/protocol"
)

func TestHubEventPublisherKeepsDirectSessionEventsOnPinnedTransport(t *testing.T) {
	s, _ := newTestServer(t)
	hub := hubclient.New(hubclient.Options{Store: s.d.Store})
	s.d.Hub = hub

	s.rememberDirectSession("codex:direct")
	s.PublishHubEvent(protocol.Event{Type: "message", SessionKey: "codex:direct", Text: "private direct event"})
	if queued := hub.Status().Queued; queued != 0 {
		t.Fatalf("direct session event entered the Hub queue: %d", queued)
	}

	s.PublishHubEvent(protocol.Event{Type: "message", SessionKey: "codex:hub", Text: "ordinary local event"})
	if queued := hub.Status().Queued; queued != 1 {
		t.Fatalf("ordinary event queue count = %d, want 1", queued)
	}

	s.beginDirectSessionStart()
	s.PublishHubEvent(protocol.Event{Type: "message", SessionKey: "codex:starting", Text: "first direct frame"})
	if queued := hub.Status().Queued; queued != 1 {
		t.Fatalf("event emitted during direct session.start entered Hub queue: %d", queued)
	}
	if err := s.endDirectSessionStart("codex:starting", false); err != nil {
		t.Fatal(err)
	}
}

func TestDirectSessionScopedCommandsRequireSessionKey(t *testing.T) {
	for typ := range directSessionScopedCommands {
		t.Run(typ, func(t *testing.T) {
			base := map[string]any{"type": typ, "controlSurface": "cli"}
			if typ == "session.side-question.tools" {
				base["action"] = "open"
			}
			if err := directCommandAllowed(base); err == nil {
				t.Fatal("session-scoped direct command without a session key was accepted")
			}
			base["sessionKey"] = "codex:scoped"
			if err := directCommandAllowed(base); err != nil {
				t.Fatalf("valid session-scoped command was rejected: %v", err)
			}
		})
	}
}

func TestDirectSessionStartRequiresExplicitCLISurface(t *testing.T) {
	command := map[string]any{"type": "session.start", "tool": "codex", "cwd": "C:/work", "prompt": "inspect"}
	if err := directCommandAllowed(command); err == nil {
		t.Fatal("direct session.start without a control surface was accepted")
	}
	command["controlSurface"] = "desktop"
	if err := directCommandAllowed(command); err == nil {
		t.Fatal("direct session.start selected the desktop surface")
	}
	command["controlSurface"] = "cli"
	if err := directCommandAllowed(command); err != nil {
		t.Fatalf("direct CLI session.start rejected: %v", err)
	}
}

func TestDirectModelCatalogRequiresExplicitCLIAndAcceptsTemporaryQuestionOptions(t *testing.T) {
	command := map[string]any{"type": "models.list", "tool": "codex"}
	if err := directCommandAllowed(command); err == nil {
		t.Fatal("missing model catalog surface accepted")
	}
	command["controlSurface"] = "cli"
	if err := directCommandAllowed(command); err != nil {
		t.Fatalf("explicit model catalog rejected: %v", err)
	}
	ask := map[string]any{"type": "session.side-question.ask", "tool": "codex", "sessionKey": "codex:parent", "controlSurface": "cli", "model": "model-a", "effort": "high", "attachments": []any{"11111111-2222-4333-8444-555555555555"}}
	if err := directCommandAllowed(ask); err != nil {
		t.Fatalf("temporary question rejected: %v", err)
	}
	ask["controlSurface"] = "desktop"
	if err := directCommandAllowed(ask); err == nil {
		t.Fatal("temporary question acquired native Work authority")
	}
}

func TestDirectSessionStartRejectsNativeDesktopEntrypoint(t *testing.T) {
	command := map[string]any{
		"type": "session.start", "tool": "claude", "cwd": "C:/work",
		"prompt": "inspect", "controlSurface": "cli", "entrypoint": "claude-desktop",
	}
	if err := directCommandAllowed(command); err == nil {
		t.Fatal("direct session.start accepted the native Claude Desktop entrypoint")
	}
	command["entrypoint"] = ""
	if err := directCommandAllowed(command); err != nil {
		t.Fatalf("empty entrypoint should not change the CLI decision: %v", err)
	}
}

func TestDirectToolsSideQuestionRequiresExplicitOwnerlessCLIAction(t *testing.T) {
	for _, action := range []string{"open", "read", "send", "stop", "approve", "close"} {
		t.Run(action, func(t *testing.T) {
			base := func() map[string]any {
				return map[string]any{"type": "session.side-question.tools", "action": action, "sessionKey": "codex:parent", "controlSurface": "cli", "chainId": "temporary-chain", "operationId": "operation"}
			}
			if err := directCommandAllowed(base()); err != nil {
				t.Fatalf("valid temporary action rejected: %v", err)
			}
			for _, mutate := range []func(map[string]any){
				func(c map[string]any) { c["owner"] = "forged" },
				func(c map[string]any) { c["Owner"] = "forged" },
				func(c map[string]any) { c["Action"] = "send" },
				func(c map[string]any) { c["action"] = "unknown" },
				func(c map[string]any) { delete(c, "action") },
				func(c map[string]any) { c["action"] = []any{"open"} },
				func(c map[string]any) { delete(c, "sessionKey") },
				func(c map[string]any) { c["controlSurface"] = "desktop" },
				func(c map[string]any) { delete(c, "controlSurface") },
			} {
				command := base()
				mutate(command)
				if err := directCommandAllowed(command); err == nil {
					t.Fatalf("invalid temporary action accepted: %#v", command)
				}
			}
		})
	}
}

func TestDirectCommandAllowlistRejectsUnimplementedFrontendAndRuntimeBridges(t *testing.T) {
	for _, typ := range []string{
		"frontend.proxy",
		"frontend.input",
		"port.forward",
		"docker.run",
		"workspace.exec",
		"shell.exec",
	} {
		t.Run(typ, func(t *testing.T) {
			command := map[string]any{
				"type":           typ,
				"controlSurface": "cli",
				"sessionKey":     "codex:tool-rail",
			}
			if err := directCommandAllowed(command); err == nil {
				t.Fatalf("unimplemented direct command %q was accepted", typ)
			}
		})
	}
}

func TestDirectEventSubscriptionsDeliverMultipleSessionsAndFenceBeforeDispatch(t *testing.T) {
	phoneHash := directrelay.HashPhoneSecret("synthetic-direct-event-secret")
	var server *httptest.Server
	s := &Server{directSessionKeys: map[string]struct{}{}, directEventTargets: map[string]directEventSubscription{}}
	var relay *directrelay.Relay
	relay = directrelay.New(directrelay.Options{
		DeviceID: "desktop-events", PairedPhoneID: "phone-events", PairedPhoneHash: phoneHash,
		SecureURLs:     func() []string { return []string{"wss" + strings.TrimPrefix(server.URL, "https") + "/direct/ws"} },
		OnDisconnected: func(target directrelay.EventTarget) { s.clearDirectEventSubscriptionsForTarget(relay, target) },
	})
	server = httptest.NewTLSServer(relay.Handler())
	t.Cleanup(func() { _ = relay.Close(); server.Close() })
	transport, ok := server.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("TLS test transport unavailable")
	}
	dialer := websocket.Dialer{TLSClientConfig: transport.TLSClientConfig}
	url := "wss" + strings.TrimPrefix(server.URL, "https") + "/direct/ws"
	conn, response, err := dialer.Dial(url, nil)
	if err != nil {
		if response != nil {
			t.Fatalf("dial direct relay: %v (HTTP %d)", err, response.StatusCode)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.WriteJSON(map[string]string{"type": "auth", "phoneId": "phone-events", "phoneHash": phoneHash}); err != nil {
		t.Fatal(err)
	}
	var auth map[string]any
	if err := conn.ReadJSON(&auth); err != nil || auth["type"] != "auth_ok" {
		t.Fatalf("direct auth response = %#v, err=%v", auth, err)
	}

	var oldTarget directrelay.EventTarget
	for i, sessionKey := range []string{"codex:first", "claude:second"} {
		requestID := "open-" + strconv.Itoa(i)
		if err := conn.WriteJSON(map[string]any{"type": "command", "requestId": requestID,
			"command": map[string]any{"type": "session.open", "sessionKey": sessionKey, "controlSurface": "cli"}}); err != nil {
			t.Fatal(err)
		}
		var command directrelay.Command
		select {
		case command = <-relay.Commands():
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for session.open command")
		}
		if _, err, admitted := relay.RunCurrentCommand(command, func(context.Context, map[string]any) (any, error) {
			return nil, nil
		}); err != nil || !admitted {
			t.Fatalf("admit session command: err=%v admitted=%v", err, admitted)
		}
		if err := s.setDirectEventSubscription(relay, command.EventTarget(), sessionKey); err != nil {
			t.Fatalf("register %s subscription: %v", sessionKey, err)
		}
		oldTarget = command.EventTarget()
		if !s.IsDirectSessionEvent(protocol.Event{Type: "message", SessionKey: sessionKey}) {
			t.Fatalf("session %s was not fenced before dispatch", sessionKey)
		}
	}

	for _, sessionKey := range []string{"codex:first", "claude:second"} {
		s.broadcastDirectEvent(protocol.Event{Type: "message", SessionKey: sessionKey, Text: "direct-only"})
	}
	for _, sessionKey := range []string{"codex:first", "claude:second"} {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		var envelope struct {
			Type  string         `json:"type"`
			Event string         `json:"event"`
			Data  protocol.Event `json:"data"`
		}
		if err := conn.ReadJSON(&envelope); err != nil {
			t.Fatalf("read direct event for %s: %v", sessionKey, err)
		}
		if envelope.Type != "event" || envelope.Event != "message" || envelope.Data.SessionKey != sessionKey {
			t.Fatalf("event envelope = %#v, want session %s", envelope, sessionKey)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.directEventMu.RLock()
		remaining := len(s.directEventTargets)
		s.directEventMu.RUnlock()
		if remaining == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.directEventMu.RLock()
	remaining := len(s.directEventTargets)
	s.directEventMu.RUnlock()
	if remaining != 0 {
		t.Fatalf("disconnect retained %d stale session routes", remaining)
	}
	if relay.IsCurrentEventTarget(oldTarget) {
		t.Fatal("old event target remained current after disconnect")
	}

	// A late callback from the disconnected connection must not remove routes
	// installed by the new authenticated generation.
	newConn, response, err := dialer.Dial(url, nil)
	if err != nil {
		if response != nil {
			t.Fatalf("reconnect direct relay: %v (HTTP %d)", err, response.StatusCode)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = newConn.Close() })
	if err := newConn.WriteJSON(map[string]string{"type": "auth", "phoneId": "phone-events", "phoneHash": phoneHash}); err != nil {
		t.Fatal(err)
	}
	if err := newConn.ReadJSON(&auth); err != nil || auth["type"] != "auth_ok" {
		t.Fatalf("direct re-auth response = %#v, err=%v", auth, err)
	}
	newSessionKey := "codex:new-generation"
	if err := newConn.WriteJSON(map[string]any{"type": "command", "requestId": "open-new",
		"command": map[string]any{"type": "session.open", "sessionKey": newSessionKey, "controlSurface": "cli"}}); err != nil {
		t.Fatal(err)
	}
	var newCommand directrelay.Command
	select {
	case newCommand = <-relay.Commands():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reconnected session.open command")
	}
	if _, err, admitted := relay.RunCurrentCommand(newCommand, func(context.Context, map[string]any) (any, error) { return nil, nil }); err != nil || !admitted {
		t.Fatalf("admit reconnected session command: err=%v admitted=%v", err, admitted)
	}
	if err := s.setDirectEventSubscription(relay, newCommand.EventTarget(), newSessionKey); err != nil {
		t.Fatalf("register reconnected session subscription: %v", err)
	}
	if err := s.setDirectEventSubscription(relay, oldTarget, "codex:late-old"); err == nil {
		t.Fatal("stale connection target was accepted")
	}
	s.directEventMu.RLock()
	_, retained := s.directEventTargets[newSessionKey]
	_, stale := s.directEventTargets["codex:late-old"]
	s.directEventMu.RUnlock()
	if !retained || stale {
		t.Fatalf("stale callback changed current route: retained=%v stale=%v", retained, stale)
	}
}

func TestDirectWebSocketDispatchCorrelatesResponseAndKeepsSessionEventOutOfHub(t *testing.T) {
	s, _ := newTestServer(t)
	hub := hubclient.New(hubclient.Options{Store: s.d.Store})
	s.d.Hub = hub
	phoneID := "phone-direct-integration"
	phoneHash := directrelay.HashPhoneSecret("synthetic-direct-integration-secret")
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.ActivateRemoteMode("direct")
		c.DirectPhoneID, c.DirectPhoneHash = phoneID, phoneHash
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var server *httptest.Server
	var relay *directrelay.Relay
	relay = directrelay.New(directrelay.Options{
		DeviceID: "desktop-direct-integration", PairedPhoneID: phoneID, PairedPhoneHash: phoneHash,
		SecureURLs:     func() []string { return []string{"wss" + strings.TrimPrefix(server.URL, "https") + "/direct/ws"} },
		OnDisconnected: func(target directrelay.EventTarget) { s.clearDirectEventSubscriptionsForTarget(relay, target) },
	})
	server = httptest.NewTLSServer(relay.Handler())
	t.Cleanup(func() { _ = relay.Close(); server.Close() })
	transport, ok := server.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("TLS test transport unavailable")
	}
	dialer := websocket.Dialer{TLSClientConfig: transport.TLSClientConfig}
	url := "wss" + strings.TrimPrefix(server.URL, "https") + "/direct/ws"
	conn, response, err := dialer.Dial(url, nil)
	if err != nil {
		if response != nil {
			t.Fatalf("dial direct relay: %v (HTTP %d)", err, response.StatusCode)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.WriteJSON(map[string]string{"type": "auth", "phoneId": phoneID, "phoneHash": phoneHash}); err != nil {
		t.Fatalf("write direct auth: %v", err)
	}
	var auth map[string]any
	if err := conn.ReadJSON(&auth); err != nil || auth["type"] != "auth_ok" {
		t.Fatalf("direct auth response = %#v, err=%v", auth, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.runDirectCommands(ctx, relay)
	const requestID = "direct-open-17"
	const sessionKey = "codex:direct-integration"
	if err := conn.WriteJSON(map[string]any{"type": "command", "requestId": requestID, "command": map[string]any{
		"type": "session.open", "sessionKey": sessionKey, "controlSurface": "cli",
	}}); err != nil {
		t.Fatalf("write direct session.open: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var responseFrame map[string]any
	if err := conn.ReadJSON(&responseFrame); err != nil {
		t.Fatalf("read direct command response: %v", err)
	}
	if responseFrame["type"] != "response" || responseFrame["requestId"] != requestID || responseFrame["error"] == nil {
		t.Fatalf("direct response = %#v, want correlated local dispatch error", responseFrame)
	}
	if queued := hub.Status().Queued; queued != 0 {
		t.Fatalf("direct command entered Hub event queue: %d", queued)
	}

	event := protocol.Event{Type: "message", DeviceID: "desktop-direct-integration", SessionKey: sessionKey, Tool: "codex", Text: "direct-only event"}
	s.Broadcast(event)
	s.PublishHubEvent(event)
	if queued := hub.Status().Queued; queued != 0 {
		t.Fatalf("direct session event entered Hub event queue: %d", queued)
	}
	var eventFrame struct {
		Type  string         `json:"type"`
		Event string         `json:"event"`
		Data  protocol.Event `json:"data"`
	}
	if err := conn.ReadJSON(&eventFrame); err != nil {
		t.Fatalf("read direct session event: %v", err)
	}
	if eventFrame.Type != "event" || eventFrame.Event != "message" || eventFrame.Data.SessionKey != sessionKey || eventFrame.Data.Text != event.Text {
		t.Fatalf("direct event = %#v, want event for %s", eventFrame, sessionKey)
	}
}

func TestDirectSessionPrivacyFenceSurvivesSubscriptionClear(t *testing.T) {
	s := &Server{}
	s.rememberDirectSession("codex:direct")
	if !s.IsDirectSessionEvent(protocol.Event{SessionKey: "codex:direct", Type: "message"}) {
		t.Fatal("direct session event was not fenced from Hub")
	}
	s.clearDirectEventSubscription()
	if !s.IsDirectSessionEvent(protocol.Event{SessionKey: "codex:direct", Type: "message"}) {
		t.Fatal("clearing the live subscription removed the process-lifetime Hub privacy fence")
	}
	if s.IsDirectSessionEvent(protocol.Event{SessionKey: "codex:hub", Type: "message"}) {
		t.Fatal("unrelated session was incorrectly fenced")
	}
}

func TestDirectSessionPrivacyFenceSurvivesBridgeRestart(t *testing.T) {
	s, _ := newTestServer(t)
	if err := s.rememberDirectSession("codex:persisted"); err != nil {
		t.Fatalf("rememberDirectSession() = %v", err)
	}
	if got := s.d.Store.Get().DirectSessionKeys; len(got) != 1 || got[0] != "codex:persisted" {
		t.Fatalf("persisted direct session keys = %#v", got)
	}
	restarted := New(Deps{Store: s.d.Store, Logger: s.log})
	t.Cleanup(restarted.CloseUsageLedger)
	if !restarted.IsDirectSessionEvent(protocol.Event{SessionKey: "codex:persisted", Type: "message"}) {
		t.Fatal("direct session privacy fence was lost after Bridge reconstruction")
	}
}

func TestDirectSessionStartFenceIsDurableBeforeDispatch(t *testing.T) {
	s, _ := newTestServer(t)
	if err := s.beginDirectSessionStart(); err != nil {
		t.Fatalf("beginDirectSessionStart() = %v", err)
	}
	if got := s.d.Store.Get().DirectSessionStartsPending; got != 1 {
		t.Fatalf("persisted in-flight direct starts = %d, want 1 before dispatch", got)
	}
	restarted := New(Deps{Store: s.d.Store, Logger: s.log})
	t.Cleanup(restarted.CloseUsageLedger)
	if !restarted.IsDirectSessionEvent(protocol.Event{SessionKey: "codex:maybe-direct", Type: "message"}) {
		t.Fatal("restart during an unknown direct session start did not close the Hub event fence")
	}
	if !s.d.Store.Get().DirectSessionFenceUnknown {
		t.Fatal("recovered unknown direct session start was not persisted")
	}
}

func TestCompletedDirectSessionStartPersistsKeyAndRetiresPendingMarker(t *testing.T) {
	s, _ := newTestServer(t)
	if err := s.beginDirectSessionStart(); err != nil {
		t.Fatal(err)
	}
	if err := s.endDirectSessionStart("codex:completed", false); err != nil {
		t.Fatalf("endDirectSessionStart() = %v", err)
	}
	got := s.d.Store.Get()
	if got.DirectSessionStartsPending != 0 || got.DirectSessionFenceUnknown {
		t.Fatalf("completed direct start left a pending/global fence: pending=%d unknown=%v", got.DirectSessionStartsPending, got.DirectSessionFenceUnknown)
	}
	if len(got.DirectSessionKeys) != 1 || got.DirectSessionKeys[0] != "codex:completed" {
		t.Fatalf("completed direct session keys = %#v", got.DirectSessionKeys)
	}
}

func TestUnknownDirectSessionStartFenceSurvivesRestart(t *testing.T) {
	s, _ := newTestServer(t)
	if err := s.markDirectStartUnknown(); err != nil {
		t.Fatalf("markDirectStartUnknown() = %v", err)
	}
	restarted := New(Deps{Store: s.d.Store, Logger: s.log})
	t.Cleanup(restarted.CloseUsageLedger)
	if !restarted.IsDirectSessionEvent(protocol.Event{SessionKey: "codex:any", Type: "message"}) {
		t.Fatal("unknown direct start fence was lost after Bridge reconstruction")
	}
}

func TestDirectSessionStartCrashFenceIsEnforcedAtHubPublisher(t *testing.T) {
	server, _ := newTestServer(t)
	firstHub := hubclient.New(hubclient.Options{Store: server.d.Store})
	server.d.Hub = firstHub
	if err := server.beginDirectSessionStart(); err != nil {
		t.Fatalf("beginDirectSessionStart() = %v", err)
	}
	server.PublishHubEvent(protocol.Event{Type: "message", SessionKey: "codex:emitted-during-start", Text: "private"})
	if queued := firstHub.Status().Queued; queued != 0 {
		t.Fatalf("in-flight direct session event entered Hub before a crash: %d", queued)
	}

	// Reconstructing the Bridge models a process crash after dispatch but before
	// the generated session key was durably returned. Startup must persist the
	// unknown fence before any later event can reach a newly connected Hub.
	restarted := New(Deps{Store: server.d.Store, Logger: server.log})
	t.Cleanup(restarted.CloseUsageLedger)
	secondHub := hubclient.New(hubclient.Options{Store: server.d.Store})
	restarted.d.Hub = secondHub
	for _, key := range []string{"codex:emitted-during-start", "codex:another-session"} {
		restarted.PublishHubEvent(protocol.Event{Type: "message", SessionKey: key, Text: "must remain private"})
	}
	if queued := secondHub.Status().Queued; queued != 0 {
		t.Fatalf("recovered unknown start fence allowed a session event into Hub after reconnect: %d", queued)
	}
	if !server.d.Store.Get().DirectSessionFenceUnknown {
		t.Fatal("crash recovery did not durably retain the unknown session fence")
	}
}

func TestDirectSessionStartFailsClosedUntilGeneratedKeyIsKnown(t *testing.T) {
	s := &Server{}
	s.beginDirectSessionStart()
	if !s.IsDirectSessionEvent(protocol.Event{SessionKey: "codex:unrelated", Type: "message"}) {
		t.Fatal("session event passed Hub while direct start had no known key")
	}
	if err := s.endDirectSessionStart("codex:created", false); err != nil {
		t.Fatal(err)
	}
	if !s.IsDirectSessionEvent(protocol.Event{SessionKey: "codex:created", Type: "message"}) {
		t.Fatal("generated direct session was not fenced after start")
	}
	if s.IsDirectSessionEvent(protocol.Event{SessionKey: "codex:unrelated", Type: "message"}) {
		t.Fatal("known direct session left an unrelated session fenced after start")
	}
}

func TestUncertainDirectSessionStartKeepsHubFenceClosed(t *testing.T) {
	s := &Server{}
	s.beginDirectSessionStart()
	s.markDirectStartUnknown()
	if err := s.endDirectSessionStart("", true); err != nil {
		t.Fatal(err)
	}
	if !s.IsDirectSessionEvent(protocol.Event{SessionKey: "codex:unknown", Type: "message"}) {
		t.Fatal("uncertain direct start did not preserve fail-closed Hub isolation")
	}
}

func TestDirectPairingQRStartsOptInTLSListenerAndRevokePersists(t *testing.T) {
	s, h := newTestServer(t)
	s.directListenAddr = "0.0.0.0:0"
	s.directHostProvider = func() []string { return []string{"192.168.1.20"} }
	if got := localRequest(s, h, http.MethodGet, "/api/local/direct/status", nil); got.Code != http.StatusOK {
		t.Fatalf("status code = %d: %s", got.Code, got.Body)
	} else {
		var status map[string]any
		if err := json.Unmarshal(got.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if status["running"] != false || status["enabled"] != false {
			t.Fatalf("status enabled listener before opt-in: %#v", status)
		}
	}
	if s.d.Store.Get().DirectRelayEnabled {
		t.Fatal("status request opted into direct relay")
	}

	qr := localRequest(s, h, http.MethodPost, "/api/local/direct/qr", map[string]any{})
	if qr.Code != http.StatusOK || qr.Header().Get("Content-Type") != "application/json; charset=utf-8" || qr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("QR response = %d %q %q", qr.Code, qr.Header().Get("Content-Type"), qr.Header().Get("Cache-Control"))
	}
	var qrData map[string]any
	if err := json.Unmarshal(qr.Body.Bytes(), &qrData); err != nil {
		t.Fatal(err)
	}
	dataURL, ok := qrData["qrDataURL"].(string)
	if !ok || !strings.HasPrefix(dataURL, "data:image/png;base64,") {
		t.Fatal("direct QR response does not contain a PNG data URL")
	}
	png, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(dataURL, "data:image/png;base64,"))
	if err != nil || !bytes.HasPrefix(png, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatalf("invalid QR PNG: err=%v", err)
	}
	stored := s.d.Store.Get()
	if !stored.DirectRelayEnabled || len(stored.DirectRelayHosts) != 1 || stored.DirectRelayHosts[0] != "192.168.1.20" {
		t.Fatalf("direct pairing opt-in was not persisted: %+v", stored)
	}

	statusResponse := localRequest(s, h, http.MethodGet, "/api/local/direct/status", nil)
	var status map[string]any
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	// This fixture intentionally has no local Agent dispatcher, so the secure
	// pairing listener is available while command dispatch remains unavailable.
	if status["running"] != true || status["pairingSupported"] != true || status["fingerprint"] == "" || status["commandDispatch"] != false || status["mobileSupported"] != true {
		t.Fatalf("direct listener did not report active TLS pairing: %#v", status)
	}
	if urls, ok := status["secureUrls"].([]any); !ok || len(urls) != 1 || !strings.HasPrefix(urls[0].(string), "wss://192.168.1.20:") {
		t.Fatalf("secure local URL missing: %#v", status["secureUrls"])
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	pingURL := "https://127.0.0.1:" + strconv.Itoa(int(status["port"].(float64))) + "/direct/ping"
	response, err := client.Get(pingURL)
	if err != nil {
		t.Fatalf("dedicated direct listener did not accept TLS: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("direct TLS ping status = %d", response.StatusCode)
	}

	// Seed the direct-only binding fields to verify revoke uses the Store hook,
	// not the independent Hub pairing state.
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.DirectPhoneID = "phone-id"
		c.DirectPhoneHash = strings.Repeat("a", 64)
		// Direct pairing is a separate peer authorization. Revoking it must not
		// clear the existing Hub phone pairing or make the Hub path unusable.
		c.PhoneBindingID = "hub-phone-id"
		c.PhoneHash = strings.Repeat("b", 64)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := localRequest(s, h, http.MethodPost, "/api/local/direct/revoke", map[string]any{}); got.Code != http.StatusOK {
		t.Fatalf("revoke code = %d: %s", got.Code, got.Body)
	}
	stored = s.d.Store.Get()
	if stored.DirectPhoneID != "" || stored.DirectPhoneHash != "" || stored.DirectRelayEnabled || len(stored.DirectRelayHosts) != 0 {
		t.Fatalf("direct phone authorization survived revoke: %+v", stored)
	}
	if stored.PhoneBindingID != "hub-phone-id" || stored.PhoneHash != strings.Repeat("b", 64) {
		t.Fatalf("direct revoke changed the separate Hub phone authorization: %+v", stored)
	}
	if s.directHTTP != nil || s.directRelay != nil {
		t.Fatal("revocation left the direct pairing listener running")
	}
}

func TestDirectPairingListenerBindsOnlyAdvertisedLocalAddresses(t *testing.T) {
	listeners, hosts, port, err := openDirectListeners("127.0.0.1:0", []string{"127.0.0.1", "192.0.2.254"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	if port == 0 || len(listeners) != 1 || len(hosts) != 1 || hosts[0] != "127.0.0.1" {
		t.Fatalf("listeners=%d hosts=%#v port=%d", len(listeners), hosts, port)
	}
	if got := listeners[0].Addr().String(); got != net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) {
		t.Fatalf("listener unexpectedly bound to %q", got)
	}
}

func TestEligibleDirectIPRequiresRoutablePrivateOrVPNAddress(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "::1", "0.0.0.0", "::", "169.254.1.2", "224.0.0.1", "8.8.8.8"} {
		if ip := net.ParseIP(raw); ip == nil || eligibleDirectIP(ip) {
			t.Errorf("eligibleDirectIP(%q) = %v, want false", raw, ip != nil && eligibleDirectIP(ip))
		}
	}
	for _, raw := range []string{"192.168.1.20", "10.2.3.4", "172.20.1.2", "100.100.0.8", "fd00::12"} {
		if ip := net.ParseIP(raw); ip == nil || !eligibleDirectIP(ip) {
			t.Errorf("eligibleDirectIP(%q) = %v, want true", raw, ip != nil && eligibleDirectIP(ip))
		}
	}
}
