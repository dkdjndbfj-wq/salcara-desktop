package config

import (
	"reflect"
	"strings"
	"testing"
)

func directAPIStore(t *testing.T) *Store {
	t.Helper()
	s := gatewayAuthorizationStore(t, true)
	updateGatewayAuthorization(t, s, func(c *Config) {
		c.DirectRelayEnabled, c.DirectPhoneID, c.DirectPhoneHash = true, "direct-phone", "synthetic-direct-binding"
		c.RemoteAPI["codex"] = RemoteAPI{AccountID: "account-B", Model: "gpt-fixture"}
		c.DirectAPI = map[string]RemoteAPI{"codex": {AccountID: "account-C", Model: "gpt-fixture"}}
	})
	return s
}

func TestDirectAPISelectionIsIndependentOfHubAndNativeConfiguration(t *testing.T) {
	s := directAPIStore(t)
	before := s.Get()
	for target, want := range map[string]string{"codex": "account-A", "remote-codex": "account-B", "direct-codex": "account-C"} {
		a, ok := before.GatewaySourceAccount(target)
		if !ok || a.ID != want {
			t.Fatalf("%s source = %s, available=%v", target, a.ID, ok)
		}
	}
	if key := before.DirectGatewayCredential("direct-codex"); key == "" || key == before.RemoteGatewayCredential("remote-codex") || key == before.LocalGatewayCredential("codex") {
		t.Fatal("direct credential was empty or shared with another control surface")
	}
	updateGatewayAuthorization(t, s, func(c *Config) {
		if err := c.SetDirectAPIForAgent("codex", "account-B", "direct-model"); err != nil {
			t.Fatal(err)
		}
	})
	after := s.Get()
	if !reflect.DeepEqual(before.RemoteAPI, after.RemoteAPI) || !reflect.DeepEqual(before.ToolAPIApplied, after.ToolAPIApplied) || !reflect.DeepEqual(before.ToolAPISelections, after.ToolAPISelections) {
		t.Fatal("direct choice changed Hub or native tool state")
	}
	if before.RemoteGatewayCredential("remote-codex") != after.RemoteGatewayCredential("remote-codex") || before.LocalGatewayCredential("codex") != after.LocalGatewayCredential("codex") {
		t.Fatal("direct choice invalidated another surface's credential")
	}
	clone := after.Clone()
	clone.DirectAPI["codex"] = RemoteAPI{AccountID: "account-A"}
	if reflect.DeepEqual(clone.DirectAPI, after.DirectAPI) {
		t.Fatal("direct choice map aliases its source snapshot")
	}
	updateGatewayAuthorization(t, s, func(c *Config) { _ = c.SetDirectAPIForAgent("codex", "", "") })
	if _, ok := s.Get().GatewaySourceAccount("direct-codex"); ok || s.Get().DirectGatewayCredential("direct-codex") != "" {
		t.Fatal("cleared direct route inherited the Hub or applied source")
	}
}

