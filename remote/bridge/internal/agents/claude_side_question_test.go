package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/gateway"
	"salcara/bridge/internal/protocol"
	"salcara/bridge/internal/usageledger"
)

// Every inference endpoint in these tests is an httptest server. No installed
// CLI/SDK, environment credential or paid provider is invoked.
type claudeSideFixture struct {
	a        *claudeAgent
	p        *claudeProc
	cfg      config.Config
	ctx      context.Context
	snapshot SideQuestionSnapshot
	allowed  atomic.Bool
	writes   atomic.Int32
	rec      *recorder
	path     string
	port     int
}

func newClaudeSideFixture(t *testing.T, handler http.Handler) *claudeSideFixture {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	f := newClaudeSideFixtureAt(t, server.URL)
	return f
}

func newClaudeSideFixtureAt(t *testing.T, root string) *claudeSideFixture {
	t.Helper()
	u, _ := url.Parse(root)
	port, _ := strconv.Atoi(u.Port())
	local := config.LocalAccount{ID: "local-a", Kind: "claude", SourceType: "api_key", BaseURL: "https://local.invalid", Key: "synthetic-local-a", Model: "claude-local"}
	remote := config.LocalAccount{ID: "remote-b", Kind: "claude", SourceType: "api_key", BaseURL: "https://remote.invalid", Key: "synthetic-remote-b", Model: "claude-fixture", Models: []string{"claude-fixture"}}
	cfg := config.Config{GatewayRuntimeKey: "synthetic-runtime", RemoteExecutionArmed: true, LocalAccounts: []config.LocalAccount{local, remote}, RemoteAPI: map[string]config.RemoteAPI{"claude": {AccountID: remote.ID, Model: remote.Model}}, GatewayAuthEpochs: map[string]string{"claude": "local-r1", "remote-claude": "remote-r1"}}
	cfg.RecordAppliedAPI("claude", local, "")
	cfg.RemoteStationAuthorization = &config.StationAuthorization{Identity: config.RemoteIdentity(cfg), Revision: "station-r1", Station: protocol.StationPolicy{Version: 1, APIBases: []string{remote.BaseURL}}}
	f := &claudeSideFixture{cfg: cfg, rec: &recorder{}, port: port}
	f.allowed.Store(true)
	f.a = newClaudeAgent(f.rec.sink, func() Settings {
		s := LocalSettingsWithGateway(cfg, port)
		s.LocalPort = port
		return s
	}, "", t.TempDir())
	t.Cleanup(f.a.Close)
	f.ctx = WithConfigSnapshot(context.Background(), cfg)
	id := "11111111-1111-4111-8111-111111111111"
	f.p = &claudeProc{id: id, cwd: t.TempDir(), model: remote.Model, policy: "ask", stopCh: make(chan struct{}), done: make(chan struct{}), workerSettingsSig: claudeSettingsSig(settingsForContext(f.ctx, f.a.settings), "")}
	f.p.stdin = &claudeDeliveryFixtureWriter{write: func(data []byte) (int, error) { f.writes.Add(1); return len(data), nil }}
	f.a.procs[id] = f.p
	f.a.live[id] = protocol.SessionInfo{SessionKey: "claude:" + id, Tool: "claude", Cwd: f.p.cwd, Title: "Parent", Status: "idle", Controllable: true}
	f.path = filepath.Join(f.p.cwd, "parent-transcript.jsonl")
	if err := os.WriteFile(f.path, []byte("HIDDEN_NATIVE_HISTORY_SENTINEL\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.snapshot = SideQuestionSnapshot{Revision: "reading-1", Truncated: true, Messages: []SideQuestionMessage{{ID: "u1", Role: "user", Text: "Loaded user context"}, {ID: "a1", Role: "assistant", Text: "Loaded assistant context"}}}
	return f
}

func (f *claudeSideFixture) guard() error {
	if !f.allowed.Load() {
		return errSideQuestionStale
	}
	return nil
}

func (f *claudeSideFixture) catalog(t *testing.T) protocol.SideQuestionCatalog {
	t.Helper()
	view, err := f.a.SideQuestionCatalog(f.ctx, f.p.id, f.snapshot, f.guard)
	if err != nil || !view.Available || view.Revision == "" || view.ContextScope != "loadedSnapshot" || view.ContextMessages != 2 || !view.ContextTruncated {
		t.Fatalf("catalog: %+v %v", view, err)
	}
	return view
}

func sideFixtureRequest(view protocol.SideQuestionCatalog) protocol.SideQuestionRequest {
	return protocol.SideQuestionRequest{Question: "What does this mean?", OperationID: newUUID(), ExpectedRevision: view.Revision}
}

func writeSideAnswer(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "message", "role": "assistant", "stop_reason": "end_turn", "content": []any{map[string]string{"type": "text", "text": text}}, "usage": map[string]int{"input_tokens": 11, "output_tokens": 3, "cache_read_input_tokens": 2, "cache_creation_input_tokens": 1}})
}

