package console

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func usageMaintenanceConfig() config.Config {
	return config.Config{
		GatewayKey: "synthetic-key", GatewayRuntimeKey: "synthetic-run",
		LocalAccounts: []config.LocalAccount{
			{ID: "primary", Kind: "api", SourceType: "official_account", CredentialRef: "vault-primary", CredentialEpoch: 4, Model: "gpt-fixture", PoolGroup: "group", PoolEnabled: true},
			{ID: "member", Kind: "api", SourceType: "official_account", CredentialRef: "vault-member", CredentialEpoch: 6, Model: "gpt-fixture", PoolGroup: "group", PoolEnabled: false},
			{ID: "image", Kind: "api", SourceType: "official_account", CredentialRef: "vault-image", CredentialEpoch: 8, Model: "gpt-fixture", PoolGroup: "elsewhere", PoolEnabled: true},
		},
		CodexAPI: config.CodexAPIService{Enabled: true, RuntimeLease: "synthetic-run", Clients: []config.NamedAPIClient{
			{ID: "client", Name: "Synthetic client", AccountID: "primary", KeyDigest: config.APIClientDigest("synthetic-client")},
		}},
	}
}

func TestOfficialUsageMaintenanceSelectsOnlyActiveQuotaConsumers(t *testing.T) {
	tests := []struct {
		name string
		edit func(*config.Config)
		want []string
	}{
		{name: "fixed-without-reserve"},
		{name: "stopped", edit: func(c *config.Config) { c.LocalAccounts[0].QuotaReservePercent = 5; c.CodexAPI.Enabled = false }},
		{name: "restart-without-runtime-lease", edit: func(c *config.Config) { c.LocalAccounts[0].QuotaReservePercent = 5; c.CodexAPI.RuntimeLease = "" }},
		{name: "fixed-with-reserve", edit: func(c *config.Config) { c.LocalAccounts[0].QuotaReservePercent = 5 }, want: []string{"vault-primary"}},
		{name: "disabled-client", edit: func(c *config.Config) {
			c.LocalAccounts[0].QuotaReservePercent = 5
			c.CodexAPI.Clients[0].Disabled = true
		}},
		{name: "invalid-client", edit: func(c *config.Config) {
			c.LocalAccounts[0].QuotaReservePercent = 5
			c.CodexAPI.Clients[0].KeyDigest = "invalid"
		}},
		{name: "named-auto-default-explicit-members", edit: func(c *config.Config) {
			c.LocalAccounts[0].QuotaReservePercent = 5
			c.CodexAPI.Clients[0].AccountIDs = []string{"member"}
		}, want: []string{"vault-member"}},
		{name: "named-random-does-not-use-metrics", edit: func(c *config.Config) {
			c.CodexAPI.Clients[0].AccountIDs, c.CodexAPI.Clients[0].PoolStrategy = []string{"member"}, config.PoolStrategyRandom
		}},
		{name: "named-plan-routing", edit: func(c *config.Config) {
			c.CodexAPI.Clients[0].AccountIDs, c.CodexAPI.Clients[0].PoolStrategy = []string{"member"}, config.PoolStrategyPlanHighFirst
		}, want: []string{"vault-member"}},
		{name: "named-single-only-selected", edit: func(c *config.Config) {
			c.LocalAccounts[0].QuotaReservePercent, c.LocalAccounts[1].QuotaReservePercent = 5, 5
			client := &c.CodexAPI.Clients[0]
			client.AccountIDs, client.PoolStrategy, client.PoolAccountID = []string{"primary", "member"}, config.PoolStrategySingleAccount, "member"
		}, want: []string{"vault-member"}},
		{name: "broken-named-members-do-not-poll", edit: func(c *config.Config) { c.CodexAPI.Clients[0].AccountIDs = []string{"member", "removed"} }},
		{name: "text-and-image-pools-independent", edit: func(c *config.Config) {
			c.CodexAPI.ImagesMode = "enabled"
			c.LocalAccounts[2].QuotaReservePercent = 5
			c.CodexAPI.Clients[0].AccountIDs, c.CodexAPI.Clients[0].ImageAccountIDs = []string{"member"}, []string{"image"}
		}, want: []string{"vault-image", "vault-member"}},
		{name: "images-fallback-includes-primary-outside-text-pool", edit: func(c *config.Config) {
			c.CodexAPI.ImagesMode, c.LocalAccounts[0].QuotaReservePercent = "enabled", 5
			c.CodexAPI.Clients[0].AccountIDs = []string{"member"}
		}, want: []string{"vault-member", "vault-primary"}},
		{name: "images-only-does-not-poll-text-members", edit: func(c *config.Config) {
			c.CodexAPI.ImagesMode, c.LocalAccounts[2].QuotaReservePercent = "images_only", 5
			c.CodexAPI.Clients[0].AccountIDs, c.CodexAPI.Clients[0].ImageAccountIDs = []string{"member"}, []string{"image"}
		}, want: []string{"vault-image"}},
		{name: "disabled-images-do-not-poll-image-members", edit: func(c *config.Config) {
			c.LocalAccounts[2].QuotaReservePercent = 5
			c.CodexAPI.Clients[0].ImageAccountIDs = []string{"image"}
		}},
		{name: "pending-local-selection-is-not-active", edit: func(c *config.Config) {
			c.CodexAPI.Enabled = false
			c.LocalAccounts[0].QuotaReservePercent = 5
			c.ToolAPISelections = map[string]string{"codex": "primary"}
		}},
		{name: "local-reserve-independent-of-api-service", edit: func(c *config.Config) {
			c.CodexAPI.Enabled = false
			c.LocalAccounts[0].QuotaReservePercent, c.LocalAccounts[0].PoolEnabled = 5, false
			c.RecordAppliedAPI("codex", c.APIForTool(c.LocalAccounts[0], "codex"), "synthetic")
		}, want: []string{"vault-primary"}},
		{name: "local-lease-revocation", edit: func(c *config.Config) {
			c.CodexAPI.Enabled = false
			c.LocalAccounts[0].QuotaReservePercent = 5
			c.RecordAppliedAPI("codex", c.APIForTool(c.LocalAccounts[0], "codex"), "synthetic")
			binding := c.ToolAPIApplied["codex"]
			binding.LaunchLease = ""
			c.ToolAPIApplied["codex"] = binding
		}},
		{name: "local-pool-membership-and-group", edit: func(c *config.Config) {
			c.CodexAPI.Enabled = false
			c.LocalAccounts[0].PoolStrategy, c.LocalAccounts[1].PoolEnabled = config.PoolStrategyQuotaHighFirst, true
			c.RecordAppliedAPI("claude-desktop", c.APIForTool(c.LocalAccounts[0], "claude-desktop"), "synthetic")
		}, want: []string{"vault-member", "vault-primary"}},
		{name: "local-single-account-ignores-other-reserve-members", edit: func(c *config.Config) {
			c.CodexAPI.Enabled = false
			c.LocalAccounts[0].PoolStrategy, c.LocalAccounts[1].PoolEnabled = config.PoolStrategySingleAccount, true
			c.LocalAccounts[0].QuotaReservePercent, c.LocalAccounts[1].QuotaReservePercent = 5, 5
			c.RecordAppliedAPI("codex-desktop", c.APIForTool(c.LocalAccounts[0], "codex-desktop"), "synthetic")
		}, want: []string{"vault-primary"}},
		{name: "remote-armed-exact-source-only", edit: func(c *config.Config) {
			c.CodexAPI.Enabled, c.RemoteExecutionArmed = false, true
			c.LocalAccounts[0].QuotaReservePercent, c.LocalAccounts[1].QuotaReservePercent = 5, 5
			c.LocalAccounts[0].PoolStrategy, c.LocalAccounts[1].PoolEnabled = config.PoolStrategyAuto, true
			c.RemoteAPI = map[string]config.RemoteAPI{"codex": {AccountID: "primary", Model: "gpt-fixture"}}
			c.LocalAccounts[0].ProviderID, c.LocalAccounts[0].BaseURL, c.LocalAccounts[0].Wire = "openai", "https://chatgpt.com/backend-api/codex", "responses"
			c.RemoteStationAuthorization = &config.StationAuthorization{Identity: config.RemoteIdentity(*c), Revision: "current", Station: protocol.StationPolicy{Version: 1, AllowOfficialAccounts: true}}
		}, want: []string{"vault-primary"}},
		{name: "remote-station-not-authorized", edit: func(c *config.Config) {
			c.CodexAPI.Enabled, c.RemoteExecutionArmed = false, true
			c.LocalAccounts[0].QuotaReservePercent = 5
			c.RemoteAPI = map[string]config.RemoteAPI{"codex": {AccountID: "primary", Model: "gpt-fixture"}}
		}},
		{name: "remote-unarmed-fallback-key-not-active", edit: func(c *config.Config) {
			c.CodexAPI.Enabled, c.LocalAccounts[0].QuotaReservePercent = false, 5
			c.RemoteAPI = map[string]config.RemoteAPI{"codex": {AccountID: "primary", Model: "gpt-fixture"}}
		}},
		{name: "one-vault-through-several-routes-deduplicated", edit: func(c *config.Config) {
			c.LocalAccounts[0].QuotaReservePercent, c.LocalAccounts[0].PoolEnabled = 5, false
			c.RecordAppliedAPI("codex", c.APIForTool(c.LocalAccounts[0], "codex"), "synthetic")
			c.RemoteExecutionArmed = true
			c.RemoteAPI = map[string]config.RemoteAPI{"claude": {AccountID: "primary", Model: "gpt-fixture"}}
		}, want: []string{"vault-primary"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := usageMaintenanceConfig()
			if tc.edit != nil {
				tc.edit(&c)
			}
			var ids []string
			for _, target := range officialUsageRefreshTargets(c) {
				ids = append(ids, target.ID)
			}
			if !reflect.DeepEqual(ids, tc.want) {
				t.Fatalf("maintained vault IDs = %v; want %v", ids, tc.want)
			}
		})
	}
}

