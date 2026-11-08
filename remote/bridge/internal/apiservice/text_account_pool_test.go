package apiservice

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/gateway"
)

func enableTextPoolFixture(s *Service, live *liveConfig, ids []string, strategy, single string) {
	live.change(func(c *config.Config) {
		client := &c.CodexAPI.Clients[0]
		client.AccountIDs = append([]string(nil), ids...)
		client.PoolStrategy = strategy
		client.PoolAccountID = single
	})
	s.Pool = gateway.NewAccountPool()
}

func TestNamedTextPoolUsesOnlyExplicitMembersAndKeepsAnchorOut(t *testing.T) {
	s, live := fixtureService(t)
	enableTextPoolFixture(s, live, []string{"account-B"}, config.PoolStrategySingleAccount, "account-B")
	var selected string
	s.Official = func(_ context.Context, account config.LocalAccount, body []byte, stream, compact bool, current func() bool) (*http.Response, error) {
		selected = account.ID
		if !current() || stream || compact {
			t.Fatal("selected text member was not current")
		}
		var input map[string]any
		if json.Unmarshal(body, &input) != nil || input["model"] != "gpt-fixture" {
			t.Fatal("wrong text request")
		}
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	if response := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture","input":"explicit member"}`); response.Code != 200 {
		t.Fatalf("explicit pool request: %d %s", response.Code, response.Body)
	}
	if selected != "account-B" {
		t.Fatalf("anchor was used instead of explicit member: %q", selected)
	}
}

func TestNamedTextPoolModelDirectoryIsExplicitMemberUnion(t *testing.T) {
	s, live := fixtureService(t)
	live.change(func(c *config.Config) {
		c.CodexAPI.Clients[0].AccountIDs = []string{"account-A", "account-B"}
		c.LocalAccounts[0].Models = []string{"model-a", "shared"}
		c.LocalAccounts[0].Model = "model-a"
		c.LocalAccounts[1].Models = []string{"model-b", "shared"}
		c.LocalAccounts[1].Model = "model-b"
	})
	w := callModels(s)
	if w.Code != 200 {
		t.Fatalf("explicit pool models: %d %s", w.Code, w.Body)
	}
	var response struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, model := range response.Data {
		seen[model.ID] = true
	}
	for _, model := range []string{"model-a", "model-b", "shared"} {
		if !seen[model] {
			t.Fatalf("member union omitted %q: %v", model, seen)
		}
	}
}

func TestNamedTextPoolMissingMemberFailsClosed(t *testing.T) {
	s, live := fixtureService(t)
	enableTextPoolFixture(s, live, []string{"account-B", "account-gone"}, config.PoolStrategyAuto, "")
	var calls int
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		calls++
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	if response := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture","input":"must reject"}`); response.Code != 409 {
		t.Fatalf("missing explicit member status: %d %s", response.Code, response.Body)
	}
	if calls != 0 {
		t.Fatal("missing member reached provider")
	}
}

func TestNamedTextPoolQuotaReserveSkipsStaleMember(t *testing.T) {
	s, live := fixtureService(t)
	live.change(func(c *config.Config) {
		c.CodexAPI.Clients[0].AccountIDs = []string{"account-A", "account-B"}
		c.CodexAPI.Clients[0].PoolStrategy = config.PoolStrategyAuto
		c.LocalAccounts[0].QuotaReservePercent = 0
		c.LocalAccounts[1].QuotaReservePercent = 50
	})
	s.Pool = gateway.NewAccountPool()
	s.Pool.SetRoutingProbe(func(ref string, _ uint64, _ string) gateway.PoolRoutingObservation {
		if ref == "credential-B" {
			return gateway.PoolRoutingObservation{Current: true, QuotaFresh: true, QuotaAllowed: false, RemainingPercent: 10}
		}
		return gateway.PoolRoutingObservation{Current: true, QuotaFresh: true, QuotaAllowed: true, RemainingPercent: 90}
	})
	var selected string
	s.Official = func(_ context.Context, account config.LocalAccount, _ []byte, _, _ bool, _ func() bool) (*http.Response, error) {
		selected = account.ID
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	if response := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture","input":"quota"}`); response.Code != 200 {
		t.Fatalf("quota selection: %d %s", response.Code, response.Body)
	}
	if selected != "account-A" {
		t.Fatalf("stale quota member selected: %q", selected)
	}
}

func TestNamedTextPoolWebSocketSelectsMemberPerFrame(t *testing.T) {
	s, live := fixtureService(t)
	enableTextPoolFixture(s, live, []string{"account-B"}, config.PoolStrategySingleAccount, "account-B")
	var mu sync.Mutex
	var selected []string
	s.Official = func(_ context.Context, account config.LocalAccount, body []byte, stream, compact bool, current func() bool) (*http.Response, error) {
		mu.Lock()
		selected = append(selected, account.ID)
		mu.Unlock()
		if !stream || compact || !current() {
			t.Fatal("invalid websocket text execution")
		}
		return successfulResponse(io.NopCloser(strings.NewReader(namedCompleted("text-pool-response"))), true), nil
	}
	address := namedSocketServer(t, s)
	c := namedSocket(t, address, syntheticClientKey)
	sendNamedFrame(t, c, map[string]any{"type": "response.create", "model": "gpt-fixture", "input": "text pool"})
	namedSocketEvent(t, c, "response.completed")
	mu.Lock()
	defer mu.Unlock()
	if len(selected) != 1 || selected[0] != "account-B" {
		t.Fatalf("websocket did not select explicit member: %v", selected)
	}
}

func TestNamedTextPoolWebSocketWarmupUsesMemberUnion(t *testing.T) {
	s, live := fixtureService(t)
	live.change(func(c *config.Config) {
		c.CodexAPI.Clients[0].AccountIDs = []string{"account-B"}
		c.CodexAPI.Clients[0].ModelAliases = nil
		c.LocalAccounts[0].Models = []string{"anchor-only"}
		c.LocalAccounts[0].Model = "anchor-only"
		c.LocalAccounts[1].Models = []string{"member-only"}
		c.LocalAccounts[1].Model = "member-only"
	})
	s.Pool = gateway.NewAccountPool()
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		t.Fatal("warmup must not execute upstream")
		return nil, nil
	}
	c := namedSocket(t, namedSocketServer(t, s), syntheticClientKey)
	sendNamedFrame(t, c, map[string]any{"type": "response.create", "model": "member-only", "generate": false, "input": "warmup"})
	event := namedSocketEvent(t, c, "response.completed")
	response, _ := event["response"].(map[string]any)
	if response["model"] != "member-only" {
		t.Fatalf("warmup did not accept explicit member model: %v", event)
	}
}
