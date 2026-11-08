package console

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/config"
)

const accountTestChatSSE = "data: {\"id\":\"test\",\"object\":\"chat.completion.chunk\",\"model\":\"coding-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"selected-only\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: [DONE]\n\n"

const accountTestResponsesSSE = `data: {"type":"response.created","response":{"id":"r1","model":"coding-model"}}

data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"selected-only"}

data: {"type":"response.completed","response":{"id":"r1","object":"response","status":"completed","model":"coding-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"selected-only"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}

`

const accountTestAnthropicSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg-test","type":"message","role":"assistant","model":"coding-model","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"selected-only"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`

func prepareAccountTest(t *testing.T, s *Server, h http.Handler, target map[string]any) string {
	t.Helper()
	w := localRequest(s, h, "POST", "/api/local/accounts/test/prepare", target)
	var out struct {
		Revision string `json:"revision"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Revision) != 64 {
		t.Fatalf("prepare: %d %s", w.Code, w.Body)
	}
	return out.Revision
}

func accountTestInput(id, revision string) map[string]any {
	return map[string]any{"accountId": id, "revision": revision, "model": "coding-model", "messages": []accountTestMessage{{Role: "user", Content: "private-test-message"}}}
}

func testChatAccount(t *testing.T, s *Server, h http.Handler, url string) string {
	t.Helper()
	id := createLocal(t, s, h, "api", "指定 API", "private-test-key", url)
	if err := s.d.Store.Update(func(c *config.Config) error {
		for i := range c.LocalAccounts {
			if c.LocalAccounts[i].ID == id {
				c.LocalAccounts[i].Wire = "chat"
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAccountPlaygroundUsesExactSavedCredentialAndRecordsOnlyMetadata(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer private-test-key" || !strings.Contains(string(body), "private-test-message") {
			t.Error("wrong target, credential or message")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, accountTestChatSSE)
	}))
	defer upstream.Close()
	s, h := newTestServer(t)
	id := testChatAccount(t, s, h, upstream.URL)
	_ = createLocal(t, s, h, "api", "另一个账号", "other-private-key", "https://unused.invalid")
	revision := prepareAccountTest(t, s, h, map[string]any{"accountId": id})
	if calls.Load() != 0 {
		t.Fatal("opening test panel contacted upstream")
	}
	before := s.d.Store.Get()
	w := localRequest(s, h, "POST", "/api/local/accounts/test/send", accountTestInput(id, revision))
	if w.Code != 200 || calls.Load() != 1 || !strings.Contains(w.Body.String(), "selected-only") || !strings.Contains(w.Body.String(), `"state":"completed"`) || !strings.Contains(w.Body.String(), "event: diagnostic") {
		t.Fatalf("test response: %d %s, calls %d", w.Code, w.Body, calls.Load())
	}
	if strings.Contains(w.Body.String(), "private-test-key") {
		t.Fatal("credential returned to browser")
	}
	after := s.d.Store.Get()
	if len(after.LocalAccounts) != len(before.LocalAccounts) || after.CodexAPI.Enabled != before.CodexAPI.Enabled || after.ActiveCodexAccount != before.ActiveCodexAccount {
		t.Fatal("test changed configured service")
	}
	flushConsoleLedger(t, s)
	q := localRequest(s, h, "GET", "/api/local/usage-ledger", nil)
	if strings.Contains(q.Body.String(), "private-test-message") || strings.Contains(q.Body.String(), "selected-only") || strings.Contains(q.Body.String(), "private-test-key") || !strings.Contains(q.Body.String(), `"completed"`) {
		t.Fatal("ledger did not keep metadata boundary")
	}
}

func TestAccountPlaygroundRefusesStaleTargetPolicyAndUntrustedInput(t *testing.T) {
	s, h := newTestServer(t)
	id := testChatAccount(t, s, h, "https://unused.invalid")
	revision := prepareAccountTest(t, s, h, map[string]any{"accountId": id})
	for _, body := range []map[string]any{
		{"accountId": id, "officialId": "other"}, {"accountId": id, "url": "https://evil.invalid"},
	} {
		if w := localRequest(s, h, "POST", "/api/local/accounts/test/prepare", body); w.Code != 400 {
			t.Fatal("accepted ambiguous target", w.Code)
		}
	}
	input := accountTestInput(id, revision)
	input["tools"] = []any{map[string]any{"type": "function"}}
	if w := localRequest(s, h, "POST", "/api/local/accounts/test/send", input); w.Code != 400 {
		t.Fatal("tools accepted", w.Code)
	}
	if w := do(h, "POST", "/api/local/accounts/test/send", "127.0.0.1:47831", map[string]string{"Content-Type": "application/json"}, `{}`); w.Code != 401 {
		t.Fatal("console auth bypass")
	}
	if w := do(h, "POST", "/api/local/accounts/test/send", "127.0.0.1:47831", map[string]string{"Cookie": CookieName + "=" + s.Token(), "Content-Type": "application/json", "Origin": "https://evil.invalid"}, `{}`); w.Code != 403 {
		t.Fatal("origin bypass")
	}
	if err := s.d.Store.Update(func(c *config.Config) error { c.LocalAccounts[0].ModelDenylist = []string{"coding-model"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if w := localRequest(s, h, "POST", "/api/local/accounts/test/send", accountTestInput(id, revision)); w.Code != 409 {
		t.Fatal("stale source was used", w.Code)
	}
	revision = prepareAccountTest(t, s, h, map[string]any{"accountId": id})
	if w := localRequest(s, h, "POST", "/api/local/accounts/test/send", accountTestInput(id, revision)); w.Code != 403 {
		t.Fatal("model rule bypass", w.Code)
	}
}

func TestAccountPlaygroundAdaptsSavedProtocolsAndExplicitModels(t *testing.T) {
	for _, tc := range []struct{ name, wire, model, path, response, auth string }{
		{"responses", "responses", "custom-model", "/v1/responses", accountTestResponsesSSE, "bearer"},
		{"anthropic", "anthropic", "custom-model", "/v1/messages", accountTestAnthropicSSE, "api-key"},
		{"auto-gpt", "auto", "gpt-custom", "/v1/responses", accountTestResponsesSSE, "bearer"},
		{"auto-claude", "auto", "claude-custom", "/v1/messages", accountTestAnthropicSSE, "bearer"},
		{"auto-unknown", "auto", "custom-model", "/v1/responses", accountTestResponsesSSE, "bearer"},
		{"explicit-chat-gpt", "chat", "gpt-custom", "/v1/chat/completions", accountTestChatSSE, "bearer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				body, _ := io.ReadAll(r.Body)
				var payload map[string]any
				if json.Unmarshal(body, &payload) != nil || r.URL.Path != tc.path || payload["model"] != tc.model || payload["stream"] != true {
					t.Error("wrong protocol, model or stream mode", r.URL.Path)
				}
				for _, text := range []string{"private-test-message", "previous-reply", "next-message"} {
					if !strings.Contains(string(body), text) {
						t.Error("lost conversation context")
					}
				}
				if tc.auth == "api-key" {
					if r.Header.Get("x-api-key") != "private-test-key" || r.Header.Get("Authorization") != "" || r.Header.Get("anthropic-version") == "" {
						t.Error("wrong Anthropic authentication")
					}
				} else if r.Header.Get("Authorization") != "Bearer private-test-key" {
					t.Error("wrong bearer authentication")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.response)
			}))
			defer upstream.Close()
			s, h := newTestServer(t)
			id := testChatAccount(t, s, h, upstream.URL)
			if err := s.d.Store.Update(func(c *config.Config) error {
				c.LocalAccounts[0].Wire, c.LocalAccounts[0].AuthMode = tc.wire, tc.auth
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before, _ := s.d.Store.Get().LocalAccount(id)
			revision := prepareAccountTest(t, s, h, map[string]any{"accountId": id})
			input := accountTestInput(id, revision)
			input["model"] = tc.model
			input["messages"] = []accountTestMessage{{Role: "user", Content: "private-test-message"}, {Role: "assistant", Content: "previous-reply"}, {Role: "user", Content: "next-message"}}
			w := localRequest(s, h, "POST", "/api/local/accounts/test/send", input)
			if w.Code != 200 || calls.Load() != 1 || !strings.Contains(w.Body.String(), "selected-only") || !strings.Contains(w.Body.String(), `"state":"completed"`) {
				t.Fatalf("protocol response: %d %s", w.Code, w.Body)
			}
			after, _ := s.d.Store.Get().LocalAccount(id)
			if config.AccountFingerprint(before) != config.AccountFingerprint(after) {
				t.Fatal("testing a custom model changed the saved account")
			}
		})
	}
}

func TestAccountPlaygroundDoesNotRetryRefusalOrClaimTruncatedSuccess(t *testing.T) {
	for _, status := range []int{401, 429, 200} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if status != 200 {
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"error":{"message":"private-test-key private-error"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
			}))
			defer upstream.Close()
			s, h := newTestServer(t)
			id := testChatAccount(t, s, h, upstream.URL)
			revision := prepareAccountTest(t, s, h, map[string]any{"accountId": id})
			w := localRequest(s, h, "POST", "/api/local/accounts/test/send", accountTestInput(id, revision))
			if calls.Load() != 1 || strings.Contains(w.Body.String(), `"state":"completed"`) || strings.Contains(w.Body.String(), "private-error") || strings.Contains(w.Body.String(), "private-test-key") {
				t.Fatalf("unsafe result: %d %s", w.Code, w.Body)
			}
			if status == 200 && !strings.Contains(w.Body.String(), `"state":"interrupted"`) {
				t.Fatal("truncation hidden", w.Body)
			}
		})
	}
}

