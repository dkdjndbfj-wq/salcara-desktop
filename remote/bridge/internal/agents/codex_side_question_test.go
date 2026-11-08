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
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/codexruntime"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/gateway"
	"salcara/bridge/internal/protocol"
	"salcara/bridge/internal/usageledger"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const codexSideAnswer = `{"id":"resp-side-fixture","object":"response","status":"completed","output":[{"type":"reasoning","summary":[]},{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Independent fixture answer"}]}],"usage":{"input_tokens":13,"output_tokens":7,"total_tokens":20,"input_tokens_details":{"cached_tokens":4}}}`

type codexSideFixture struct {
	a        *codexAgent
	native   *codexSessionActionTransport
	cfg      config.Config
	ctx      context.Context
	snapshot SideQuestionSnapshot
	port     int
	validate func() error
}

func newCodexSideFixture(t *testing.T, remote bool, cfg config.Config, listener string) *codexSideFixture {
	t.Helper()
	endpoint, err := url.Parse(listener)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(endpoint.Port())
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.LocalAccounts) == 0 {
		cfg.LocalAccounts = []config.LocalAccount{{ID: "side-account", Kind: "codex", SourceType: "api_key", BaseURL: "https://synthetic.invalid", Key: "synthetic-upstream", Model: "side-model", Models: []string{"side-model"}, Protocol: "responses"}}
	}
	cfg.GatewayRuntimeKey = "synthetic-side-runtime"
	cfg.RecordAppliedAPI("codex", cfg.LocalAccounts[0], "")
	cfg.GatewayAuthEpochs = map[string]string{"codex": "side-local-epoch", "remote-codex": "side-remote-epoch"}
	if remote {
		cfg.RemoteExecutionArmed = true
		cfg.RemoteAPI = map[string]config.RemoteAPI{"codex": {AccountID: cfg.LocalAccounts[0].ID, Model: cfg.LocalAccounts[0].Model}}
		cfg.RemoteStationAuthorization = &config.StationAuthorization{Identity: config.RemoteIdentity(cfg), Revision: "fixture-station", Station: protocol.StationPolicy{Version: 1, APIBases: []string{cfg.LocalAccounts[0].BaseURL}}}
	}
	a, native, _ := newCodexSessionActionFixture(t)
	f := &codexSideFixture{a: a, native: native, cfg: cfg, port: port, validate: func() error { return nil },
		snapshot: SideQuestionSnapshot{Revision: "loaded-snapshot-1", Truncated: true, Messages: []SideQuestionMessage{{ID: "parent-user", Role: "user", Text: "Original parent question"}, {ID: "parent-answer", Role: "assistant", Text: "Original parent explanation"}}}}
	f.installConfig(remote, cfg)
	return f
}

func (f *codexSideFixture) installConfig(remote bool, cfg config.Config) {
	f.cfg = cfg
	settings := LocalSettingsWithGateway(cfg, f.port)
	f.ctx = WithLocalConfigSnapshot(context.Background(), cfg)
	if remote {
		settings = SettingsWithGateway(cfg, f.port)
		f.ctx = WithConfigSnapshot(context.Background(), cfg)
	}
	settings.LocalPort = f.port
	f.a.settings = func() Settings { return settings }
	f.a.mu.Lock()
	f.a.connSig = codexSig(settings)
	f.a.mu.Unlock()
}

func (f *codexSideFixture) request(t *testing.T, n int) protocol.SideQuestionRequest {
	t.Helper()
	view, err := f.a.SideQuestionCatalog(f.ctx, sessionActionThread, f.snapshot, f.validate)
	if err != nil || !view.Available || view.ContextScope != "loadedSnapshot" || !view.ContextTruncated || view.ContextMessages != 2 {
		t.Fatalf("side catalog unavailable: %+v %v", view, err)
	}
	return protocol.SideQuestionRequest{Question: "Explain this independently", OperationID: fmt.Sprintf("65000000-0000-4000-8000-%012x", n), ExpectedRevision: view.Revision}
}

func assertCodexSideNoNativeMutation(t *testing.T, f *codexSideFixture, before protocol.SessionInfo) {
	t.Helper()
	f.native.mu.Lock()
	count := len(f.native.requests)
	f.native.mu.Unlock()
	f.a.mu.Lock()
	after := f.a.threads[sessionActionThread].info
	f.a.mu.Unlock()
	if count != 0 || !reflect.DeepEqual(before, after) {
		t.Fatalf("side question touched native parent: calls=%d before=%+v after=%+v", count, before, after)
	}
}

