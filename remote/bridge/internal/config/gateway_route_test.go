package config

import "testing"

func TestNeedsGatewayWhenToolMaySwitchToOtherFamilies(t *testing.T) {
	a := LocalAccount{Kind: "codex", Model: "gpt-5", Models: []string{"gpt-5", "grok-4"}, Protocol: "responses"}
	if NeedsGateway(a) {
		t.Fatal("GPT default without picker override goes direct")
	}
	a.CatalogOverride = true
	if !NeedsGateway(a) || !MixedModels(a) {
		t.Fatal("override with Grok in the catalog must use the converting gateway")
	}
	a.Models = []string{"gpt-5", "gpt-5-codex"}
	if NeedsGateway(a) {
		t.Fatal("all-native catalog stays direct")
	}
}

func TestSensitiveSourcesAlwaysUseLeaseBoundGateway(t *testing.T) {
	for _, source := range []string{"reverse_proxy", "oauth", "official_account"} {
		t.Run(source, func(t *testing.T) {
			a := LocalAccount{ID: "fixture", Kind: "codex", SourceType: source, BaseURL: "https://provider.test", Key: "fixture", Model: "gpt-fixture", Protocol: "responses", GatewayLaunchLease: "explicit-apply"}
			if !NeedsGateway(a) {
				t.Fatalf("%s source could bypass the local gateway", source)
			}
			c := Config{LocalAccounts: []LocalAccount{a}, GatewayKey: "persisted", GatewayRuntimeKey: "fresh-process"}
			c.RecordAppliedAPI("codex", a, "fixture")
			connection := c.ToolConnection(a, "codex", 47831)
			if connection.BaseURL != "http://127.0.0.1:47831/gateway/codex" || connection.Key != c.LocalGatewayCredential("codex") || connection.Key == "fresh-process" || connection.Key == "fixture" {
				t.Fatalf("%s source did not receive the current lease-bound route: %+v", source, connection)
			}
		})
	}
}
