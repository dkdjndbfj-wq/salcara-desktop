package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/hubclient"
	"salcara/bridge/internal/launcher"
	"salcara/bridge/internal/protocol"
)

// Opt-in integration with an installed Claude executable and a built Bridge
// approval helper. All configuration and inference remain in a temporary,
// loopback-only fixture; no native login or provider credentials are needed.
func TestClaudeNativeDirectAPIExecution(t *testing.T) {
	exe, bridge := os.Getenv("SALCARA_CLAUDE_E2E_EXE"), os.Getenv("SALCARA_CLAUDE_E2E_BRIDGE")
	if exe == "" || bridge == "" {
		t.Skip("set SALCARA_CLAUDE_E2E_EXE and SALCARA_CLAUDE_E2E_BRIDGE to opt in")
	}
	for _, path := range []string{exe, bridge} {
		if st, err := os.Stat(path); err != nil || st.IsDir() {
			t.Fatal("fixture executable is missing")
		}
	}
	root := t.TempDir()
	home, project := filepath.Join(root, "home"), filepath.Join(root, "project")
	for _, path := range []string{home, filepath.Join(project, ".claude")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("ANTHROPIC_API_KEY", "synthetic-stale-ambient-key")
	// Project env is more specific than the user's settings; worker --settings
	// must still win without changing this file.
	projectSettings := []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:1","ANTHROPIC_AUTH_TOKEN":"synthetic-project-key","CLAUDE_CODE_USE_VERTEX":"1"}}`)
	projectFile := filepath.Join(project, ".claude", "settings.local.json")
	if err := os.WriteFile(projectFile, projectSettings, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".claude", "settings.json"), []byte(`{"env":{"CLAUDE_CODE_USE_BEDROCK":"1","ANTHROPIC_BASE_URL":"http://127.0.0.1:2","ANTHROPIC_API_KEY":"synthetic-project-older-key"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newManagementTransportFixture(t, true)
	f.s.d.Store.SetRuntimeGatewayKey("synthetic-claude-direct-process")
	if err := f.s.d.Store.Update(func(c *config.Config) error {
		c.Projects = []protocol.Project{{Path: project, Name: "Claude fixture"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.s.d.Hub = hubclient.New(hubclient.Options{Store: f.s.d.Store,
		DiscoverTools: func(context.Context, map[string]string) []launcher.Tool {
			return []launcher.Tool{{ID: "claude", Kind: "claude", Name: "Claude Code", Available: true}}
		}})
	local := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("native loopback request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		f.h.ServeHTTP(w, r)
	}))
	f.s.d.Port = local.Listener.Addr().(*net.TCPAddr).Port
	mgr := agents.NewManagerWithOptions(func(ev protocol.Event) {
		ev = f.s.Attribute(ev)
		f.s.PublishHubEvent(ev)
		f.s.Broadcast(ev)
	}, func() agents.Settings {
		s := agents.LocalSettingsWithGateway(f.s.d.Store.Get(), f.s.d.Port)
		s.BridgeExe, s.LocalPort, s.LocalToken = bridge, f.s.d.Port, f.s.Token()
		return s
	}, agents.Options{ClaudePath: exe, ClaudeHome: home, CodexPath: filepath.Join(root, "absent-codex"), StateDir: filepath.Join(root, "state")})
	f.s.SetManager(mgr)
	f.s.d.Hub.SetManager(mgr)
	local.Start()
	defer local.Close()
	defer mgr.Close()
	var mu sync.Mutex
	var bodies []string
	var expectedKey atomic.Value
	expectedKey.Store("synthetic-claude-provider")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("synthetic upstream request: %s %s", r.Method, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer "+expectedKey.Load().(string) {
			t.Error("selected API credential did not reach upstream")
		}
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":[{"id":"claude-sonnet-4-6"}]}`)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			t.Error(err)
			return
		}
		if r.URL.Path == "/v1/messages/count_tokens" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"input_tokens":100}`)
			return
		}
		if r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		bodies = append(bodies, string(body))
		n := len(bodies)
		mu.Unlock()
		var request struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Error(err)
		}
		if request.Model != "claude-sonnet-4-6" {
			t.Errorf("wrong selected model: %s", request.Model)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		data := func(event map[string]any) {
			encoded, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], encoded)
		}
		data(map[string]any{"type": "message_start", "message": map[string]any{"id": fmt.Sprintf("msg_fixture_%d", n), "type": "message", "role": "assistant", "model": request.Model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 100, "output_tokens": 0}}})
		stopReason := "end_turn"
		if n == 3 {
			input, _ := json.Marshal(map[string]string{"file_path": filepath.Join(project, "phone-approved.txt"), "content": "phone-approved-fixture\n"})
			data(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "tool_fixture_write", "name": "Write", "input": map[string]any{}}})
			data(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)}})
			stopReason = "tool_use"
		} else {
			data(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
			data(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": fmt.Sprintf("claude-direct-reply-%d", n)}})
		}
		data(map[string]any{"type": "content_block_stop", "index": 0})
		data(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 10}})
		data(map[string]any{"type": "message_stop"})
	}))
	defer upstream.Close()
	phone := f.connect(t, f.token, "phone-claude-native", "auth_ok")
	create := directCreateFixture()
	create["baseUrl"], create["key"], create["wire"], create["models"] = upstream.URL, "synthetic-claude-provider", "anthropic", []string{"claude-sonnet-4-6"}
	created := claudeNativeRequest(t, phone, "create", create)
	handle := created["accountId"].(string)
	claudeNativeRequest(t, phone, "select", map[string]any{"type": "agents.api.set", "agent": "claude", "accountId": handle})
	started := claudeNativeRequest(t, phone, "start", map[string]any{"type": "session.start", "tool": "claude", "controlSurface": "cli", "cwd": project, "prompt": "claude-direct-first-prompt", "model": "claude-sonnet-4-6", "approval": "ask", "operationId": "e6da529b-aa90-4a36-a949-a121984da31f", "accountId": handle})
	sessionKey, _ := started["sessionKey"].(string)
	if !strings.HasPrefix(sessionKey, "claude:") {
		t.Fatal("missing Claude session identity")
	}
	claudeNativeAwaitTurn(t, phone, sessionKey, "claude-direct-reply-1")
	expectedKey.Store("synthetic-claude-B")
	createB := directCreateFixture()
	createB["requestId"] = "create-claude-api-B"
	createB["baseUrl"], createB["key"], createB["wire"], createB["models"] = upstream.URL, "synthetic-claude-B", "anthropic", []string{"claude-sonnet-4-6"}
	handleB := claudeNativeRequest(t, phone, "create-B", createB)["accountId"].(string)
	claudeNativeRequest(t, phone, "select-B", map[string]any{"type": "agents.api.set", "agent": "claude", "accountId": handleB, "sessionKey": sessionKey})
	handleA := handle
	handle = handleB
	claudeNativeRequest(t, phone, "send", map[string]any{"type": "session.send", "sessionKey": sessionKey, "controlSurface": "cli", "text": "claude-direct-second-prompt", "model": "claude-sonnet-4-6", "approval": "ask", "operationId": "6cd12d3e-733c-4c77-9e7a-ff1f3da80573", "accountId": handle})
	claudeNativeAwaitTurn(t, phone, sessionKey, "claude-direct-reply-2")
	mu.Lock()
	validHistory := len(bodies) == 2 && strings.Contains(bodies[1], "claude-direct-first-prompt") && strings.Contains(bodies[1], "claude-direct-reply-1") && strings.Contains(bodies[1], "claude-direct-second-prompt")
	mu.Unlock()
	if !validHistory {
		t.Fatal("native continuation lost API/history")
	}
	expectedKey.Store("synthetic-claude-provider")
	claudeNativeRequest(t, phone, "select-A-again", map[string]any{"type": "agents.api.set", "agent": "claude", "accountId": handleA, "sessionKey": sessionKey})
	handle = handleA
	claudeNativeRequest(t, phone, "write", map[string]any{"type": "session.send", "sessionKey": sessionKey, "controlSurface": "cli", "text": "Create the fixture file after phone approval.", "model": "claude-sonnet-4-6", "approval": "ask", "operationId": "fdaefc18-a08b-4d1e-83ea-b55f80c369b9", "accountId": handle})
	approvalID := claudeNativeAwaitApproval(t, phone, sessionKey)
	if _, err := os.Stat(filepath.Join(project, "phone-approved.txt")); !os.IsNotExist(err) {
		t.Fatal("write happened before phone approval")
	}
	claudeNativeRequest(t, phone, "approve", map[string]any{"type": "approval.respond", "sessionKey": sessionKey, "controlSurface": "cli", "approvalId": approvalID, "decision": "allow"})
	claudeNativeAwaitTurn(t, phone, sessionKey, "claude-direct-reply-4")
	if data, err := os.ReadFile(filepath.Join(project, "phone-approved.txt")); err != nil || string(data) != "phone-approved-fixture\n" {
		t.Fatal("phone approval did not release the native write")
	}
	// Completing a Direct task must release the local workspace reservation
	// even though its events are intentionally kept out of the Hub stream.
	next := claudeNativeRequest(t, phone, "start-after-completion", map[string]any{"type": "session.start", "tool": "claude", "controlSurface": "cli", "cwd": project, "prompt": "Start a fresh task after the prior one completed.", "model": "claude-sonnet-4-6", "approval": "ask", "operationId": "e6f9b2be-5143-46b1-ac3a-67b6fca24f2a", "accountId": handle})
	nextKey, _ := next["sessionKey"].(string)
	if nextKey == "" || nextKey == sessionKey {
		t.Fatal("fresh task did not get a separate identity")
	}
	claudeNativeAwaitTurn(t, phone, nextKey, "claude-direct-reply-5")
	if data, err := os.ReadFile(projectFile); err != nil || string(data) != string(projectSettings) {
		t.Fatal("worker changed project settings")
	}
	t.Log("TLS Direct A→B→A -> same native Claude conversation/history -> phone approval -> native file write -> next task in same workspace passed")
}

func claudeNativeAwaitApproval(t *testing.T, phone *websocket.Conn, session string) string {
	t.Helper()
	_ = phone.SetReadDeadline(time.Now().Add(75 * time.Second))
	for {
		var envelope struct {
			Type string         `json:"type"`
			Data protocol.Event `json:"data"`
		}
		if err := phone.ReadJSON(&envelope); err != nil {
			t.Fatalf("waiting for native approval: %v", err)
		}
		ev := envelope.Data
		if envelope.Type != "event" || ev.SessionKey != session {
			continue
		}
		if ev.Error != "" {
			t.Fatalf("native write failed: %s", ev.Error)
		}
		if ev.Type == "approval.request" && ev.ApprovalID != "" {
			return ev.ApprovalID
		}
		if ev.Type == "turn" && ev.Status == "completed" {
			t.Fatal("write turn completed without phone approval")
		}
	}
}

func claudeNativeRequest(t *testing.T, phone *websocket.Conn, id string, command map[string]any) map[string]any {
	t.Helper()
	if err := phone.WriteJSON(map[string]any{"type": "command", "requestId": id, "command": command}); err != nil {
		t.Fatal(err)
	}
	_ = phone.SetReadDeadline(time.Now().Add(75 * time.Second))
	for {
		var envelope map[string]any
		if err := phone.ReadJSON(&envelope); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if envelope["type"] != "response" || envelope["requestId"] != id {
			continue
		}
		if envelope["error"] != nil {
			t.Fatalf("%s rejected: %v", id, envelope["error"])
		}
		result, _ := envelope["result"].(map[string]any)
		return result
	}
}

func claudeNativeAwaitTurn(t *testing.T, phone *websocket.Conn, session, reply string) {
	t.Helper()
	_ = phone.SetReadDeadline(time.Now().Add(75 * time.Second))
	sawReply := false
	for {
		var envelope struct {
			Type string         `json:"type"`
			Data protocol.Event `json:"data"`
		}
		if err := phone.ReadJSON(&envelope); err != nil {
			t.Fatalf("waiting for native reply: %v", err)
		}
		ev := envelope.Data
		if envelope.Type != "event" || ev.SessionKey != session {
			continue
		}
		if ev.Type == "message" && ev.Role == "assistant" && strings.Contains(ev.Text, reply) {
			sawReply = true
		}
		if ev.Error != "" {
			t.Fatalf("native execution: %s", ev.Error)
		}
		if ev.Type == "turn" && ev.Status == "completed" {
			if !sawReply {
				t.Fatal("turn completed without the native reply reaching the phone")
			}
			return
		}
	}
}