func TestCodexSideQuestionExactGatewayChainAndLedger(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(fmt.Sprint("remote=", remote), func(t *testing.T) {
			var calls atomic.Int32
			inputs := make(chan []map[string]any, 2)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer synthetic-upstream" || body["model"] != "side-model" || body["tool_choice"] != "none" || body["store"] != false || body["stream"] != false || body["max_output_tokens"] != float64(2048) {
					t.Errorf("wrong upstream route or constraints: path=%s body=%+v", r.URL.Path, body)
				}
				tools, ok := body["tools"].([]any)
				if !ok || len(tools) != 0 {
					t.Error("side question exposed tools")
				}
				for _, field := range []string{"conversation", "previous_response_id", "metadata", "session_id", "background"} {
					if _, ok := body[field]; ok {
						t.Errorf("unexpected persistent/native field %s", field)
					}
				}
				var messages []map[string]any
				for _, message := range body["input"].([]any) {
					messages = append(messages, message.(map[string]any))
				}
				inputs <- messages
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, codexSideAnswer)
			}))
			defer upstream.Close()
			var f *codexSideFixture
			h := gateway.New(nil, func(target string) (config.LocalAccount, bool) { return f.cfg.GatewaySourceAccount(target) })
			h.KeyForTarget = func(target string) string {
				if strings.HasPrefix(target, "remote-") {
					return f.cfg.RemoteGatewayCredential(target)
				}
				return f.cfg.LocalGatewayCredential(target)
			}
			h.AuthorizationRevision = func(target string) string { return f.cfg.GatewayAuthorizationRevision(target) }
			observations := make(chan usageledger.Observation, 2)
			h.Observe = func(o usageledger.Observation) { observations <- o }
			listener := httptest.NewServer(h)
			defer listener.Close()
			cfg := config.Config{LocalAccounts: []config.LocalAccount{{ID: "side-account", Kind: "codex", SourceType: "api_key", BaseURL: upstream.URL, Key: "synthetic-upstream", Model: "side-model", Models: []string{"side-model"}, Protocol: "responses"}}}
			f = newCodexSideFixture(t, remote, cfg, listener.URL)
			before := f.a.threads[sessionActionThread].info
			req := f.request(t, 1)
			start, end := int64(0), int64(8)
			req.Selection = &protocol.ContextReference{Kind: "message", SessionKey: "codex:" + sessionActionThread, MessageID: "parent-answer", Label: "Selection", Text: "Original", StartOffset: &start, EndOffset: &end, CapturedAt: 1}
			result, err := f.a.AskSideQuestion(f.ctx, sessionActionThread, f.snapshot, req, f.validate)
			if err != nil || result.State != "completed" || result.Answer != "Independent fixture answer" || result.Usage == nil || result.Usage.InputTokens != 13 || result.Usage.OutputTokens != 7 {
				t.Fatalf("side answer failed: %+v %v", result, err)
			}
			first := <-inputs
			if len(first) != 1 || first[0]["role"] != "user" || !strings.Contains(first[0]["content"].(string), "Original parent explanation") || !strings.Contains(first[0]["content"].(string), "truncated") {
				t.Fatal("parent context was not a quoted bounded user message")
			}
			result.Answer, result.Usage.InputTokens = "caller mutation", 999
			retry, err := f.a.AskSideQuestion(f.ctx, sessionActionThread, f.snapshot, req, f.validate)
			if err != nil || retry.Answer != "Independent fixture answer" || retry.Usage.InputTokens != 13 || calls.Load() != 1 {
				t.Fatalf("cached operation replayed or aliased: %+v %v", retry, err)
			}
			second := f.request(t, 2)
			second.PreviousOperationID, second.Question = req.OperationID, "Explain the first answer further"
			if result, err := f.a.AskSideQuestion(f.ctx, sessionActionThread, f.snapshot, second, f.validate); err != nil || result.State != "completed" {
				t.Fatalf("followup failed: %+v %v", result, err)
			}
			chain := <-inputs
			if len(chain) != 3 || chain[1]["role"] != "assistant" || chain[1]["content"] != "Independent fixture answer" || chain[2]["role"] != "user" || strings.Contains(chain[2]["content"].(string), "Original parent explanation") {
				t.Fatal("followup did not retain exactly the server-confirmed exchange")
			}
			for n := 0; n < 2; n++ {
				select {
				case o := <-observations:
					if o.AccountID != "side-account" || o.State != "completed" || !o.UsageFinal || o.Tokens.Input == nil || *o.Tokens.Input != 13 || o.Tokens.Output == nil || *o.Tokens.Output != 7 {
						t.Fatalf("side request bypassed ordinary accounting: %+v", o)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("missing gateway usage observation")
				}
			}
			assertCodexSideNoNativeMutation(t, f, before)
		})
	}
}