func TestClaudeSideQuestionLoadedSnapshotAndConfirmedFollowupNeverTouchParent(t *testing.T) {
	var calls atomic.Int32
	bodies := make(chan map[string]json.RawMessage, 4)
	f := newClaudeSideFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gateway/remote-claude/v1/messages" || r.Header.Get("anthropic-version") != "2023-06-01" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer rt-salcara-route-") {
			t.Error("wrong route/credential")
		}
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid JSON")
		}
		bodies <- body
		writeSideAnswer(w, fmt.Sprintf("answer-%d", calls.Add(1)))
	}))
	f.p.turns = 1 // Side questions are allowed during the parent's active work.
	view := f.catalog(t)
	request := sideFixtureRequest(view)
	start, end := int64(0), int64(5)
	request.Selection = &protocol.ContextReference{Kind: "message", SessionKey: view.SessionKey, MessageID: "older-loaded-message", Label: "Quoted selection", Text: "quote", StartOffset: &start, EndOffset: &end, CapturedAt: 1}
	result, err := f.a.AskSideQuestion(f.ctx, f.p.id, f.snapshot, request, f.guard)
	if err != nil || result.State != "completed" || result.Answer != "answer-1" || result.Usage == nil || result.Usage.InputTokens != 14 || result.ContextScope != "loadedSnapshot" || result.ContextMessages != 2 || !result.ContextTruncated {
		t.Fatalf("answer: %+v %v", result, err)
	}
	body := <-bodies
	if string(body["tool_choice"]) != `{"type":"none"}` || body["tools"] != nil || body["resume"] != nil || body["session_id"] != nil || string(body["stream"]) != "false" || string(body["model"]) != `"claude-fixture"` || string(body["max_tokens"]) != "2048" {
		t.Fatalf("unsafe request: %v", body)
	}
	raw, _ := json.Marshal(body)
	if !strings.Contains(string(raw), "Loaded user context") || !strings.Contains(string(raw), "Loaded assistant context") || !strings.Contains(string(raw), "older-loaded-message") || strings.Contains(string(raw), "HIDDEN_NATIVE_HISTORY_SENTINEL") {
		t.Fatal("wrong context source")
	}
	result.Usage.InputTokens = 999
	result.Answer = "client-mutated-answer"
	replay, err := f.a.AskSideQuestion(f.ctx, f.p.id, f.snapshot, request, f.guard)
	if err != nil || replay.Answer != "answer-1" || replay.Usage.InputTokens != 14 || calls.Load() != 1 {
		t.Fatal("receipt aliased or resent")
	}
	followup := protocol.SideQuestionRequest{Question: "Why?", PreviousOperationID: request.OperationID, OperationID: newUUID(), ExpectedRevision: view.Revision}
	second, err := f.a.AskSideQuestion(f.ctx, f.p.id, f.snapshot, followup, f.guard)
	if err != nil || second.Answer != "answer-2" {
		t.Fatalf("followup: %+v %v", second, err)
	}
	var messages []sideQuestionWireMessage
	if json.Unmarshal((<-bodies)["messages"], &messages) != nil || len(messages) != 3 || messages[1].Role != "assistant" || messages[1].Content[0].Text != "answer-1" {
		t.Fatalf("followup did not use server answer: %+v", messages)
	}
	if strings.Contains(fmt.Sprint(messages), "client-mutated-answer") {
		t.Fatal("caller supplied assistant answer")
	}
	changed := request
	changed.Question = "different payload"
	if _, err = f.a.AskSideQuestion(f.ctx, f.p.id, f.snapshot, changed, f.guard); !errors.Is(err, errSideQuestionStale) {
		t.Fatal("operation accepted a different question")
	}
	fork := followup
	fork.OperationID = newUUID()
	if _, err = f.a.AskSideQuestion(f.ctx, f.p.id, f.snapshot, fork, f.guard); !errors.Is(err, errSideQuestionStale) {
		t.Fatal("predecessor accepted a second next question")
	}
	if calls.Load() != 2 || f.writes.Load() != 0 || f.rec.count(func(protocol.Event) bool { return true }) != 0 || f.p.turns != 1 {
		t.Fatal("side question mutated main worker/history/events")
	}
	data, _ := os.ReadFile(f.path)
	if string(data) != "HIDDEN_NATIVE_HISTORY_SENTINEL\n" {
		t.Fatal("parent transcript changed")
	}
}

