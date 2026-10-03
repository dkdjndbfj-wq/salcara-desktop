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
