package apiservice

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
)

func TestManagedLaunchCredentialScopeAndAdmission(t *testing.T) {
	s, live := fixtureService(t)
	live.change(func(c *config.Config) {
		c.GatewayRuntimeKey = "synthetic-managed-runtime"
		c.CodexAPI.RuntimeLease = c.GatewayRuntimeKey
		c.CodexAPI.AuthEpoch = "service-epoch"
		c.CodexAPI.Clients[0].AuthEpoch = "client-epoch"
		c.RecordManagedAPI("codex", c.CodexAPI.Clients[0], "synthetic-lease", true, []string{"gpt-fixture"})
	})
	key := live.get().ManagedAPICredential("codex")
	calls := 0
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		calls++
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	request := func(route, credential string) int {
		r := apiRequest(context.Background(), credential, strings.NewReader(`{"model":"gpt-fixture","input":"synthetic"}`))
		r.URL.Path = route
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w.Code
	}
	const route = "/codex-api/managed/codex/v1/responses"
	if key == "" || request(route, key) != 200 {
		t.Fatal("managed dispatch unavailable")
	}
	for _, tc := range []struct{ route, key string }{{"/codex-api/v1/responses", key}, {route, syntheticClientKey}, {"/codex-api/managed/claude/v1/messages", key}, {"/codex-api/managed/codex-desktop/v1/responses", key}} {
		if request(tc.route, tc.key) != 401 {
			t.Fatal("credential escaped target scope")
		}
	}
	release, err := s.BeginManagedLaunch()
	if err != nil {
		t.Fatal(err)
	}
	if request(route, key) != 503 || request("/codex-api/v1/responses", syntheticClientKey) != 503 {
		t.Fatal("old work entered pending native switch")
	}
	s.AdmitManagedLaunch(key)
	if request(route, key) != 200 {
		t.Fatal("committed launch cannot bootstrap")
	}
	release()
	release()
	live.change(func(c *config.Config) { c.CodexAPI.Clients[0].AuthEpoch = "rotated" })
	if request(route, key) != 401 {
		t.Fatal("old managed authorization survived client rotation")
	}
	if calls != 2 {
		t.Fatalf("unexpected provider calls %d", calls)
	}
}

func TestManagedLaunchDoesNotInterruptInflightRequest(t *testing.T) {
	s, _ := fixtureService(t)
	entered, finish, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		close(entered)
		<-finish
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	go func() {
		defer close(done)
		if got := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture","input":"synthetic"}`); got.Code != 200 {
			t.Errorf("running request failed: %d", got.Code)
		}
	}()
	<-entered
	if release, err := s.BeginManagedLaunch(); err == nil {
		release()
		t.Error("launch admitted during active request")
	}
	close(finish)
	awaitDone(t, done)
}

func TestManagedClaudeMessagesUsesExistingGatewayTranslation(t *testing.T) {
	s, live := fixtureService(t)
	live.change(func(c *config.Config) {
		c.GatewayRuntimeKey = "synthetic-runtime"
		c.CodexAPI.RuntimeLease = c.GatewayRuntimeKey
		c.CodexAPI.AuthEpoch = "service"
		c.CodexAPI.Clients[0].AuthEpoch = "client"
		c.RecordManagedAPI("claude", c.CodexAPI.Clients[0], "lease", false, nil)
	})
	s.Official = func(_ context.Context, _ config.LocalAccount, body []byte, _, _ bool, _ func() bool) (*http.Response, error) {
		if strings.Contains(string(body), `"messages"`) {
			t.Error("Anthropic envelope bypassed translation")
		}
		sse := "event: response.completed\ndata: " + `{"type":"response.completed","response":{"id":"fixture-response","object":"response","status":"completed","model":"gpt-fixture","output":[{"id":"msg","type":"message","role":"assistant","content":[{"type":"output_text","text":"synthetic reply"}]}],"usage":{"input_tokens":2,"output_tokens":1}}}` + "\n\n"
		return successfulResponse(io.NopCloser(strings.NewReader(sse)), true), nil
	}
	r := apiRequest(context.Background(), live.get().ManagedAPICredential("claude"), strings.NewReader(`{"model":"friendly","messages":[{"role":"user","content":"synthetic"}],"max_tokens":64}`))
	r.URL.Path = "/codex-api/managed/claude/v1/messages"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "synthetic reply") {
		t.Fatalf("messages translation failed: %d %s", w.Code, w.Body)
	}
}

func TestAdditionalManagedLaunchCredentialsRemainTargetScoped(t *testing.T) {
	for _, target := range []string{"deepseek-harness", "hermes"} {
		t.Run(target, func(t *testing.T) {
			s, live := fixtureService(t)
			live.change(func(c *config.Config) {
				c.GatewayRuntimeKey = "synthetic-additional-runtime"
				c.CodexAPI.RuntimeLease = c.GatewayRuntimeKey
				c.CodexAPI.AuthEpoch = "service-epoch"
				c.CodexAPI.Clients[0].AuthEpoch = "client-epoch"
				c.RecordManagedAPI(target, c.CodexAPI.Clients[0], "synthetic-target-lease", true, []string{"gpt-fixture"})
			})
			key := live.get().ManagedAPICredential(target)
			if key == "" {
				t.Fatal("extension target has no scoped credential")
			}
			s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
				return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
			}
			request := func(path, credential string) int {
				r := apiRequest(context.Background(), credential, strings.NewReader(`{"model":"gpt-fixture","input":"synthetic"}`))
				r.URL.Path = path
				w := httptest.NewRecorder()
				s.ServeHTTP(w, r)
				return w.Code
			}
			path := "/codex-api/managed/" + target + "/v1/responses"
			if code := request(path, key); code != 200 {
				t.Fatal("scoped extension dispatch failed", code)
			}
			for _, route := range []string{"/codex-api/v1/responses", "/codex-api/managed/codex/v1/responses", "/codex-api/managed/claude/v1/responses"} {
				if code := request(route, key); code != 401 {
					t.Fatal("credential escaped target", route, code)
				}
			}
			if code := request(path, syntheticClientKey); code != 401 {
				t.Fatal("manual client bypassed managed lease", code)
			}
			if _, _, ok := managedAPIPath("/codex-api/managed/arbitrary-agent/v1/responses"); ok {
				t.Fatal("arbitrary target admitted")
			}
		})
	}
}
