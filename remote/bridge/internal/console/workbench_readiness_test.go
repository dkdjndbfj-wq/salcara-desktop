package console

import (
	"reflect"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/hubclient"
)

func TestConnectedClaudeLampRequiresWorkerAPI(t *testing.T) {
	c, tools := workbenchConfig()
	c.RemoteDeviceOnly = true
	if lamp := toolBindings(c, tools, hubclient.StateConnected)["claude"].Remote; lamp.State != "pending" {
		t.Fatalf("missing API advertised ready: %+v", lamp)
	}
	c.RemoteAPI = map[string]config.RemoteAPI{"claude": {AccountID: "cl", Model: "model"}}
	if lamp := toolBindings(c, tools, hubclient.StateConnected)["claude"].Remote; lamp.State != "connected" {
		t.Fatalf("selected API not ready: %+v", lamp)
	}
	c.GatewayRuntimeKey, c.RemoteExecutionArmed = "fixture-process", true
	if lamp := toolBindings(c, tools, hubclient.StateConnected)["claude"].Remote; lamp.State != "pending" {
		t.Fatalf("unleased worker advertised ready: %+v", lamp)
	}
}

func TestAgentReadinessIsIndependentOfRemoteErrors(t *testing.T) {
	c, tools := workbenchConfig()
	c.RecordAppliedAPI("codex", c.LocalAccounts[0], "responses")
	want := toolBindings(c, tools, hubclient.StateConnected)["codex"].Agent
	if want.State != "ready" || want.Label != "API 已应用" {
		t.Fatalf("applied API not ready: %+v", want)
	}
	for _, state := range []string{hubclient.StateNotLoggedIn, hubclient.StateConnecting, hubclient.StateInvalidKey, hubclient.StateDeviceAuthRequired, hubclient.StateSourceUnauthorized, hubclient.StatePairRequired, hubclient.StateRemoteBlocked} {
		if got := toolBindings(c, tools, state)["codex"].Agent; !reflect.DeepEqual(got, want) {
			t.Fatalf("remote %s replaced local readiness: %+v", state, got)
		}
	}
	c.ActiveCodexAccount = ""
	c.ToolAPISelections["codex"] = ""
	if got := toolBindings(c, tools, hubclient.StateConnected)["codex"].Agent; got.State != "pending" {
		t.Fatalf("missing API advertised applied: %+v", got)
	}
	tools[1].Available = false
	if got := toolBindings(c, tools, hubclient.StateConnected)["codex"].Agent; got.Supported || got.State != "unsupported" {
		t.Fatalf("missing Agent advertised installed: %+v", got)
	}
}
