package apiservice

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func namespaceService(t *testing.T) *Service {
	t.Helper()
	s, live := fixtureService(t)
	live.change(func(c *config.Config) {
		client := &c.CodexAPI.Clients[0]
		client.ModelPrefix = "team/"
		client.ModelAliases["gpt-fixture"] = "gpt-denied"
		client.ModelAllowPrefixes = []string{"gpt-"}
		client.ModelDenylist = []string{"gpt-denied"}
	})
	return s
}

func TestNamedModelNamespaceHTTPAndCatalogUseSameSingleAliasPolicy(t *testing.T) {
	s := namespaceService(t)
	var calls atomic.Int32
	s.Official = func(_ context.Context, _ config.LocalAccount, raw []byte, stream, compact bool, _ func() bool) (*http.Response, error) {
		calls.Add(1)
		var request map[string]any
		if json.Unmarshal(raw, &request) != nil || request["model"] != "gpt-fixture" || stream || request["metadata"].(map[string]any)["fixture"] != "preserved" {
			t.Errorf("namespace damaged protected request: %s", raw)
		}
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	for _, path := range []string{"v1/responses", "v1/responses/compact", "v1/chat/completions"} {
		for _, model := range []string{"friendly", "wrong/friendly", "team/", "team/team/friendly", "team/gpt-fixture", "team/gpt-denied"} {
			body := `{"model":` + strconv.Quote(model) + `,"input":"synthetic","metadata":{"fixture":"preserved"}}`
			if path == "v1/chat/completions" {
				body = `{"model":` + strconv.Quote(model) + `,"messages":[{"role":"user","content":"synthetic"}]}`
			}
			r := apiRequest(context.Background(), syntheticClientKey, strings.NewReader(body))
			r.URL.Path = "/codex-api/" + path
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != 403 || calls.Load() != 0 {
				t.Fatalf("namespace policy bypassed: path=%s model=%s status=%d calls=%d", path, model, w.Code, calls.Load())
			}
		}
		if path == "v1/chat/completions" {
			continue // The independent Chat fixtures verify its message translation.
		}
		r := apiRequest(context.Background(), syntheticClientKey, strings.NewReader(`{"model":"team/friendly","input":"synthetic","metadata":{"fixture":"preserved"}}`))
		r.URL.Path = "/codex-api/" + path
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("authorized namespaced alias failed: path=%s status=%d body=%s", path, w.Code, w.Body)
		}
		calls.Store(0)
	}
	s.Official = func(_ context.Context, _ config.LocalAccount, raw []byte, stream, compact bool, _ func() bool) (*http.Response, error) {
		calls.Add(1)
		var request map[string]any
		if json.Unmarshal(raw, &request) != nil || request["model"] != "gpt-fixture" || !stream || compact || !strings.Contains(string(raw), "original tool result") {
			t.Errorf("namespaced Chat lost protected model/tool history: %s", raw)
		}
		return successfulResponse(io.NopCloser(strings.NewReader("data: "+completedChatResponseEvent+"\n\n")), true), nil
	}
	chat := callChatAPI(s, syntheticClientKey, strings.Replace(chatWithTools, `"model":"friendly"`, `"model":"team/friendly"`, 1))
	if chat.Code != 200 || calls.Load() != 1 || !strings.Contains(chat.Body.String(), "tool_calls") {
		t.Fatal("namespaced Chat failed", chat.Code, chat.Body)
	}
	s.ModelCatalog = func(context.Context, config.LocalAccount, bool) (protocol.ModelCatalog, error) {
		return protocol.ModelCatalog{Models: []string{"gpt-fixture", "gpt-denied", "gpt-future-native"}}, nil
	}
	r := apiRequest(context.Background(), syntheticClientKey, nil)
	r.Method, r.URL.Path = http.MethodGet, "/codex-api/v1/models"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var catalog struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &catalog) != nil {
		t.Fatal("namespace discovery failed", w.Code, w.Body)
	}
	seen := map[string]bool{}
	for _, model := range catalog.Data {
		seen[model.ID] = true
	}
	if len(seen) != 2 || !seen["team/friendly"] || !seen["team/gpt-future-native"] {
		t.Fatalf("catalog disagrees with actual model/alias policy: %+v", seen)
	}
}

func TestNamedWebSocketModelNamespaceRetainsVisibleAliasAcrossContinuation(t *testing.T) {
	s := namespaceService(t)
	var calls atomic.Int32
	s.Official = func(_ context.Context, _ config.LocalAccount, raw []byte, _, _ bool, _ func() bool) (*http.Response, error) {
		calls.Add(1)
		var request map[string]any
		if json.Unmarshal(raw, &request) != nil || request["model"] != "gpt-fixture" {
			t.Errorf("visible name resolved more than once: %s", raw)
		}
		return successfulResponse(io.NopCloser(strings.NewReader(namedCompleted("namespace-response"))), true), nil
	}
	c := namedSocket(t, namedSocketServer(t, s), syntheticClientKey)
	for _, model := range []string{"friendly", "wrong/friendly", "team/", "team/team/friendly", "team/gpt-fixture"} {
		sendNamedFrame(t, c, map[string]any{"type": "response.create", "model": model, "generate": false})
		if event := namedSocketEvent(t, c, "error"); event["error"].(map[string]any)["code"] != "invalid_model" || calls.Load() != 0 {
			t.Fatal("socket namespace bypassed", event)
		}
	}
	sendNamedFrame(t, c, map[string]any{"type": "response.create", "model": "team/friendly", "generate": false, "input": "private synthetic input"})
	warm := namedSocketEvent(t, c, "response.completed")["response"].(map[string]any)
	sendNamedFrame(t, c, map[string]any{"type": "response.create", "previous_response_id": warm["id"], "input": []any{}})
	first := namedSocketEvent(t, c, "response.completed")["response"].(map[string]any)
	sendNamedFrame(t, c, map[string]any{"type": "response.create", "previous_response_id": first["id"], "input": "continuation"})
	namedSocketEvent(t, c, "response.completed")
	if calls.Load() != 2 {
		t.Fatal("socket lost namespaced continuation")
	}
}
