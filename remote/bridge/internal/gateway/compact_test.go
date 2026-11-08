package gateway

import (
	"bytes"
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

const compactFixtureResponse = `{"id":"resp_compact","object":"response.compaction","created_at":1,"output":[{"id":"msg_1","type":"message","role":"user","content":[{"type":"input_text","text":"original context"}]},{"id":"cmp_1","type":"compaction","encrypted_content":"opaque-provider-state"}],"usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120}}`

func compactFixtureGateway(t *testing.T, account config.LocalAccount, upstream http.HandlerFunc) (*Handler, *config.LocalAccount, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if upstream != nil {
			upstream(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, compactFixtureResponse)
	}))
	t.Cleanup(u.Close)
	account.ID, account.Kind = "compact-account", "codex"
	account.Key, account.BaseURL = upstreamTestKey, u.URL+"/prefix/v1"
	if account.Model == "" {
		account.Model = "gpt-5"
	}
	h := New(func() string { return localTestKey }, func(string) (config.LocalAccount, bool) { return account, true })
	return h, &account, calls
}

func TestCompactNativePreservesPayloadResultAndCredentials(t *testing.T) {
	body := `{
  "model": "gpt-5",
  "input": [{"role":"user","content":"original context"},{"type":"custom_tool_call_output","call_id":"call_1","output":"tool history"},{"type":"compaction","encrypted_content":"prior-opaque-state"}],
  "instructions": "preserve this instruction",
  "previous_response_id": "resp_previous",
  "prompt_cache_key": "cache-key",
  "prompt_cache_options": {"mode":"explicit","ttl":"30m"},
  "prompt_cache_retention": "24h",
  "service_tier": "default",
  "provider_option": {"future":"preserved"}
}`
	for _, sourceType := range []string{"", "api_key", "reverse_proxy"} {
		for _, authMode := range []string{"bearer", "api-key"} {
			t.Run(sourceType+"/"+authMode, func(t *testing.T) {
				h, _, calls := compactFixtureGateway(t, config.LocalAccount{Protocol: "responses", SourceType: sourceType, AuthMode: authMode}, func(w http.ResponseWriter, r *http.Request) {
					got, err := io.ReadAll(r.Body)
					if err != nil || !bytes.Equal(got, []byte(body)) {
						t.Errorf("compact payload changed: %s (%v)", got, err)
					}
					if r.Method != http.MethodPost || r.URL.Path != "/prefix/v1/responses/compact" || r.URL.RawQuery != "" {
						t.Errorf("wrong compact upstream route: %s %s", r.Method, r.URL)
					}
					if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/json" {
						t.Errorf("wrong compact request content headers: %v", r.Header)
					}
					wantBearer, wantAPIKey := "Bearer "+upstreamTestKey, ""
					if authMode == "api-key" {
						wantBearer, wantAPIKey = "", upstreamTestKey
					}
					if r.Header.Get("Authorization") != wantBearer || r.Header.Get("x-api-key") != wantAPIKey {
						t.Error("compact used the wrong upstream credential")
					}
					for _, header := range []string{"Cookie", "X-Salcara-Session", "X-Client-Secret", "anthropic-version"} {
						if r.Header.Get(header) != "" {
							t.Errorf("compact forwarded client-only header %s", header)
						}
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, compactFixtureResponse)
				})
				r := httptest.NewRequest(http.MethodPost, "/gateway/codex/v1/responses/compact", strings.NewReader(body))
				r.RemoteAddr = "127.0.0.1:10000"
				r.Header.Set("Authorization", "Bearer "+localTestKey)
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Cookie", "client-cookie")
				r.Header.Set("X-Salcara-Session", "local-affinity")
				r.Header.Set("X-Client-Secret", "client-private-header")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != http.StatusOK || w.Body.String() != compactFixtureResponse || !json.Valid(w.Body.Bytes()) || calls.Load() != 1 {
					t.Fatalf("compact result changed: %d %s (calls %d)", w.Code, w.Body, calls.Load())
				}
			})
		}
	}
}