func TestClaudeSideQuestionPendingBlocksSwitchButAllowsMainSend(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	f := newClaudeSideFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-release
		writeSideAnswer(w, "done")
	}))
	request := sideFixtureRequest(f.catalog(t))
	done := make(chan protocol.SideQuestionResult, 1)
	go func() { result, _ := f.a.AskSideQuestion(f.ctx, f.p.id, f.snapshot, request, f.guard); done <- result }()
	<-entered
	if !f.a.ActiveRemoteTurns() {
		t.Error("API switch did not see pending side request")
	}
	duplicate, err := f.a.AskSideQuestion(f.ctx, f.p.id, f.snapshot, request, f.guard)
	if err != nil || duplicate.State != "unconfirmed" {
		t.Errorf("pending receipt: %+v %v", duplicate, err)
	}
	other := request
	other.OperationID = newUUID()
	if _, err = f.a.AskSideQuestion(f.ctx, f.p.id, f.snapshot, other, f.guard); !errors.Is(err, errSideQuestionBusy) {
		t.Error("parallel root admitted")
	}
	if err = f.a.Send(f.ctx, f.p.id, "Continue the main task"); err != nil {
		t.Errorf("side request blocked main Send: %v", err)
	}
	close(release)
	if result := <-done; result.State != "completed" {
		t.Errorf("side result: %+v", result)
	}
	if f.writes.Load() != 1 || f.a.sideQuestions.activeTurns() {
		t.Fatal("main write missing or side pending not released")
	}
}

func TestClaudeSideQuestionUnconfirmedNeverRetriesAndRedactsResponses(t *testing.T) {
	var redirected atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer other.Close()
	for _, kind := range []string{"redirect", "error", "invalid-json", "tool", "oversized-response", "oversized-answer", "bad-usage", "truncated-answer"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			f := newClaudeSideFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				switch kind {
				case "redirect":
					w.Header().Set("Location", other.URL)
					w.WriteHeader(http.StatusTemporaryRedirect)
				case "error":
					http.Error(w, "raw private prompt and synthetic-secret https://upstream.invalid", 502)
				case "invalid-json":
					_, _ = io.WriteString(w, "synthetic-secret-invalid-json")
				case "tool":
					_, _ = io.WriteString(w, `{"type":"message","role":"assistant","stop_reason":"end_turn","content":[{"type":"tool_use","name":"Bash","input":{"command":"must not run"}}]}`)
				case "oversized-response":
					_, _ = io.WriteString(w, strings.Repeat("x", maxSideQuestionResponse+1))
				case "oversized-answer":
					writeSideAnswer(w, strings.Repeat("x", maxSideQuestionAnswer+1))
				case "bad-usage":
					_, _ = io.WriteString(w, `{"type":"message","role":"assistant","stop_reason":"end_turn","content":[{"type":"text","text":"answer"}],"usage":{"input_tokens":-1}}`)
				case "truncated-answer":
					_, _ = io.WriteString(w, `{"type":"message","role":"assistant","stop_reason":"max_tokens","content":[{"type":"text","text":"partial"}]}`)
				}
			}))
			request := sideFixtureRequest(f.catalog(t))
			result, err := f.a.AskSideQuestion(f.ctx, f.p.id, f.snapshot, request, f.guard)
			if err != nil || result.State != "unconfirmed" || result.Answer != "" || result.Usage != nil || strings.Contains(result.Error, "synthetic") || strings.Contains(result.Error, "https:") {
				t.Fatalf("unconfirmed: %+v %v", result, err)
			}
			if replay, err := f.a.AskSideQuestion(f.ctx, f.p.id, f.snapshot, request, f.guard); err != nil || replay.Error != result.Error {
				t.Fatal("unknown receipt lost")
			}
			request.OperationID = newUUID()
			if _, err = f.a.AskSideQuestion(f.ctx, f.p.id, f.snapshot, request, f.guard); !errors.Is(err, errSideQuestionUnknown) {
				t.Fatal("new operation bypassed unknown fence")
			}
			view, err := f.a.SideQuestionCatalog(f.ctx, f.p.id, f.snapshot, f.guard)
			if err != nil || view.Available || calls.Load() != 1 || f.a.sideQuestions.activeTurns() {
				t.Fatal("unknown request was retried or remained pending")
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatal("redirect leaked prompt or credential")
	}
}

