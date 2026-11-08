package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"salcara/bridge/internal/protocol"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Opt-in acceptance against an unmodified official source tree and its own
// isolated Python 3.14 environment with the official locked core dependencies.
// This exercises the real Gateway and the production sidecar, not the native
// launcher packaging. Credentials, home, provider and processes are test-owned.
func TestHermesOfficialGatewayWithSyntheticUpstream(t *testing.T) {
	python, source := os.Getenv("SALCARA_HERMES_OFFICIAL_PYTHON"), os.Getenv("SALCARA_HERMES_OFFICIAL_SOURCE")
	if python == "" || source == "" {
		t.Skip("requires explicitly selected isolated official Hermes Python and source")
	}
	if !filepath.IsAbs(python) || !filepath.IsAbs(source) {
		t.Fatal("official fixture paths must be absolute")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	home := t.TempDir()
	config := "updates:\n  check: false\nauxiliary:\n  title_generation:\n    enabled: false\n    model_upgrade_enabled: false\ntelemetry:\n  shared_metrics:\n    enabled: false\n    send: false\n"
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	// An allowlist prevents discovering user providers, proxy credentials or
	// global Hermes state through the test processes' environment.
	env := []string{}
	for _, key := range []string{"SystemRoot", "WINDIR", "COMSPEC", "PATHEXT", "TEMP", "TMP", "PATH"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	env = append(env, "HERMES_HOME="+home, "HOME="+home, "USERPROFILE="+home,
		"APPDATA="+home, "LOCALAPPDATA="+home, "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1",
		"PYTHONNOUSERSITE=1", "HERMES_DISABLE_LAZY_INSTALLS=1", "NO_PROXY=*")
	newCommand := func(args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, python, args...)
		cmd.Dir, cmd.Env = source, env
		prepareCmd(cmd)
		return cmd
	}
	logPath := filepath.Join(home, "official-gateway.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	gateway := newCommand("-m", "hermes_cli.main", "--profile", "default", "gateway", "run", "--quiet")
	gateway.Stdout, gateway.Stderr = logFile, logFile
	if err := gateway.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- gateway.Wait() }()
	defer func() {
		// Only this explicitly spawned foreground fixture is ours to terminate.
		killTree(gateway)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("test-owned official Gateway did not exit")
		}
		if t.Failed() {
			for _, path := range []string{logPath, filepath.Join(home, "logs", "gateway.log")} {
				data, _ := os.ReadFile(path)
				if len(data) > 64<<10 {
					data = data[len(data)-(64<<10):]
				}
				t.Logf("isolated official Gateway %s:\n%s", filepath.Base(path), data)
			}
		}
	}()
	probe := newCommand("-c", `import os,time
from hermes_cli.gateway_runtime import discover_gateway_endpoint
deadline=time.monotonic()+65
while time.monotonic()<deadline:
    state=discover_gateway_endpoint(os.environ['HERMES_HOME'])
    if state.state=='ready':
        print('ready'); break
    time.sleep(.2)
else:
    raise RuntimeError(repr(state))
`)
	if output, err := probe.CombinedOutput(); err != nil {
		t.Fatalf("official Gateway startup: %v; %s", err, output)
	}
	var calls atomic.Int32
	var hold atomic.Bool
	var cancelled atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && (r.URL.Path == "/v1/models" || r.URL.Path == "/models" || r.URL.Path == "/api/v1/models") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"object":"list","data":[{"id":"salcara-hermes-fixture","object":"model","owned_by":"synthetic-fixture","context_length":128000}]}`)
			return
		}
		// Official context discovery probes Ollama/llama.cpp metadata too.
		if r.Method == http.MethodGet || r.URL.Path == "/api/show" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer synthetic-hermes-e2e-key" {
			t.Errorf("unexpected fixture route: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected fixture route", http.StatusBadRequest)
			return
		}
		var request struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&request) != nil || request.Model != "salcara-hermes-fixture" {
			t.Errorf("unexpected fixture request model: %q", request.Model)
			http.Error(w, "unexpected fixture model", http.StatusBadRequest)
			return
		}
		calls.Add(1)
		if !request.Stream {
			if hold.Load() {
				<-r.Context().Done()
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"fixture","object":"chat.completion","created":1700000000,"model":"salcara-hermes-fixture","choices":[{"index":0,"message":{"role":"assistant","content":"Synthetic Hermes Gateway reply"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":5,"total_tokens":9}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"salcara-hermes-fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Synthetic Hermes Gateway reply\"},\"finish_reason\":null}]}\n\n")
		if hold.Load() {
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"salcara-hermes-fixture\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":5,\"total_tokens\":9}}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	defer server.CloseClientConnections()
	settings := Settings{Approval: "ask", Extensions: map[string]ExtensionSettings{hermesID: {Root: server.URL, Key: "synthetic-hermes-e2e-key", Model: "salcara-hermes-fixture"}}}
	state := t.TempDir()
	var traceMu sync.Mutex
	var traceFile *os.File
	if tracePath := os.Getenv("SALCARA_HERMES_E2E_TRACE"); tracePath != "" {
		traceFile, err = os.Create(tracePath)
		if err != nil {
			t.Fatal(err)
		}
		defer traceFile.Close()
	}
	trace := func(method string, raw json.RawMessage) {
		if method == "event" {
			var frame hermesNativeEvent
			var result struct {
				Outcome string `json:"outcome"`
			}
			if json.Unmarshal(raw, &frame) == nil && frame.Type == "message.complete" && frame.Generation == 3 &&
				json.Unmarshal(frame.Payload, &result) == nil && (result.Outcome == "cancelled" || result.Outcome == "interrupted") {
				cancelled.Store(true)
			}
		}
		if traceFile != nil {
			traceMu.Lock()
			defer traceMu.Unlock()
			_ = json.NewEncoder(traceFile).Encode(map[string]any{"method": method, "result": raw, "time": time.Now().UTC()})
		}
	}
	newAgent := func() *hermesAgent {
		a := newHermesAgent(func() Settings { return settings }, state)
		a.dial = func(_ context.Context, notify func(string, json.RawMessage)) (hermesGateway, error) {
			cmd := newCommand("-c", hermesSidecar)
			rpc, err := startRPCProtocol(cmd, "2.0", func(method string, raw json.RawMessage) {
				trace(method, raw)
				notify(method, raw)
			}, nil)
			if err != nil {
				return nil, err
			}
			return &hermesOfficialTraceGateway{hermesGateway: &hermesPipe{rpc}, trace: trace}, nil
		}
		return a
	}
	a := newAgent()
	defer func() { a.Close() }()
	id, err := a.Start(ctx, t.TempDir(), "Reply briefly. Do not call tools.", "", "ask")
	if err != nil {
		t.Fatal("official Gateway create/submit:", err)
	}
	waitIdle := func(want int32) {
		t.Helper()
		lastState := ""
		for ctx.Err() == nil {
			info, events, err := a.Open(ctx, id)
			if err != nil {
				t.Fatalf("official Gateway resume: %v; context=%v; provider calls=%d; last state=%s", err, ctx.Err(), calls.Load(), lastState)
			}
			var found int
			var rows []string
			for _, e := range events {
				if e.Role == "assistant" && e.Text == "Synthetic Hermes Gateway reply" {
					found++
					rows = append(rows, fmt.Sprintf("%s:%s final=%t", e.Role, e.ID, e.Final))
				}
			}
			state := fmt.Sprintf("status=%s calls=%d replies=%d wanted=%d", info.Status, calls.Load(), found, want)
			if state != lastState {
				t.Log("official await snapshot:", state)
				lastState = state
			}
			if found > int(want) {
				t.Fatalf("duplicate official reply rows: %s; rows=%v; context=%v", state, rows, ctx.Err())
			}
			if info.Status == "idle" && calls.Load() >= want {
				if calls.Load() != want || found != int(want) {
					t.Fatalf("official output mismatch: calls=%d replies=%d wanted=%d events=%+v", calls.Load(), found, want, events)
				}
				return
			}
			if info.Status == "idle" && info.UpdatedAt > 0 {
				for _, event := range events {
					if event.Role == "assistant" && strings.Contains(event.Text, "This turn did not complete.") {
						t.Fatalf("official runtime rejected fixture before completion: %+v", events)
					}
				}
			}
			if info.Status == "failed" || info.Status == "unknown" {
				t.Fatalf("official turn %s: %+v", info.Status, events)
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("official response wait:", ctx.Err())
	}
	waitIdle(1)
	t.Log("official first reply and snapshot: 1 provider call, 1 assistant message")
	a.Close()
	a = newAgent()
	if err := a.Send(ctx, id, "Reply after reconnecting with the same key. Do not call tools."); err != nil {
		t.Fatal("official persisted same-key resume:", err)
	}
	waitIdle(2)
	t.Log("official same-key persisted resume: 2 provider calls, 2 distinct assistant rows with identical text")
	s, err := a.session(id)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := a.runtime(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Verify the owner itself rejects key replacement, independently of our
	// earlier local frozen-route guard.
	var snapshot hermesSnapshot
	if err := conn.Call(ctx, "session.resume", map[string]string{"session_id": s.NativeID, "api_key": "different-synthetic-key"}, &snapshot); err == nil {
		t.Fatal("official Gateway accepted a replacement launch key")
	} else {
		t.Logf("official different-key resume refused: %v", err)
	}
	updated := settings.Extensions[hermesID]
	updated.Key = "different-synthetic-key"
	settings.Extensions[hermesID] = updated
	if err := a.Send(ctx, id, "Must not reach provider."); err == nil || !strings.Contains(err.Error(), "凭据") {
		t.Fatalf("adapter frozen-key guard: %v", err)
	}
	updated.Key = "synthetic-hermes-e2e-key"
	settings.Extensions[hermesID] = updated
	hold.Store(true)
	if err := a.Send(ctx, id, "Start the interrupt fixture. Do not call tools."); err != nil {
		t.Fatal(err)
	}
	var active protocol.SessionInfo
	for ctx.Err() == nil {
		active, _, err = a.Open(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if active.ActiveTurnID != "" && calls.Load() == 3 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if active.ActiveTurnID == "" {
		t.Fatal("official interrupt target missing:", ctx.Err())
	}
	result, err := a.InterruptTurn(ctx, id, active.ActiveTurnID, "01234567-1234-1234-1234-123456789abc")
	if err != nil || result.Status != "requested" {
		t.Fatalf("official exact interrupt: %+v %v", result, err)
	}
	for ctx.Err() == nil {
		info, _, err := a.Open(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if info.Status == "idle" && info.ActiveTurnID == "" && cancelled.Load() {
			if calls.Load() != 3 {
				t.Fatalf("unexpected extra provider request after interrupt: %d", calls.Load())
			}
			t.Log("official create/reply, persisted same-key resume, different-key refusal and precise interrupt passed (3 synthetic provider calls, final native outcome cancelled/interrupted)")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("official interrupt did not settle:", ctx.Err())
}

type hermesOfficialTraceGateway struct {
	hermesGateway
	trace func(string, json.RawMessage)
}

func (g *hermesOfficialTraceGateway) Call(ctx context.Context, method string, params, out any) error {
	started := time.Now()
	var raw json.RawMessage
	if err := g.hermesGateway.Call(ctx, method, params, &raw); err != nil {
		detail := map[string]string{"error": err.Error(), "elapsed": time.Since(started).String(), "context_error": fmt.Sprint(ctx.Err())}
		if pipe, ok := g.hermesGateway.(*hermesPipe); ok {
			detail["sidecar_stderr"] = pipe.stderrText()
		}
		data, _ := json.Marshal(detail)
		g.trace(method+".error", data)
		return err
	}
	g.trace(method, raw)
	return json.Unmarshal(raw, out)
}