func TestCodexSideQuestionProtectedOfficialGateway(t *testing.T) {
	var f *codexSideFixture
	var calls atomic.Int32
	var providerCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		tools, ok := body["tools"].([]any)
		if !ok || len(tools) != 0 || body["tool_choice"] != "none" || body["store"] != false || r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer synthetic-protected" {
			t.Errorf("configured pinned SDK changed no-tools constraints: %+v", body)
		}
		for _, field := range []string{"max_output_tokens", "previous_response_id", "conversation"} {
			if _, exists := body[field]; exists {
				t.Errorf("official request retained unsupported/persistent field %s", field)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":"+codexSideAnswer+"}\n\n")
	}))
	defer provider.Close()
	runtime := codexruntime.New(t.TempDir())
	defer runtime.Close()
	h := gateway.New(nil, func(target string) (config.LocalAccount, bool) { return f.cfg.GatewaySourceAccount(target) })
	h.KeyForTarget = func(target string) string { return f.cfg.LocalGatewayCredential(target) }
	h.AuthorizationRevision = func(target string) string { return f.cfg.GatewayAuthorizationRevision(target) }
	h.Official = func(ctx context.Context, account config.LocalAccount, body []byte, stream, compact bool, valid func() bool) (*http.Response, error) {
		calls.Add(1)
		if account.ID != "official-side" || account.CredentialRef != "protected-fixture" || account.Key != "" || stream || compact || !valid() || !strings.Contains(string(body), `"tools":[]`) || !strings.Contains(string(body), `"store":false`) {
			t.Error("official gateway lost the protected exact-account stateless contract")
		}
		auth := &coreauth.Auth{ID: "official-side", Provider: "codex", Attributes: map[string]string{"base_url": provider.URL}, Metadata: map[string]any{"access_token": "synthetic-protected", "plan_type": "plus"}}
		return runtime.Execute(ctx, auth, body, stream, compact)
	}
	listener := httptest.NewServer(h)
	defer listener.Close()
	cfg := config.Config{LocalAccounts: []config.LocalAccount{{ID: "official-side", Kind: "codex", SourceType: "official_account", CredentialRef: "protected-fixture", CredentialEpoch: 1, BaseURL: "https://chatgpt.com/backend-api/codex", Model: "gpt-side-fixture", Protocol: "responses"}}}
	f = newCodexSideFixture(t, false, cfg, listener.URL)
	req := f.request(t, 1)
	result, err := f.a.AskSideQuestion(f.ctx, sessionActionThread, f.snapshot, req, f.validate)
	if err != nil || result.State != "completed" || calls.Load() != 1 || providerCalls.Load() != 1 {
		t.Fatalf("official source unavailable: %+v %v calls=%d providerCalls=%d", result, err, calls.Load(), providerCalls.Load())
	}
}

func TestCodexSideQuestionMixedProtocolGatewayPreservesNoToolsAndContext(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.URL.Path != "/v1/chat/completions" || body["model"] != "side-model" || body["tool_choice"] != nil && body["tool_choice"] != "none" {
			t.Errorf("wrong translated request: %+v", body)
		}
		if tools, ok := body["tools"].([]any); ok && len(tools) != 0 {
			t.Error("translation injected tools")
		}
		encoded, _ := json.Marshal(body["messages"])
		if !strings.Contains(string(encoded), "Original parent explanation") || !strings.Contains(string(encoded), "Explain this independently") {
			t.Error("translated input lost side-question context")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-side-fixture","object":"chat.completion","model":"side-model","choices":[{"index":0,"message":{"role":"assistant","content":"Independent fixture answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":13,"completion_tokens":7,"total_tokens":20}}`)
	}))
	defer upstream.Close()
	var f *codexSideFixture
	h := gateway.New(nil, func(target string) (config.LocalAccount, bool) { return f.cfg.GatewaySourceAccount(target) })
	h.KeyForTarget = func(target string) string { return f.cfg.LocalGatewayCredential(target) }
	h.AuthorizationRevision = func(target string) string { return f.cfg.GatewayAuthorizationRevision(target) }
	listener := httptest.NewServer(h)
	defer listener.Close()
	cfg := config.Config{LocalAccounts: []config.LocalAccount{{ID: "side-account", Kind: "codex", SourceType: "api_key", BaseURL: upstream.URL, Key: "synthetic-upstream", Model: "side-model", Protocol: "chat"}}}
	f = newCodexSideFixture(t, false, cfg, listener.URL)
	req := f.request(t, 1)
	result, err := f.a.AskSideQuestion(f.ctx, sessionActionThread, f.snapshot, req, f.validate)
	if err != nil || result.State != "completed" || result.Answer != "Independent fixture answer" || calls.Load() != 1 {
		t.Fatalf("mixed protocol did not retain side-question contract: %+v %v calls=%d", result, err, calls.Load())
	}
}

