package config

import "testing"

func TestPoolPolicyValidationAndFingerprint(t *testing.T) {
	base := LocalAccount{
		ID: "pool-policy", Name: "Pool policy", Kind: "api", SourceType: "api_key",
		BaseURL: "https://api.fixture.test", Key: "synthetic-key", PoolEnabled: true, PoolGroup: "coding",
	}
	strategies := []string{"", "auto", "random", "single_account", "quota_high_first", "quota_low_first", "plan_high_first", "plan_low_first", "expiry_soon_first", "custom"}
	for _, strategy := range strategies {
		a := base
		a.PoolStrategy = strategy
		if err := ValidateLocalAccount(&a); err != nil {
			t.Fatalf("valid strategy %q rejected: %v", strategy, err)
		}
	}
	invalid := base
	invalid.PoolStrategy = "AUTO"
	if err := ValidateLocalAccount(&invalid); err == nil {
		t.Fatal("unknown/case-confusable pool strategy accepted")
	}

	baseline := AccountFingerprint(base)
	for name, mutate := range map[string]func(*LocalAccount){
		"explicit-auto": func(a *LocalAccount) { a.PoolStrategy = "auto" },
		"backup":        func(a *LocalAccount) { a.PoolBackup = true },
	} {
		a := base
		mutate(&a)
		if AccountFingerprint(a) == baseline {
			t.Fatalf("%s policy did not enter account fingerprint", name)
		}
	}
}

func TestGatewayPoolStrategyChangesFenceOnlyLocalRoute(t *testing.T) {
	for _, change := range []string{"strategy", "backup"} {
		t.Run(change, func(t *testing.T) {
			store := gatewayAuthorizationStore(t, true)
			before := store.Get()
			updateGatewayAuthorization(t, store, func(c *Config) {
				if change == "strategy" {
					c.LocalAccounts[0].PoolStrategy = "single_account"
				} else {
					c.LocalAccounts[1].PoolBackup = true
				}
			})
			after := store.Get()
			if after.GatewayAuthorizationRevision("codex") == before.GatewayAuthorizationRevision("codex") {
				t.Fatal("pool policy change did not fence local route")
			}
			// A policy on an unrelated pool member cannot affect the fixed remote
			// source. Changing the applied source itself may conservatively fence
			// that fixed route because its card fingerprint changed.
			if change == "backup" && after.GatewayAuthorizationRevision("remote-codex") != before.GatewayAuthorizationRevision("remote-codex") {
				t.Fatal("fixed remote route inherited local pool policy")
			}
		})
	}
}
