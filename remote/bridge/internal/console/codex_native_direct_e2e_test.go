package console

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
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

// The installed app-server is real; HOME, credentials, project and inference
// are isolated fixtures. The helper is tools/protocol-smoke, never a provider.
func TestCodexNativeDirectAPIExecution(t *testing.T) {
	exe, gateway := os.Getenv("SALCARA_CODEX_E2E_EXE"), os.Getenv("SALCARA_CODEX_E2E_GATEWAY")
	if exe == "" || gateway == "" {
		t.Skip("set SALCARA_CODEX_E2E_EXE and SALCARA_CODEX_E2E_GATEWAY to opt in")
	}
	if !filepath.IsAbs(exe) || !filepath.IsAbs(gateway) || filepath.Base(gateway) != "protocol-smoke.exe" {
		t.Fatal("explicit native executable and synthetic gateway required")
	}
	root := t.TempDir()
	home, project := filepath.Join(root, "home"), filepath.Join(root, "project")
	for _, dir := range []string{home, project} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "CODEX_ELECTRON_USER_DATA_PATH", "CLAUDE_USER_DATA_DIR"} {
		t.Setenv(name, home)
	}
	for _, name := range []string{"CODEX_TOOL_PIPE", "CODEX_APP_TOOLS_PIPE_PATH", "CODEX_TOOL_CALL_ID", "CODEX_THREAD_ID", "CODEX_TURN_ID", "OPENAI_BASE_URL", "SUB2API_API_KEY"} {
		t.Setenv(name, "")
	}
	t.Setenv("OPENAI_API_KEY", "synthetic-retired-credential")
	auth := []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"synthetic-retired-credential"}`)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), auth, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("model = \"non-openai-fixture\"\nmodel_provider = \"openai\"\ncli_auth_credentials_store = \"file\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gateway, "chat")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	base, err := bufio.NewReader(stdout).ReadString('\n')
	base = strings.TrimSpace(base)
	if err != nil || !strings.HasPrefix(base, "http://127.0.0.1:") || strings.ContainsAny(strings.TrimPrefix(base, "http://127.0.0.1:"), "/?#@") {
		t.Fatal("synthetic gateway did not expose its loopback address")
	}
	f := newManagementTransportFixture(t, true)
	f.s.d.Store.SetRuntimeGatewayKey("synthetic-codex-direct-process")
	if err = f.s.d.Store.Update(func(c *config.Config) error {
		c.Projects = []protocol.Project{{Path: project, Name: "Codex fixture"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.s.d.Hub = hubclient.New(hubclient.Options{Store: f.s.d.Store, DiscoverTools: func(context.Context, map[string]string) []launcher.Tool {
		return []launcher.Tool{{ID: "codex", Kind: "codex", Name: "Codex", Available: true}}
	}})
	local := httptest.NewUnstartedServer(f.h)
	f.s.d.Port = local.Listener.Addr().(*net.TCPAddr).Port
	mgr := agents.NewManagerWithOptions(func(ev protocol.Event) {
		ev = f.s.Attribute(ev)
		f.s.PublishHubEvent(ev)
		f.s.Broadcast(ev)
	}, func() agents.Settings {
		s := agents.LocalSettingsWithGateway(f.s.d.Store.Get(), f.s.d.Port)
		s.LocalPort, s.LocalToken = f.s.d.Port, f.s.Token()
		return s
	}, agents.Options{CodexPath: exe, ClaudePath: filepath.Join(root, "absent-claude"), ClaudeHome: home, StateDir: filepath.Join(root, "state")})
	f.s.SetManager(mgr)
	f.s.d.Hub.SetManager(mgr)
	local.Start()
	defer local.Close()
	defer mgr.Close()
	p := &codexDirectProbe{conn: f.connect(t, f.token, "phone-codex-native", "auth_ok")}
	create := directCreateFixture()
	create["baseUrl"], create["key"], create["wire"], create["models"] = base+"/gateway/codex", "synthetic-loopback-credential", "responses", []string{"non-openai-fixture"}
	handle := p.request(t, create)["accountId"].(string)
	// B has an independent URL and credential. Only the test proxy translates
	// it to the synthetic provider; an accidental A credential is rejected.
	targetURL, _ := url.Parse(base)
	proxyB := httputil.NewSingleHostReverseProxy(targetURL)
	var bCalls atomic.Int32
	apiB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-codex-B" {
			t.Error("API B received a different credential")
			http.Error(w, "wrong fixture credential", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost {
			bCalls.Add(1)
		}
		r.Header.Set("Authorization", "Bearer synthetic-loopback-credential")
		proxyB.ServeHTTP(w, r)
	}))
	defer apiB.Close()
	createB := directCreateFixture()
	createB["requestId"] = "create-codex-api-B"
	createB["baseUrl"], createB["key"], createB["wire"], createB["models"] = apiB.URL+"/gateway/codex", "synthetic-codex-B", "responses", []string{"non-openai-fixture"}
	handleB := p.request(t, createB)["accountId"].(string)
	p.request(t, map[string]any{"type": "agents.api.set", "agent": "codex", "accountId": handle})
	started := p.request(t, map[string]any{"type": "session.start", "tool": "codex", "cwd": project, "prompt": "NATIVE_SESSION_FIRST_MARKER", "model": "non-openai-fixture", "permissionMode": "ask", "operationId": config.NewUUID(), "accountId": handle})
	key, _ := started["sessionKey"].(string)
	if !strings.HasPrefix(key, "codex:") {
		t.Fatal("missing original Codex identity")
	}
	// A newly-created key is subscribed only once the start receipt exists.
	// Match the app's follow-up snapshot to recover any earlier notifications.
	initial := p.request(t, map[string]any{"type": "session.open", "sessionKey": key})
	initialBytes, _ := json.Marshal(initial["events"])
	var initialEvents []protocol.Event
	if err := json.Unmarshal(initialBytes, &initialEvents); err != nil {
		t.Fatal(err)
	}
	p.events = append(p.events, initialEvents...)
	p.await(t, key, "turn", "completed")
	p.request(t, map[string]any{"type": "agents.api.set", "agent": "codex", "accountId": handleB, "sessionKey": key})
	handleA := handle
	handle = handleB
	p.request(t, map[string]any{"type": "sessions.list", "tool": "codex"})
	p.request(t, map[string]any{"type": "session.send", "sessionKey": key, "text": "NATIVE_SESSION_PHONE_MARKER", "model": "non-openai-fixture", "permissionMode": "ask", "operationId": config.NewUUID(), "accountId": handle})
	approval := p.await(t, key, "approval.request", "")
	if approval.Kind != "command" || !strings.Contains(approval.Text, "Get-Location") {
		encoded, _ := json.Marshal(approval)
		if approval.Kind != "command" || !strings.Contains(string(encoded), "Get-Location") {
			t.Fatal("unexpected non-fixture approval")
		}
	}
	p.request(t, map[string]any{"type": "approval.respond", "sessionKey": key, "approvalId": approval.ApprovalID, "decision": "allow", "accountId": handle})
	p.await(t, key, "turn", "completed")
	p.request(t, map[string]any{"type": "sessions.list", "tool": "codex"})
	p.request(t, map[string]any{"type": "session.send", "sessionKey": key, "text": "NATIVE_SESSION_STOP_MARKER", "model": "non-openai-fixture", "accountId": handle})
	stats := func() map[string]any {
		client := &http.Client{Timeout: 3 * time.Second}
		response, err := client.Get(base + "/fixture-stats")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var result map[string]any
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	waitStat := func(name string) {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if stats()[name] == true {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("synthetic upstream did not confirm %s", name)
	}
	waitStat("stopStarted")
	opened := p.request(t, map[string]any{"type": "session.open", "sessionKey": key})
	active := opened["session"].(map[string]any)["activeTurnId"]
	if active == nil || active == "" {
		t.Fatal("stop target is not exact")
	}
	p.request(t, map[string]any{"type": "session.interrupt", "sessionKey": key, "expectedTurnId": active, "operationId": config.NewUUID()})
	if stopped := p.await(t, key, "turn", "interrupted"); stopped.TurnID != active {
		t.Fatal("another turn was interrupted")
	}
	waitStat("stopCanceled")
	_ = p.conn.Close()
	p = &codexDirectProbe{conn: f.connect(t, "", "phone-codex-native", "auth_ok")}
	recovered := p.request(t, map[string]any{"type": "session.open", "sessionKey": key})
	if recovered["session"].(map[string]any)["lastTerminalTurnId"] != active {
		t.Fatal("reconnected phone lost the interrupted turn")
	}
	p.request(t, map[string]any{"type": "agents.api.set", "agent": "codex", "accountId": handleA, "sessionKey": key})
	handle = handleA
	if bCalls.Load() < 2 {
		t.Fatal("native turns did not execute through API B")
	}
	continuedCommand := map[string]any{"type": "session.continue", "sessionKey": key, "expectedTurnId": active, "text": "NATIVE_SESSION_CONTINUE_MARKER", "model": "non-openai-fixture", "permissionMode": "ask", "operationId": config.NewUUID(), "accountId": handle}
	continued := p.request(t, continuedCommand)
	if done := p.await(t, key, "turn", "completed"); done.TurnID == "" || done.TurnID == active || continued["turnId"] != nil && done.TurnID != continued["turnId"] {
		t.Fatal("continuation changed turn identity")
	}
	calls := stats()["calls"]
	if replay := p.request(t, continuedCommand); !reflect.DeepEqual(continued, replay) || stats()["calls"] != calls {
		t.Fatal("replaying the receipt re-executed input")
	}
	history := p.request(t, map[string]any{"type": "session.open", "sessionKey": key})
	encoded, _ := json.Marshal(history)
	for _, marker := range []string{"NATIVE_SESSION_FIRST_MARKER", "NATIVE_SESSION_PHONE_MARKER", "NATIVE_SESSION_CONTINUE_MARKER", "Synthetic Grok-compatible reply"} {
		if !strings.Contains(string(encoded), marker) {
			t.Fatalf("original history lost %s", marker)
		}
	}
	if current, err := os.ReadFile(filepath.Join(home, "auth.json")); err != nil || string(current) != string(auth) {
		t.Fatal("Direct execution rewrote native authentication")
	}
	t.Logf("Actual Codex TLS Direct: A→B→A on the original thread, retained history, approval, exact stop, reconnect, continue and receipt replay passed; synthetic requests=%v, B requests=%d", calls, bCalls.Load())
}

// Preserve events that arrive before an execution receipt. Native app-server
// notifications and the command response are allowed to arrive in either order.
type codexDirectProbe struct {
	conn   *websocket.Conn
	events []protocol.Event
}

func (p *codexDirectProbe) read(t *testing.T) map[string]any {
	t.Helper()
	_ = p.conn.SetReadDeadline(time.Now().Add(40 * time.Second))
	var value map[string]any
	if err := p.conn.ReadJSON(&value); err != nil {
		t.Fatal(err)
	}
	if value["type"] == "event" {
		body, _ := json.Marshal(value["data"])
		var event protocol.Event
		if err := json.Unmarshal(body, &event); err != nil {
			t.Fatal(err)
		}
		p.events = append(p.events, event)
	}
	return value
}

func (p *codexDirectProbe) request(t *testing.T, command map[string]any) map[string]any {
	t.Helper()
	command["controlSurface"] = "cli"
	id := config.NewUUID()
	if err := p.conn.WriteJSON(map[string]any{"type": "command", "requestId": id, "command": command}); err != nil {
		t.Fatal(err)
	}
	for {
		value := p.read(t)
		if value["type"] != "response" || value["requestId"] != id {
			continue
		}
		if value["error"] != nil {
			t.Fatalf("%v rejected: %v", command["type"], value["error"])
		}
		result, _ := value["result"].(map[string]any)
		return result
	}
}

func (p *codexDirectProbe) await(t *testing.T, key, typ, status string) protocol.Event {
	t.Helper()
	for {
		for len(p.events) > 0 {
			event := p.events[0]
			p.events = p.events[1:]
			if event.SessionKey != key {
				continue
			}
			if event.Error != "" || event.Type == "turn" && event.Status == "failed" {
				t.Fatal(fmt.Sprintf("native execution failed: %s", event.Error))
			}
			if event.Type == typ && (status == "" || event.Status == status) {
				return event
			}
		}
		p.read(t)
	}
}