func TestOfficialUsageMaintenanceFollowsLiveConfigAndConsoleClose(t *testing.T) {
	started := make(chan context.Context, 10)
	accounts, err := codexaccounts.New(codexaccounts.Options{Path: filepath.Join(t.TempDir(), "synthetic.vault"), Protector: consoleProtector{},
		UsageTimeout: time.Second, UsageFreshFor: 30 * time.Millisecond,
		UsageFetcher: codexaccounts.UsageFetchFunc(func(ctx context.Context, _ *coreauth.Auth) (codexaccounts.UsageFetchResult, error) {
			started <- ctx
			<-ctx.Done()
			return codexaccounts.UsageFetchResult{}, ctx.Err()
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(accounts.Close)
	prepared, err := codexaccounts.ParseImport([]byte(officialFixture))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := accounts.Import(context.Background(), prepared, "")
	if err != nil {
		t.Fatal(err)
	}
	store, err := config.Open(filepath.Join(t.TempDir(), "synthetic.json"))
	if err != nil {
		t.Fatal(err)
	}
	store.SetRuntimeGatewayKey("synthetic-run")
	if err := store.Update(func(c *config.Config) error {
		c.LocalAccounts = []config.LocalAccount{{ID: "primary", Kind: "api", SourceType: "official_account", CredentialRef: rows[0].ID, CredentialEpoch: rows[0].IdentityEpoch, QuotaReservePercent: 5}}
		c.CodexAPI = usageMaintenanceConfig().CodexAPI
		c.CodexAPI.Enabled = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s := New(Deps{Store: store, CodexAccounts: accounts, Dir: t.TempDir()})
	t.Cleanup(s.CloseAPIListeners)
	t.Cleanup(s.CloseUsageLedger)
	if err := store.Update(func(c *config.Config) error {
		c.CodexAPI.Enabled = true
		c.CodexAPI.RuntimeLease = c.GatewayRuntimeKey
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var fetchCtx context.Context
	select {
	case fetchCtx = <-started:
	case <-time.After(time.Second):
		t.Fatal("starting the active service did not schedule quota maintenance")
	}
	if err := store.Update(func(c *config.Config) error { c.CodexAPI.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fetchCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("stopping the service did not cancel background work")
	}
	if err := store.Update(func(c *config.Config) error {
		c.LocalAccounts[0].ProviderID, c.LocalAccounts[0].BaseURL, c.LocalAccounts[0].Wire = "openai", "https://chatgpt.com/backend-api/codex", "responses"
		c.RemoteAPI = map[string]config.RemoteAPI{"codex": {AccountID: "primary", Model: "gpt-fixture"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg := store.Get()
	if err := store.BindRemoteStation(config.RemoteIdentity(cfg), "r1", &protocol.StationPolicy{Version: 1, AllowOfficialAccounts: true}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case fetchCtx = <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("station authorization did not start official quota maintenance")
	}
	store.ObserveRemoteStation(cfg, nil, store.BeginRemoteStationObservation(cfg))
	select {
	case <-fetchCtx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("station revocation did not cancel official quota maintenance")
	}
	s.CloseAPIListeners()
	if err := store.Update(func(c *config.Config) error {
		c.CodexAPI.Enabled = true
		c.CodexAPI.RuntimeLease = c.GatewayRuntimeKey
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
		t.Fatal("late store callback reactivated maintenance after console close")
	case <-time.After(70 * time.Millisecond):
	}
}