func TestCodexSideQuestionCloseCancelsWithoutParentTurn(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer listener.Close()
	defer close(release)
	f := newCodexSideFixture(t, false, config.Config{}, listener.URL)
	req := f.request(t, 1)
	done := make(chan protocol.SideQuestionResult, 1)
	go func() {
		result, _ := f.a.AskSideQuestion(f.ctx, sessionActionThread, f.snapshot, req, f.validate)
		done <- result
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("side question did not reach loopback fixture")
	}
	f.a.mu.Lock()
	f.a.conn = nil // this fixture has no OS process to terminate
	f.a.mu.Unlock()
	f.a.Close()
	select {
	case result := <-done:
		if result.State != "unconfirmed" || result.Answer != "" || result.Usage != nil {
			t.Fatalf("closed side request exposed success: %+v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("side request ignored Bridge shutdown")
	}
	if f.a.sideQuestions.activeTurns() || len(f.native.requests) != 0 {
		t.Fatal("closed side request retained activity or touched parent")
	}
}

func TestCodexSideQuestionAnthropicGatewayKeepsTextOnlyContract(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.URL.Path != "/v1/messages" || body["model"] != "side-model" || body["stream"] != true {
			t.Errorf("wrong Messages translation: %+v", body)
		}
		if tools, ok := body["tools"].([]any); ok && len(tools) != 0 {
			t.Error("translation injected tools")
		}
		if choice, ok := body["tool_choice"].(map[string]any); ok && choice["type"] != "none" {
			t.Error("translation enabled a tool choice")
		}
		encoded, _ := json.Marshal(body["messages"])
		if !strings.Contains(string(encoded), "Original parent explanation") || !strings.Contains(string(encoded), "Explain this independently") {
			t.Error("Messages translation lost side context")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-side\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"side-model\",\"content\":[],\"usage\":{\"input_tokens\":13,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Independent fixture answer\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":7}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()
	var f *codexSideFixture
	h := gateway.New(nil, func(target string) (config.LocalAccount, bool) { return f.cfg.GatewaySourceAccount(target) })
	h.KeyForTarget = func(target string) string { return f.cfg.LocalGatewayCredential(target) }
	h.AuthorizationRevision = func(target string) string { return f.cfg.GatewayAuthorizationRevision(target) }
	listener := httptest.NewServer(h)
	defer listener.Close()
	cfg := config.Config{LocalAccounts: []config.LocalAccount{{ID: "side-account", Kind: "codex", SourceType: "api_key", BaseURL: upstream.URL, Key: "synthetic-upstream", Model: "side-model", Protocol: "anthropic"}}}
	f = newCodexSideFixture(t, false, cfg, listener.URL)
	req := f.request(t, 1)
	result, err := f.a.AskSideQuestion(f.ctx, sessionActionThread, f.snapshot, req, f.validate)
	if err != nil || result.State != "completed" || result.Answer != "Independent fixture answer" || calls.Load() != 1 {
		t.Fatalf("Messages gateway contract failed: %+v %v calls=%d", result, err, calls.Load())
	}
}

func TestCodexSideQuestionCatalogSourceModelAndRuntimeProof(t *testing.T) {
	for _, kind := range []string{"no-snapshot", "no-validator", "no-runtime-key", "no-epoch", "no-model", "retired-model", "denied-model", "allowlist", "local-pool", "source-changed", "connection-dead", "closed", "remote-unarmed", "remote-no-station"} {
		t.Run(kind, func(t *testing.T) {
			remote := strings.HasPrefix(kind, "remote-")
			f := newCodexSideFixture(t, remote, config.Config{}, "http://127.0.0.1:12345")
			cfg := f.cfg.Clone()
			switch kind {
			case "no-runtime-key":
				cfg.GatewayRuntimeKey = ""
			case "no-epoch":
				cfg.GatewayAuthEpochs = nil
			case "no-model":
				cfg.LocalAccounts[0].Model = ""
				cfg.RecordAppliedAPI("codex", cfg.LocalAccounts[0], "")
			case "retired-model":
				cfg.LocalAccounts[0].Models = []string{"different-model"}
				cfg.RecordAppliedAPI("codex", cfg.LocalAccounts[0], "")
			case "denied-model":
				cfg.LocalAccounts[0].ModelDenylist = []string{"side-model"}
				cfg.RecordAppliedAPI("codex", cfg.LocalAccounts[0], "")
			case "allowlist":
				cfg.LocalAccounts[0].ModelAllowlist = []string{"different-model"}
				cfg.RecordAppliedAPI("codex", cfg.LocalAccounts[0], "")
			case "local-pool":
				cfg.LocalAccounts[0].PoolEnabled = true
				cfg.RecordAppliedAPI("codex", cfg.LocalAccounts[0], "")
			case "remote-unarmed":
				cfg.RemoteExecutionArmed = false
			case "remote-no-station":
				cfg.RemoteStationAuthorization = nil
			}
			f.installConfig(remote, cfg)
			switch kind {
			case "no-snapshot":
				f.ctx = context.Background()
			case "no-validator":
				f.validate = nil
			case "source-changed":
				f.a.connSig = "old-source"
			case "connection-dead":
				f.a.conn.closed = true
			case "closed":
				f.a.closed = true
				defer func() { f.a.closed = false }()
			}
			view, err := f.a.SideQuestionCatalog(f.ctx, sessionActionThread, f.snapshot, f.validate)
			if err != nil || view.Available || view.UnavailableReason == "" {
				t.Fatalf("unsafe route advertised: %+v %v", view, err)
			}
			if len(f.native.requests) != 0 {
				t.Fatal("capability read touched native worker")
			}
		})
	}
}

func TestCodexSideQuestionCatalogFencesSourceEpochAndRuntimeABA(t *testing.T) {
	for _, change := range []string{"authorization-epoch", "runtime"} {
		t.Run(change, func(t *testing.T) {
			var calls atomic.Int32
			listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = io.WriteString(w, codexSideAnswer) }))
			defer listener.Close()
			f := newCodexSideFixture(t, false, config.Config{}, listener.URL)
			req := f.request(t, 1)
			if change == "authorization-epoch" {
				cfg := f.cfg.Clone()
				cfg.GatewayAuthEpochs["codex"] = "new-epoch-after-restoring-source"
				f.installConfig(false, cfg)
			} else {
				f.a.conn = &rpcConn{pending: map[int64]chan rpcResult{}, done: make(chan struct{})}
			}
			result, err := f.a.AskSideQuestion(f.ctx, sessionActionThread, f.snapshot, req, f.validate)
			if !errors.Is(err, errSideQuestionStale) || result.Answer != "" || calls.Load() != 0 {
				t.Fatalf("stale catalog sent: %+v %v calls=%d", result, err, calls.Load())
			}
		})
	}
}