func TestClaudeSideQuestionFinalSourceAndWorkerFences(t *testing.T) {
	for _, kind := range []string{"revoked", "worker-replaced", "closed"} {
		t.Run(kind, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			f := newClaudeSideFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(entered)
				<-release
				writeSideAnswer(w, "must not publish")
			}))
			request := sideFixtureRequest(f.catalog(t))
			done := make(chan protocol.SideQuestionResult, 1)
			go func() { result, _ := f.a.AskSideQuestion(f.ctx, f.p.id, f.snapshot, request, f.guard); done <- result }()
			<-entered
			switch kind {
			case "revoked":
				f.allowed.Store(false)
			case "worker-replaced":
				f.a.mu.Lock()
				f.a.procs[f.p.id] = &claudeProc{id: f.p.id, workerSettingsSig: f.p.workerSettingsSig}
				f.a.mu.Unlock()
			case "closed":
				f.a.Close()
			}
			close(release)
			result := <-done
			if result.State != "unconfirmed" || result.Answer != "" || f.a.sideQuestions.activeTurns() {
				t.Fatalf("stale response published: %+v", result)
			}
		})
	}
}

func TestClaudeSideQuestionCatalogRejectsAmbientUnmanagedPoolAndEmptyModel(t *testing.T) {
	var calls atomic.Int32
	f := newClaudeSideFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); writeSideAnswer(w, "answer") }))
	t.Setenv("ANTHROPIC_API_KEY", "ambient-must-not-be-used")
	for _, kind := range []string{"no-snapshot", "no-runtime", "no-guard", "empty-model", "local-pool"} {
		t.Run(kind, func(t *testing.T) {
			cfg := f.cfg.Clone()
			ctx, guard := f.ctx, f.guard
			switch kind {
			case "no-snapshot":
				ctx = context.Background()
			case "no-runtime":
				cfg.GatewayRuntimeKey = ""
				ctx = WithConfigSnapshot(context.Background(), cfg)
			case "no-guard":
				guard = nil
			case "empty-model":
				cfg.RemoteAPI["claude"] = config.RemoteAPI{AccountID: "remote-b"}
				ctx = WithConfigSnapshot(context.Background(), cfg)
			case "local-pool":
				cfg.LocalAccounts[0].PoolEnabled = true
				cfg.RecordAppliedAPI("claude", cfg.LocalAccounts[0], "")
				ctx = WithLocalConfigSnapshot(context.Background(), cfg)
			}
			view, err := f.a.SideQuestionCatalog(ctx, f.p.id, f.snapshot, guard)
			if err != nil || view.Available || view.UnavailableReason == "" {
				t.Fatalf("unsafe availability: %+v %v", view, err)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("catalog performed inference")
	}
}

func TestClaudeSideQuestionSourceABAAndSnapshotMutationRejectBeforeHTTP(t *testing.T) {
	var calls atomic.Int32
	f := newClaudeSideFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); writeSideAnswer(w, "answer") }))
	request := sideFixtureRequest(f.catalog(t))
	cfg := f.cfg.Clone()
	cfg.GatewayAuthEpochs["remote-claude"] = "remote-r3-restored"
	if _, err := f.a.AskSideQuestion(WithConfigSnapshot(context.Background(), cfg), f.p.id, f.snapshot, request, f.guard); err == nil {
		t.Fatal("restored source revived old reading")
	}
	changed := f.snapshot
	changed.Messages = append([]SideQuestionMessage(nil), changed.Messages...)
	changed.Messages[0].Text = "caller supplied replacement"
	if _, err := f.a.AskSideQuestion(f.ctx, f.p.id, changed, request, f.guard); !errors.Is(err, errSideQuestionStale) {
		t.Fatal("snapshot body accepted under old revision")
	}
	f.allowed.Store(false)
	if _, err := f.a.AskSideQuestion(f.ctx, f.p.id, f.snapshot, request, f.guard); !errors.Is(err, errSideQuestionStale) {
		t.Fatal("revoked guard ignored")
	}
	if calls.Load() != 0 {
		t.Fatal("rejected request reached HTTP")
	}
}

