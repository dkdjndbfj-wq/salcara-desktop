package config

import "testing"

func TestAccountSpeedValidationAndLegacyFingerprint(t *testing.T) {
	a := LocalAccount{ID: "speed-fixture", Name: "speed", Kind: "api", BaseURL: "https://example.test", Key: "synthetic-key"}
	if err := ValidateLocalAccount(&a); err != nil {
		t.Fatal(err)
	}
	legacy := AccountFingerprint(a)
	for _, mode := range []string{"", "standard", "fast", "ultrafast"} {
		a.SpeedMode = mode
		if err := ValidateLocalAccount(&a); err != nil {
			t.Fatalf("valid mode %q rejected: %v", mode, err)
		}
		if (AccountFingerprint(a) == legacy) != (mode == "" || mode == "standard") {
			t.Fatal("speed policy did not preserve legacy standard or fence an elevated tier")
		}
	}
	for _, mode := range []string{"ultra", "priority", "FAST", " fast ", "fast\n"} {
		a.SpeedMode = mode
		if ValidateLocalAccount(&a) == nil {
			t.Fatalf("invalid mode %q accepted", mode)
		}
	}
}

func TestAccountSpeedRequiresGatewayAndFreshApplication(t *testing.T) {
	a := LocalAccount{ID: "speed-fixture", Name: "speed", Kind: "api", BaseURL: "https://example.test", Key: "synthetic-key", Model: "gpt-fixture", SpeedMode: "fast"}
	c := Config{LocalAccounts: []LocalAccount{a}, GatewayRuntimeKey: "synthetic-runtime", GatewayKey: "synthetic-loopback"}
	tool := c.APIForTool(a, "codex-desktop")
	tool.GatewayLaunchLease = "synthetic-lease"
	c.RecordAppliedAPI("codex-desktop", tool, "openai")
	connection := c.ToolConnection(tool, "codex-desktop", 12222)
	if connection.BaseURL != "http://127.0.0.1:12222/gateway/codex" || connection.Key == a.Key || connection.SpeedMode != "fast" || !NeedsGateway(tool) {
		t.Fatal("native launch bypassed the tier-aware gateway")
	}
	if _, ok := c.AuthorizedAppliedToolAccount("codex-desktop"); !ok {
		t.Fatal("initial application missing")
	}
	c.LocalAccounts[0].SpeedMode = "ultrafast"
	if _, ok := c.AuthorizedAppliedToolAccount("codex-desktop"); ok {
		t.Fatal("old native connection silently followed a more expensive tier")
	}
	c.retireInvalidAppliedLaunches()
	c.LocalAccounts[0].SpeedMode = "fast"
	if _, ok := c.AuthorizedAppliedToolAccount("codex-desktop"); ok {
		t.Fatal("restoring an old tier revived its retired launch lease")
	}
}
