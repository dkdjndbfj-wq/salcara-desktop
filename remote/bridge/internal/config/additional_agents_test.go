package config

import (
	"strings"
	"testing"
)

func TestAdditionalAgentsKeepIndependentLeases(t *testing.T) {
	a := LocalAccount{ID: "shared", Kind: "api", Name: "Shared", Model: "deepseek-chat", Key: "synthetic-key", BaseURL: "https://example.test", Wire: "chat"}
	c := Config{GatewayRuntimeKey: "synthetic-runtime", LocalAccounts: []LocalAccount{a}, ActiveCodexAccount: a.ID, ActiveClaudeAccount: a.ID}
	for _, kind := range []string{"deepseek-harness", "hermes"} {
		if _, ok := c.SelectedToolAccount(kind); ok {
			t.Fatal("implicitly selected legacy key")
		}
		adapted := c.APIForTool(a, kind)
		if adapted.Kind != kind || NativeProtocol(kind) != "chat" {
			t.Fatal("misclassified hyphenated family")
		}
		adapted.GatewayLaunchLease = "test-lease-" + kind
		c.RecordAppliedAPI(kind, adapted, "")
		connection := c.ToolConnection(adapted, kind, 47831)
		if !strings.HasSuffix(connection.BaseURL, "/gateway/"+kind) || connection.Key == a.Key || connection.Key == "" || c.LocalGatewayCredential(kind) != connection.Key {
			t.Fatal("invalid leased connection")
		}
	}
	if c.LocalGatewayCredential("hermes") == c.LocalGatewayCredential("deepseek-harness") {
		t.Fatal("route keys are interchangeable")
	}
}
