package apiservice

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/pluginhook"
)

func TestNamedAPIPluginHookRunsAfterClientPolicyAndKeepsAccount(t *testing.T) {
	s, _ := fixtureService(t)
	var calls atomic.Int32
	hooks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), syntheticClientKey) || strings.Contains(string(raw), "credential-A") {
			t.Error("private identity leaked to plugin")
		}
		if r.URL.Path == "/plugin/hook/request" {
			calls.Add(1)
			fmt.Fprint(w, `{"body":{"model":"forbidden","input":"plugin content","tools":[{"type":"image_generation"}]}}`)
		} else {
			fmt.Fprint(w, `{}`)
		}
	}))
	defer hooks.Close()
	u, _ := url.Parse(hooks.URL)
	port, _ := strconv.Atoi(u.Port())
	s.Plugins = &pluginhook.Hooks{}
	_ = s.Plugins.Configure(port, "synthetic-named-api-plugin-token")
	defer s.Plugins.Disconnect("synthetic-named-api-plugin-token")
	s.Official = func(_ context.Context, a config.LocalAccount, body []byte, _ bool, _ bool, _ func() bool) (*http.Response, error) {
		var input map[string]any
		_ = json.Unmarshal(body, &input)
		if a.ID != "account-A" || a.CredentialRef != "credential-A" || input["model"] != "gpt-fixture" || input["input"] != "plugin content" || input["tools"] != nil {
			t.Errorf("changed caller authority or missed mutation: %+v %s", a, body)
		}
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	result := callAPI(s, syntheticClientKey, `{"model":"friendly","input":"original"}`)
	if result.Code != 200 || calls.Load() != 1 {
		t.Fatalf("missing hook: %d %s", result.Code, result.Body)
	}
	result = callAPI(s, "untrusted", `{"model":"gpt-fixture","input":"private"}`)
	if result.Code != 401 || calls.Load() != 1 {
		t.Fatal("invalid client reached plugin")
	}
}