func TestClaudeSideQuestionTimeoutReturnsUnknownReceiptWithoutRetry(t *testing.T) {
	var calls atomic.Int32
	f := newClaudeSideFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	request := sideFixtureRequest(f.catalog(t))
	ctx, cancel := context.WithTimeout(f.ctx, 150*time.Millisecond)
	defer cancel()
	result, err := f.a.AskSideQuestion(ctx, f.p.id, f.snapshot, request, f.guard)
	if err != nil || result.State != "unconfirmed" || calls.Load() != 1 || f.a.sideQuestions.activeTurns() {
		t.Fatalf("timeout: %+v %v", result, err)
	}
	if replay, err := f.a.AskSideQuestion(f.ctx, f.p.id, f.snapshot, request, f.guard); err != nil || replay.State != "unconfirmed" || calls.Load() != 1 {
		t.Fatal("timeout retried")
	}
}

func TestClaudeSideQuestionUsesExistingGatewayUsageObservation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "synthetic-upstream" {
			t.Error("wrong upstream identity")
		}
		writeSideAnswer(w, "through-gateway")
	}))
	defer upstream.Close()
	var cfg config.Config
	observed := make(chan usageledger.Observation, 1)
	usageStarted, usageEnded := atomic.Int32{}, atomic.Int32{}
	h := gateway.New(func() string { return "unused" }, func(target string) (config.LocalAccount, bool) { return cfg.GatewaySourceAccount(target) })
	h.KeyForTarget = func(target string) string { return cfg.RemoteGatewayCredential(target) }
	h.AuthorizationRevision = func(target string) string { return cfg.GatewayAuthorizationRevision(target) }
	h.Observe = func(observation usageledger.Observation) { observed <- observation }
	h.BeginUsage = func(account string, epoch uint64) func() {
		if account != "remote-b" {
			t.Error("usage charged to different account")
		}
		usageStarted.Add(1)
		return func() { usageEnded.Add(1) }
	}
	loopback := httptest.NewServer(h)
	defer loopback.Close()
	f := newClaudeSideFixtureAt(t, loopback.URL)
	cfg = f.cfg.Clone()
	cfg.LocalAccounts[1].BaseURL, cfg.LocalAccounts[1].Key, cfg.LocalAccounts[1].AuthMode = upstream.URL, "synthetic-upstream", "api-key"
	cfg.RemoteStationAuthorization.Station.APIBases = []string{upstream.URL}
	f.ctx = WithConfigSnapshot(context.Background(), cfg)
	f.p.workerSettingsSig = claudeSettingsSig(settingsForContext(f.ctx, f.a.settings), "")
	request := sideFixtureRequest(f.catalog(t))
	result, err := f.a.AskSideQuestion(f.ctx, f.p.id, f.snapshot, request, f.guard)
	if err != nil || result.State != "completed" || result.Answer != "through-gateway" {
		t.Fatalf("gateway result: %+v %v", result, err)
	}
	observation := <-observed
	if observation.AccountID != "remote-b" || observation.Route != "remote-claude/v1/messages" || !observation.UsageFinal || usageStarted.Load() != 1 || usageEnded.Load() != 1 {
		t.Fatalf("accounting bypassed: %+v", observation)
	}
}