func TestAccountPlaygroundCancellationAndDuplicateCredentialFence(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "waiting-for-headers", true: "receiving-stream"}[streaming], func(t *testing.T) {
			testAccountPlaygroundCancellation(t, streaming)
		})
	}
}

func testAccountPlaygroundCancellation(t *testing.T, streaming bool) {
	t.Helper()
	started, stopped := make(chan struct{}), make(chan struct{})
	cleanup := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Consume the request as an inference endpoint does so net/http can
		// monitor the connection for a disconnect while the handler waits.
		_, _ = io.Copy(io.Discard, r.Body)
		if streaming {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
			w.(http.Flusher).Flush()
		}
		close(started)
		select {
		case <-r.Context().Done():
			close(stopped)
		case <-cleanup:
		}
	}))
	defer upstream.Close()
	defer close(cleanup)
	s, h := newTestServer(t)
	id := testChatAccount(t, s, h, upstream.URL)
	revision := prepareAccountTest(t, s, h, map[string]any{"accountId": id})
	alias := testChatAccount(t, s, h, upstream.URL)
	aliasRevision := prepareAccountTest(t, s, h, map[string]any{"accountId": alias})
	body, _ := json.Marshal(accountTestInput(id, revision))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("POST", "/api/local/accounts/test/send", strings.NewReader(string(body))).WithContext(ctx)
	r.Host = "127.0.0.1:47831"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Cookie", CookieName+"="+s.Token())
	done := make(chan struct{})
	go func() { h.ServeHTTP(httptest.NewRecorder(), r); close(done) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not start")
	}
	if w := localRequest(s, h, "POST", "/api/local/accounts/test/send", accountTestInput(id, revision)); w.Code != 429 {
		t.Fatal("overlapping request admitted", w.Code)
	}
	if w := localRequest(s, h, "POST", "/api/local/accounts/test/send", accountTestInput(alias, aliasRevision)); w.Code != 429 {
		t.Fatal("duplicate credential bypassed admission", w.Code)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("local request did not cancel")
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not cancel")
	}
	a, _ := s.d.Store.Get().LocalAccount(id)
	release, ok := s.acquireAccountTest(a)
	if !ok {
		t.Fatal("cancelled request leaked admission")
	}
	release()
}