func TestDirectGatewayLeaseCannotReviveAfterSwitchRevokeOrRestart(t *testing.T) {
	for _, family := range []string{"codex", "claude", "deepseek-harness", "hermes"} {
		t.Run(family, func(t *testing.T) {
			s := directAPIStore(t)
			set := func(id, model string) {
				updateGatewayAuthorization(t, s, func(c *Config) {
					if err := c.SetDirectAPIForAgent(family, id, model); err != nil {
						t.Fatal(err)
					}
				})
			}
			target := DirectGatewayTarget(family)
			set("account-A", "model-A")
			first := s.Get().DirectGatewayCredential(target)
			if first == "" {
				t.Fatal("selected direct route has no credential")
			}
			set("account-B", "model-B")
			second := s.Get().DirectGatewayCredential(target)
			set("account-A", "model-A")
			third := s.Get().DirectGatewayCredential(target)
			if second == first || third == first || third == second {
				t.Fatal("A/B/A switch revived a retired credential")
			}
			set("account-A", "different-model")
			if s.Get().DirectGatewayCredential(target) == third {
				t.Fatal("selected model was not bound into the lease")
			}
			beforeRevoke := s.Get().DirectGatewayCredential(target)
			updateGatewayAuthorization(t, s, func(c *Config) { c.DirectRelayEnabled = false })
			if s.Get().DirectGatewayCredential(target) != "" {
				t.Fatal("disabled pairing retained a usable credential")
			}
			updateGatewayAuthorization(t, s, func(c *Config) { c.DirectRelayEnabled = true })
			if s.Get().DirectGatewayCredential(target) == beforeRevoke {
				t.Fatal("restored pairing revived a retired credential")
			}
			beforeRestart := s.Get().DirectGatewayCredential(target)
			reopened, err := Open(s.Path())
			if err != nil {
				t.Fatal(err)
			}
			if reopened.Get().DirectGatewayCredential(target) != "" {
				t.Fatal("persisted config retained a runtime lease")
			}
			reopened.SetRuntimeGatewayKey("synthetic-next-runtime")
			if key := reopened.Get().DirectGatewayCredential(target); key == "" || key == beforeRestart {
				t.Fatal("new Bridge runtime did not rotate its direct credential")
			}
		})
	}
}

func TestDirectGatewayBindingAndVaultMutationsFenceOnlyDirectAuthority(t *testing.T) {
	for _, mutation := range []string{"phone-id", "phone-secret", "vault-key", "model-policy", "runtime", "epoch"} {
		t.Run(mutation, func(t *testing.T) {
			s := directAPIStore(t)
			before := s.Get()
			updateGatewayAuthorization(t, s, func(c *Config) {
				switch mutation {
				case "phone-id":
					c.DirectPhoneID = "another-phone"
				case "phone-secret":
					c.DirectPhoneHash = "rotated-binding"
				case "vault-key":
					c.LocalAccounts[2].Key = "rotated-key"
				case "model-policy":
					c.LocalAccounts[2].ModelDenylist = []string{"gpt-fixture"}
				case "runtime":
					c.GatewayRuntimeKey = "another-runtime"
				case "epoch":
					c.DirectRelayEnabled = false
				}
			})
			if s.Get().DirectGatewayCredential("direct-codex") == before.DirectGatewayCredential("direct-codex") {
				t.Fatal("changed direct authority reused the old credential")
			}
			updateGatewayAuthorization(t, s, func(c *Config) {
				c.DirectPhoneID, c.DirectPhoneHash, c.DirectRelayEnabled = before.DirectPhoneID, before.DirectPhoneHash, before.DirectRelayEnabled
				c.LocalAccounts[2], c.GatewayRuntimeKey = before.LocalAccounts[2], before.GatewayRuntimeKey
			})
			if s.Get().DirectGatewayCredential("direct-codex") == before.DirectGatewayCredential("direct-codex") {
				t.Fatal("restored authority revived the original credential")
			}
		})
	}
	for _, mutation := range []func(*Config){func(c *Config) { c.DirectPhoneHash = "" }, func(c *Config) { c.DirectPhoneID = "" }, func(c *Config) { c.GatewayAuthEpochs = nil }, func(c *Config) { c.LocalAccounts = nil }} {
		c := directAPIStore(t).Get()
		mutation(&c)
		if c.DirectGatewayCredential("direct-codex") != "" {
			t.Fatal("incomplete direct authority produced a credential")
		}
	}
	c := directAPIStore(t).Get()
	for _, target := range []string{"codex", "remote-codex", "direct-remote-codex", "direct-claude-desktop", "direct-shell"} {
		if c.DirectGatewayCredential(target) != "" {
			t.Fatalf("unsupported direct route %q got a credential", target)
		}
	}
	for _, model := range []string{strings.Repeat("x", 201), "model\ninvalid"} {
		if c.SetDirectAPIForAgent("codex", "account-A", model) == nil {
			t.Fatal("invalid direct model accepted")
		}
	}
}
