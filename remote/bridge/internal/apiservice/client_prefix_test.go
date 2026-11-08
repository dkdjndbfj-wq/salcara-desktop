package apiservice

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func TestClientPrefixRulesCheckResolvedModelsAndCatalog(t *testing.T) {
	s, live := fixtureService(t)
	live.change(func(c *config.Config) {
		client := &c.CodexAPI.Clients[0]
		client.ModelAllowPrefixes = []string{"gpt-"}
		client.ModelDenyPrefixes = []string{"gpt-blocked"}
		client.ModelAllowlist = []string{"other-exact", "gpt-blocked-exact"}
		client.ModelAliases = map[string]string{"friendly": "gpt-fixture", "gpt-bypass": "other-model", "blocked": "gpt-blocked-exact", "gpt-shadow": "gpt-blocked-exact"}
	})
	var calls atomic.Int32
	s.Official = func(_ context.Context, _ config.LocalAccount, body []byte, _, _ bool, _ func() bool) (*http.Response, error) {
		calls.Add(1)
		var input map[string]any
		_ = json.Unmarshal(body, &input)
		if input["model"] != "gpt-fixture" {
			t.Error("forbidden alias reached executor")
		}
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	for _, model := range []string{"gpt-bypass", "blocked", "gpt-shadow", "gpt-blocked-exact", "other-model"} {
		body, _ := json.Marshal(map[string]string{"model": model})
		if w := callAPI(s, syntheticClientKey, string(body)); w.Code != 403 {
			t.Errorf("%q accepted: %d %s", model, w.Code, w.Body)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("blocked request reached provider")
	}
	if w := callAPI(s, syntheticClientKey, `{"model":"friendly","input":"fixture"}`); w.Code != 200 || calls.Load() != 1 {
		t.Fatalf("allowed prefix alias failed: %d", w.Code)
	}
	s.ModelCatalog = func(context.Context, config.LocalAccount, bool) (protocol.ModelCatalog, error) {
		return protocol.ModelCatalog{Models: []string{"gpt-fixture", "gpt-blocked-exact", "gpt-shadow", "other-model", "other-exact"}}, nil
	}
	w := callModels(s)
	var out struct{ Data []struct{ ID string } }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("catalog: %d %s", w.Code, w.Body)
	}
	ids := []string{}
	for _, row := range out.Data {
		ids = append(ids, row.ID)
	}
	if !reflect.DeepEqual(ids, []string{"gpt-fixture", "other-exact", "friendly"}) {
		t.Fatalf("catalog bypassed resolved prefix rules: %v", ids)
	}
}