func TestCodexSideQuestionDoesNotTakeParentTurnLockAndBlocksOnlyAPISwitch(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, codexSideAnswer)
	}))
	defer listener.Close()
	f := newCodexSideFixture(t, false, config.Config{}, listener.URL)
	req := f.request(t, 1)
	parent := f.a.threads[sessionActionThread]
	before := parent.info
	parent.controlMu.Lock()
	done := make(chan protocol.SideQuestionResult, 1)
	go func() {
		result, _ := f.a.AskSideQuestion(f.ctx, sessionActionThread, f.snapshot, req, f.validate)
		done <- result
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		parent.controlMu.Unlock()
		close(release)
		t.Fatal("side question waited on the parent admission lock")
	}
	parent.controlMu.Unlock()
	if !f.a.ActiveRemoteTurns() {
		t.Error("API switch can retire a side request's selected source")
	}
	f.a.mu.Lock()
	nativeBusy := f.a.hasActiveTurnsLocked()
	f.a.mu.Unlock()
	if nativeBusy {
		t.Error("side request blocked native parent turn admission")
	}
	close(release)
	select {
	case result := <-done:
		if result.State != "completed" {
			t.Fatal(result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("side question failed to complete")
	}
	if f.a.ActiveRemoteTurns() {
		t.Fatal("side request left API-switch busy permanently")
	}
	assertCodexSideNoNativeMutation(t, f, before)
}

func TestCodexSideQuestionSourceRevocationRedactsReplyAndCachedReceipt(t *testing.T) {
	var calls atomic.Int32
	var revoked atomic.Bool
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		revoked.Store(true)
		_, _ = io.WriteString(w, codexSideAnswer)
	}))
	defer listener.Close()
	f := newCodexSideFixture(t, false, config.Config{}, listener.URL)
	f.validate = func() error {
		if revoked.Load() {
			return errors.New("fixture source revoked")
		}
		return nil
	}
	req := f.request(t, 1)
	result, err := f.a.AskSideQuestion(f.ctx, sessionActionThread, f.snapshot, req, f.validate)
	if err != nil || result.State != "unconfirmed" || result.Answer != "" || result.Usage != nil {
		t.Fatalf("source-revoked answer leaked: %+v %v", result, err)
	}
	if result, err = f.a.AskSideQuestion(f.ctx, sessionActionThread, f.snapshot, req, f.validate); err == nil || result.Answer != "" || calls.Load() != 1 {
		t.Fatalf("cached response bypassed revoked source: %+v %v", result, err)
	}
	revoked.Store(false)
	if result, err = f.a.AskSideQuestion(f.ctx, sessionActionThread, f.snapshot, req, f.validate); err != nil || result.State != "unconfirmed" || calls.Load() != 1 {
		t.Fatalf("unknown request replayed: %+v %v", result, err)
	}
}