func TestAccountPlaygroundOfficialAccountUsesProtectedExecutor(t *testing.T) {
	s, h := officialConsole(t)
	officialID := importOfficialConsole(t, s, h)
	var calls int
	s.d.OfficialGateway = func(ctx context.Context, a config.LocalAccount, body []byte, stream, compact bool, current func() bool) (*http.Response, error) {
		calls++
		if a.CredentialRef != officialID || !current() || !stream || compact || !strings.Contains(string(body), "private-test-message") {
			t.Error("wrong official execution")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(accountTestResponsesSSE))}, nil
	}
	for _, vaultOnly := range []bool{false, true} {
		if vaultOnly {
			if err := s.d.Store.Update(func(c *config.Config) error { c.LocalAccounts = nil; return nil }); err != nil {
				t.Fatal(err)
			}
		}
		target := map[string]any{"accountId": "official-" + officialID}
		if vaultOnly {
			target = map[string]any{"officialId": officialID}
		}
		revision := prepareAccountTest(t, s, h, target)
		input := accountTestInput("official-"+officialID, revision)
		if vaultOnly {
			delete(input, "accountId")
			input["officialId"] = officialID
		}
		w := localRequest(s, h, "POST", "/api/local/accounts/test/send", input)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "selected-only") || !strings.Contains(w.Body.String(), `"state":"completed"`) {
			t.Fatalf("official response: %d %s", w.Code, w.Body)
		}
		assertNoOfficialSecrets(t, w.Body.String())
	}
	if calls != 2 {
		t.Fatal("official executor count", calls)
	}
}
