package config

import (
	"fmt"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

func TestStationV2SourceClassesHaveIndependentScopes(t *testing.T) {
	accounts := []LocalAccount{
		{SourceType: "api_key", BaseURL: "https://own.test/v1"},
		{SourceType: "api_key", BaseURL: "https://external.test/v1"},
		{SourceType: "reverse_proxy", BaseURL: "https://proxy.test/v1"},
		{SourceType: "official_account", ProviderID: "openai", CredentialRef: "protected", BaseURL: "https://chatgpt.com/backend-api/codex", Wire: "responses"},
	}
	policies := []protocol.StationPolicy{
		{Version: 2, APIBases: []string{"https://own.test"}},
		{Version: 2, ExternalAPIBases: []string{"https://external.test"}},
		{Version: 2, ReverseProxyAPIBases: []string{"https://proxy.test"}},
		{Version: 2, AllowOfficialAccounts: true},
	}
	for i := range policies {
		for j := range accounts {
			if got := StationAllowsAccount(&policies[i], accounts[j]); got != (i == j) {
				t.Errorf("policy %d account %d allowed=%v", i, j, got)
			}
		}
	}
	for _, source := range []string{"api_key", "reverse_proxy"} {
		account := LocalAccount{SourceType: source, BaseURL: "https://shared.test/v1"}
		api := &protocol.StationPolicy{Version: 2, APIBases: []string{"https://shared.test"}}
		proxy := &protocol.StationPolicy{Version: 2, ReverseProxyAPIBases: []string{"https://shared.test"}}
		if StationAllowsAccount(api, account) != (source == "api_key") || StationAllowsAccount(proxy, account) != (source == "reverse_proxy") {
			t.Fatal("shared address bypassed source class isolation")
		}
		legacy := &protocol.StationPolicy{Version: 1, APIBases: []string{"https://shared.test"}}
		if !StationAllowsAccount(legacy, account) {
			t.Fatal("v1 source compatibility regressed")
		}
	}
	for _, account := range []LocalAccount{
		{SourceType: "oauth", BaseURL: "https://chatgpt.com/backend-api/codex"},
		{SourceType: "official_account", ProviderID: "openai", BaseURL: "https://chatgpt.com/backend-api/codex", Wire: "responses"},
		{SourceType: "api_key", CredentialRef: "protected", BaseURL: "https://own.test"},
	} {
		for _, policy := range policies {
			if StationAllowsAccount(&policy, account) {
				t.Fatal("unprotected or misclassified account accepted")
			}
		}
	}
}

func TestStationV2ValidationAndFullPathBoundaries(t *testing.T) {
	base := "https://api.test/Team"
	valid := &protocol.StationPolicy{Version: 2, APIBases: []string{base}, ExternalAPIBases: []string{"https://external.test"}, ReverseProxyAPIBases: []string{base}}
	if !ValidStationPolicy(valid) {
		t.Fatal("same address in API and proxy classes should be permitted")
	}
	migrated := &protocol.StationPolicy{Version: 2}
	for i := 0; i < 32; i++ {
		base := fmt.Sprintf("https://migrated%d.test", i)
		migrated.APIBases = append(migrated.APIBases, base)
		migrated.ReverseProxyAPIBases = append(migrated.ReverseProxyAPIBases, base)
	}
	if !ValidStationPolicy(migrated) {
		t.Fatal("v1 migration cannot preserve 32 shared API/proxy scopes")
	}
	legacyEmptyExtension := &protocol.StationPolicy{Version: 1, ExternalAPIBases: []string{}}
	if ValidStationPolicy(legacyEmptyExtension) || ValidStationPolicy(CloneStationPolicy(legacyEmptyExtension)) {
		t.Fatal("v1 empty v2 field became valid during clone")
	}
	for _, edit := range []func(*protocol.StationPolicy){
		func(p *protocol.StationPolicy) { p.AllowAllAccounts = true },
		func(p *protocol.StationPolicy) { p.Version = 1 },
		func(p *protocol.StationPolicy) { p.Version = 3 },
		func(p *protocol.StationPolicy) { p.ExternalAPIBases = []string{base} },
		func(p *protocol.StationPolicy) { p.ReverseProxyAPIBases = []string{base, base} },
		func(p *protocol.StationPolicy) { p.ExternalAPIBases = []string{"https://API.test:443/v1"} },
		func(p *protocol.StationPolicy) { p.ExternalAPIBases = []string{"https://api.test/Team/../other"} },
		func(p *protocol.StationPolicy) {
			for i := 0; i < 31; i++ {
				p.ExternalAPIBases = append(p.ExternalAPIBases, fmt.Sprintf("https://external%d.test", i))
			}
		},
	} {
		policy := CloneStationPolicy(valid)
		edit(policy)
		if ValidStationPolicy(policy) {
			t.Fatalf("invalid policy accepted: %+v", policy)
		}
	}
	for _, scope := range []string{"external", "proxy"} {
		policy := &protocol.StationPolicy{Version: 2}
		source := "api_key"
		if scope == "proxy" {
			policy.ReverseProxyAPIBases, source = []string{base}, "reverse_proxy"
		} else {
			policy.ExternalAPIBases = []string{base}
		}
		if !StationAllowsAccount(policy, LocalAccount{SourceType: source, BaseURL: "https://API.test:443/Team/v1/"}) {
			t.Fatal("canonical API form rejected")
		}
		for _, address := range []string{"https://api.test/team", "https://api.test/Team/child", "https://api.test/Team/../other", "https://api.test/Team%2fchild", "https://api.test/Team?x=1", "https://api.test.evil/Team"} {
			if StationAllowsAccount(policy, LocalAccount{SourceType: source, BaseURL: address}) {
				t.Errorf("path bypass accepted: %s", address)
			}
		}
	}
}

func TestStationV2ScopeChangesRevokeAuthorityAndWorkerLease(t *testing.T) {
	for _, scope := range []string{"external", "proxy", "official"} {
		t.Run(scope, func(t *testing.T) {
			store := gatewayAuthorizationStore(t, false)
			cfg := store.Get()
			policy := &protocol.StationPolicy{Version: 2, APIBases: []string{"https://api.fixture.test"}, ExternalAPIBases: []string{"https://external.test"}, ReverseProxyAPIBases: []string{"https://proxy.test"}, AllowOfficialAccounts: true}
			if err := store.BindRemoteStation(RemoteIdentity(cfg), "r2", policy, nil); err != nil {
				t.Fatal(err)
			}
			cfg = store.Get()
			if cfg.RemoteGatewayCredential("remote-codex") == "" {
				t.Fatal("valid worker missing lease")
			}
			copy := cfg.Clone()
			copy.RemoteStationAuthorization.Station.ExternalAPIBases[0] = "https://clone.test"
			copy.RemoteStationAuthorization.Station.ReverseProxyAPIBases[0] = "https://clone.test"
			if !StationPolicyEqual(&cfg.RemoteStationAuthorization.Station, policy) {
				t.Fatal("cloned policy aliases authorization")
			}
			observation := &StationObservation{Mode: "open", Revision: "r2", Station: CloneStationPolicy(policy), CheckedAt: time.Now().UnixMilli()}
			switch scope {
			case "external":
				observation.Station.ExternalAPIBases = nil
			case "proxy":
				observation.Station.ReverseProxyAPIBases = nil
			case "official":
				observation.Station.AllowOfficialAccounts = false
			}
			store.ObserveRemoteStation(cfg, observation, store.BeginRemoteStationObservation(cfg))
			if got := store.Get(); got.RemoteStationAuthorization != nil || got.RemoteGatewayCredential("remote-codex") != "" {
				t.Fatal("changed scope retained old worker authority")
			}
		})
	}
}
