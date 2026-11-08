package console

import (
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/hubclient"
	"testing"
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
