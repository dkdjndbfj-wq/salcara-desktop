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

func TestClientModelPermissionsApplyToRequestsAndDiscovery(t *testing.T) {
	s, live := fixtureService(t)
	live.change(func(c *config.Config) {
		c.CodexAPI.Clients[0].ModelAllowlist = []string{"gpt-fixture", "denied", "native-shadow"}
		c.CodexAPI.Clients[0].ModelDenylist = []string{"denied"}
		c.CodexAPI.Clients[0].ModelAliases["blocked-alias"] = "denied"
		// A colliding alias must also shadow the native name in discovery.
		c.CodexAPI.Clients[0].ModelAliases["native-shadow"] = "denied"
	})
	var calls atomic.Int32
	s.Official = func(_ context.Context, _ config.LocalAccount, body []byte, _, _ bool, _ func() bool) (*http.Response, error) {
		calls.Add(1)
		var input map[string]any
		_ = json.Unmarshal(body, &input)
		if input["model"] != "gpt-fixture" {
			t.Error("forbidden model reached upstream")
		}
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	for _, model := range []string{"denied", "blocked-alias", "unlisted", "native-shadow"} {
		body, _ := json.Marshal(map[string]string{"model": model, "input": "fixture"})
		if w := callAPI(s, syntheticClientKey, string(body)); w.Code != 403 {
			t.Fatalf("model %q bypassed client policy: %d %s", model, w.Code, w.Body)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("rejected model reached upstream")
	}
	if w := callAPI(s, syntheticClientKey, `{"model":"friendly","input":"fixture"}`); w.Code != 200 || calls.Load() != 1 {
		t.Fatalf("permitted alias failed: %d %s", w.Code, w.Body)
	}
	s.ModelCatalog = func(context.Context, config.LocalAccount, bool) (protocol.ModelCatalog, error) {
		return protocol.ModelCatalog{Models: []string{"gpt-fixture", "denied", "unlisted", "native-shadow"}}, nil
	}
	w := callModels(s)
	var out struct{ Data []struct{ ID string } }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("catalogue failed: %d %s", w.Code, w.Body)
	}
	ids := []string{}
	for _, row := range out.Data {
		ids = append(ids, row.ID)
	}
	if !reflect.DeepEqual(ids, []string{"gpt-fixture", "friendly"}) {
		t.Fatalf("discovery and forwarding permissions differ: %v", ids)
	}
}

func TestDisabledClientCannotUseEitherAPISurfaceAndCanBeReenabled(t *testing.T) {
	s, live := fixtureService(t)
	live.change(func(c *config.Config) { c.CodexAPI.Clients[0].Disabled = true })
	s.ModelCatalog = func(context.Context, config.LocalAccount, bool) (protocol.ModelCatalog, error) {
		t.Fatal("disabled client read model catalogue")
		return protocol.ModelCatalog{}, nil
	}
	if w := callModels(s); w.Code != 401 {
		t.Fatalf("disabled client models status: %d", w.Code)
	}
	if w := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture"}`); w.Code != 401 {
		t.Fatalf("disabled client request status: %d", w.Code)
	}
	live.change(func(c *config.Config) { c.CodexAPI.Clients[0].Disabled = false })
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	if w := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture"}`); w.Code != 200 {
		t.Fatalf("reenabled key failed: %d %s", w.Code, w.Body)
	}
}
