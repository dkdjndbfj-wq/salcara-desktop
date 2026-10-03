package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"salcara/bridge/internal/config"
)

// Codex on one API key can switch between GPT and Grok inside a thread: each
// request goes upstream over the wire its model family needs.
func TestGatewayPicksUpstreamWirePerRequestModel(t *testing.T) {
	var paths []string
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/chat/completions" {
			_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","created":1,"model":"grok-4","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"r","object":"response","status":"completed","model":"gpt-5","output":[]}`)
	}))
	defer u.Close()
	a := config.LocalAccount{ID: "k", Kind: "codex", Key: upstreamTestKey, BaseURL: u.URL, Model: "gpt-5", Models: []string{"gpt-5", "grok-4"}, Protocol: "responses", AuthMode: "bearer", CatalogOverride: true}
	h := New(func() string { return localTestKey }, func(string) (config.LocalAccount, bool) { return a, true })
	if w := callGateway(h, "codex", "responses", `{"model":"grok-4","input":"hi"}`); w.Code != 200 {
		t.Fatalf("grok: %d %s", w.Code, w.Body)
	}
	if w := callGateway(h, "codex", "responses", `{"model":"gpt-5","input":"hi"}`); w.Code != 200 {
		t.Fatalf("gpt: %d %s", w.Code, w.Body)
	}
	if len(paths) != 2 || paths[0] != "/v1/chat/completions" || paths[1] != "/v1/responses" {
		t.Fatalf("upstream paths %v", paths)
	}
}