func TestCompactRejectsDeniedOrUnknownModelsBeforeUpstream(t *testing.T) {
	for _, tc := range []struct {
		name    string
		account config.LocalAccount
		body    string
		status  int
	}{
		{"denied", config.LocalAccount{ModelDenylist: []string{"gpt-5"}}, `{"model":"gpt-5","input":[]}`, http.StatusForbidden},
		{"not-allowed", config.LocalAccount{ModelAllowlist: []string{"gpt-5-mini"}}, `{"model":"gpt-5","input":[]}`, http.StatusForbidden},
		{"unknown", config.LocalAccount{}, `{"model":"gpt-unknown","input":[]}`, http.StatusBadRequest},
		{"missing", config.LocalAccount{}, `{"input":[]}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, calls := compactFixtureGateway(t, tc.account, nil)
			w := callGateway(h, "codex", "responses/compact", tc.body)
			if w.Code != tc.status || calls.Load() != 0 {
				t.Fatalf("unsupported model sent upstream: %d %s (calls %d)", w.Code, w.Body, calls.Load())
			}
		})
	}
}

func TestCompactDoesNotConvertProtocolsOrCredentialSources(t *testing.T) {
	for _, tc := range []struct {
		name    string
		account config.LocalAccount
	}{
		{"chat-wire", config.LocalAccount{Wire: "chat"}},
		{"anthropic-wire", config.LocalAccount{Wire: "anthropic"}},
		{"chat-model", config.LocalAccount{Model: "grok-4", Protocol: "responses"}},
		{"anthropic-model", config.LocalAccount{Model: "claude-sonnet-4", Protocol: "responses"}},
		{"unknown-protocol", config.LocalAccount{Model: "chosen-model", Protocol: "unsupported"}},
		{"oauth", config.LocalAccount{SourceType: "oauth"}},
		{"official-account", config.LocalAccount{SourceType: "official_account"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, a, calls := compactFixtureGateway(t, tc.account, nil)
			w := callGateway(h, "codex", "responses/compact", `{"model":"`+a.Model+`","input":[]}`)
			if w.Code != http.StatusNotImplemented || calls.Load() != 0 {
				t.Fatalf("compact converted or changed credential source: %d %s (calls %d)", w.Code, w.Body, calls.Load())
			}
		})
	}
}

func TestCompactRejectsStreamingAndConflictingContinuation(t *testing.T) {
	h, _, calls := compactFixtureGateway(t, config.LocalAccount{}, nil)
	for _, body := range []string{
		`{"model":"gpt-5","stream":true,"input":[]}`,
		`{"model":"gpt-5","stream":"true","input":[]}`,
		`{"model":"gpt-5","previous_response_id":"resp_previous","conversation":"conversation_1","input":[]}`,
	} {
		w := callGateway(h, "codex", "responses/compact", body)
		if w.Code != http.StatusBadRequest || calls.Load() != 0 {
			t.Fatalf("incompatible compact sent upstream: %d %s (calls %d)", w.Code, w.Body, calls.Load())
		}
	}
}

type compactChangeReader struct {
	io.Reader
	change func()
}

func (r *compactChangeReader) Read(p []byte) (int, error) {
	if r.change != nil {
		r.change()
		r.change = nil
	}
	return r.Reader.Read(p)
}

func TestCompactRechecksSourceAndCredentialAfterReadingBody(t *testing.T) {
	for _, change := range []string{"source", "local-credential", "provider-wire", "model-policy"} {
		t.Run(change, func(t *testing.T) {
			h, a, calls := compactFixtureGateway(t, config.LocalAccount{}, nil)
			key := localTestKey
			h.Key = func() string { return key }
			body := &compactChangeReader{Reader: strings.NewReader(`{"model":"gpt-5","input":[]}`), change: func() {
				switch change {
				case "source":
					a.Key = "replacement-upstream-key"
				case "local-credential":
					key = "replacement-local-key"
				case "provider-wire":
					a.Wire = "chat"
				case "model-policy":
					a.ModelDenylist = []string{"gpt-5"}
				}
			}}
			r := httptest.NewRequest(http.MethodPost, "/gateway/codex/v1/responses/compact", body)
			r.RemoteAddr = "127.0.0.1:10000"
			r.Header.Set("Authorization", "Bearer "+localTestKey)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusConflict || calls.Load() != 0 {
				t.Fatalf("compact used a stale source: %d %s (calls %d)", w.Code, w.Body, calls.Load())
			}
		})
	}
}

type compactRepeatReader struct{}

func (compactRepeatReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestCompactRejectsMalformedAndOversizedBodies(t *testing.T) {
	h, _, calls := compactFixtureGateway(t, config.LocalAccount{}, nil)
	for _, body := range []string{`{`, `null`, `[]`, `{"model":42}`, `{"model":"gpt-5"} {}`} {
		w := callGateway(h, "codex", "responses/compact", body)
		if w.Code != http.StatusBadRequest || calls.Load() != 0 {
			t.Fatalf("malformed compact sent upstream: %d %s (calls %d)", w.Code, w.Body, calls.Load())
		}
	}
	body := io.MultiReader(strings.NewReader(`{"model":"gpt-5","input":"`), io.LimitReader(compactRepeatReader{}, maxBody), strings.NewReader(`"}`))
	r := httptest.NewRequest(http.MethodPost, "/gateway/codex/v1/responses/compact", body)
	r.RemoteAddr = "127.0.0.1:10000"
	r.Header.Set("Authorization", "Bearer "+localTestKey)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || calls.Load() != 0 {
		t.Fatalf("oversized compact sent upstream: %d %s (calls %d)", w.Code, w.Body, calls.Load())
	}
}

func TestCompactSanitizesUpstreamErrorsAndRejectsInvalidResults(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		contentType string
		body        string
		want        int
	}{
		{"provider-error", http.StatusUnauthorized, "application/json", `{"error":{"message":"` + upstreamTestKey + ` ` + localTestKey + `"}}`, http.StatusUnauthorized},
		{"invalid-json", http.StatusOK, "application/json", `{"incomplete":`, http.StatusBadGateway},
		{"unexpected-stream", http.StatusOK, "text/event-stream", "data: " + compactFixtureResponse + "\n\n", http.StatusBadGateway},
		{"credential-in-result", http.StatusOK, "application/json", `{"secret":"` + upstreamTestKey + `"}`, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, calls := compactFixtureGateway(t, config.LocalAccount{}, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			w := callGateway(h, "codex", "responses/compact", `{"model":"gpt-5","input":[]}`)
			if w.Code != tc.want || !json.Valid(w.Body.Bytes()) || strings.Contains(w.Body.String(), upstreamTestKey) || strings.Contains(w.Body.String(), localTestKey) || calls.Load() != 1 {
				t.Fatalf("unsafe compact result: %d %s (calls %d)", w.Code, w.Body, calls.Load())
			}
		})
	}
}

type compactRoundTripper func(*http.Request) (*http.Response, error)

func (f compactRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCompactUsesBoundedRequestDeadline(t *testing.T) {
	h, _, calls := compactFixtureGateway(t, config.LocalAccount{}, nil)
	transport := h.Client.Transport
	h.Client.Transport = compactRoundTripper(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		remaining := time.Until(deadline)
		if !ok || remaining > 2*time.Minute || remaining < 110*time.Second {
			t.Errorf("compact request deadline is not two minutes: %v %v", ok, remaining)
		}
		return transport.RoundTrip(r)
	})
	w := callGateway(h, "codex", "responses/compact", `{"model":"gpt-5","input":[]}`)
	if w.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("bounded compact request failed: %d %s", w.Code, w.Body)
	}
}