func TestCodexSideQuestionMalformedResponsesNeverCompleteOrReplay(t *testing.T) {
	responses := map[string]string{
		"partial-envelope":   `{"type":"response.in_progress","response":` + codexSideAnswer + `}`,
		"failed-envelope":    `{"type":"response.failed","response":` + codexSideAnswer + `}`,
		"mixed-envelope":     `{"type":"response.completed","object":"response","response":` + codexSideAnswer + `}`,
		"nested-envelope":    `{"type":"response.completed","response":{"type":"response.completed","response":` + codexSideAnswer + `}}`,
		"missing-final":      strings.Replace(codexSideAnswer, `"status":"completed"`, `"status":"in_progress"`, 1),
		"tool-call":          strings.Replace(codexSideAnswer, `"type":"reasoning"`, `"type":"function_call"`, 1),
		"wrong-role":         strings.Replace(codexSideAnswer, `"role":"assistant"`, `"role":"user"`, 1),
		"unfinished-message": strings.Replace(codexSideAnswer, `"role":"assistant","status":"completed"`, `"role":"assistant","status":"in_progress"`, 1),
		"negative-usage":     strings.Replace(codexSideAnswer, `"input_tokens":13`, `"input_tokens":-1`, 1),
		"missing-usage":      strings.Replace(codexSideAnswer, `"input_tokens":13,`, ``, 1),
		"invalid-total":      strings.Replace(codexSideAnswer, `"total_tokens":20`, `"total_tokens":99`, 1),
		"js-overflow":        strings.Replace(codexSideAnswer, `"input_tokens":13`, `"input_tokens":9007199254740992`, 1),
		"error":              strings.Replace(codexSideAnswer, `"object":"response"`, `"object":"response","error":{"message":"private fixture key"}`, 1),
		"incomplete":         strings.Replace(codexSideAnswer, `"object":"response"`, `"object":"response","incomplete_details":{"reason":"max_output_tokens"}`, 1),
		"oversized-answer":   strings.Replace(codexSideAnswer, "Independent fixture answer", strings.Repeat("x", maxSideQuestionAnswer+1), 1),
		"invalid-utf8":       strings.Replace(codexSideAnswer, "Independent fixture answer", string([]byte{0xff}), 1),
	}
	for name, response := range responses {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = io.WriteString(w, response) }))
			defer listener.Close()
			f := newCodexSideFixture(t, false, config.Config{}, listener.URL)
			req := f.request(t, 1)
			for n := 0; n < 2; n++ {
				result, err := f.a.AskSideQuestion(f.ctx, sessionActionThread, f.snapshot, req, f.validate)
				if err != nil || result.State != "unconfirmed" || result.Answer != "" || result.Usage != nil || strings.Contains(result.Error, "private") {
					t.Fatalf("malformed response became confirmed or leaked: %+v %v", result, err)
				}
			}
			if calls.Load() != 1 {
				t.Fatal("unknown result was dispatched again")
			}
		})
	}
}
