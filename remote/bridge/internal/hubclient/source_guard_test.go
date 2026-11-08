package hubclient

import (
	"context"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func sourceGuardFixture(t *testing.T, sourceType string) (*Client, *config.Store, config.LocalAccount, protocol.CommandEnvelope) {
	t.Helper()
	store, err := config.Open(t.TempDir() + "/config.json")
	if err != nil {
		t.Fatal(err)
	}
	account := config.LocalAccount{ID: "a1", Name: "A", Kind: "api", SourceType: sourceType, BaseURL: "https://api.example.test/v1", Key: "key-a", Model: "model-a", AuthMode: "api-key"}
	if err := store.Update(func(c *config.Config) error {
		c.LocalAccounts = []config.LocalAccount{account}
		c.RemoteAPI = map[string]config.RemoteAPI{"codex": {AccountID: account.ID, Model: account.Model}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	store.SetRuntimeGatewayKey("runtime-lease")
	c := New(Options{Store: store})
	handle := APIHandle(store.Get(), account.ID)
	command := map[string]any{"type": "session.send", "agent": "codex", "tool": "codex", "controlSurface": "cli", "sessionKey": "codex:s", "accountId": handle, "model": account.Model}
	env := protocol.CommandEnvelope{Phone: true, Command: command, Source: &protocol.ExecutionSource{Mode: "restricted", PolicyRevision: "r1", Issuer: "site", SourceType: "site_api", CredentialHandle: handle, Origin: "https://api.example.test", ConfigRevision: store.Get().AccountSourceRevision(account), Agent: "codex", ControlSurface: "cli", Model: account.Model}}
	env.Source.Station = &protocol.StationPolicy{Version: 1, APIBases: []string{"https://api.example.test"}}
	return c, store, account, env
}

func TestValidateExecutionSourceBindsRequestedAccountFingerprint(t *testing.T) {
	c, store, account, env := sourceGuardFixture(t, "api_key")
	if err := c.validateExecutionSource(env); err != nil {
		t.Fatalf("valid source rejected: %v", err)
	}
	if err := store.Update(func(cfg *config.Config) error { cfg.LocalAccounts[0].Key = "key-changed"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := c.validateExecutionSource(env); err == nil {
		t.Fatal("stale source fingerprint was accepted after key change")
	}
	_ = account
}

func TestValidateExecutionSourceRejectsConflictingAgentNamespaces(t *testing.T) {
	c, _, _, env := sourceGuardFixture(t, "api_key")
	env.Command["tool"] = "claude"
	if err := c.validateExecutionSource(env); err == nil {
		t.Fatal("conflicting tool namespace was accepted")
	}
}

func TestValidateExecutionSourceRejectsUnclassifiedMigratedAccount(t *testing.T) {
	c, _, _, env := sourceGuardFixture(t, "")
	if err := c.validateExecutionSource(env); err == nil {
		t.Fatal("unclassified account was accepted as site API")
	}
}

func TestValidateExecutionAccountRequiresManagedRemoteSource(t *testing.T) {
	c, store, account, env := sourceGuardFixture(t, "api_key")
	ctx := withCommandGuard(context.Background(), commandGuard{identity: config.RemoteIdentity(store.Get()), deviceID: store.Get().DeviceID})
	handle := APIHandle(store.Get(), account.ID)
	if err := c.validateExecutionAccount(ctx, "codex", "codex:s", handle); err != nil {
		t.Fatalf("managed source rejected: %v", err)
	}
	if err := c.validateExecutionAccount(ctx, "codex", "codex:s", "api_unknown"); err == nil {
		t.Fatal("unknown API handle accepted")
	}
	_ = env
}

func TestExecutionRejectsOtherSavedAccount(t *testing.T) {
	c, store, account, _ := sourceGuardFixture(t, "api_key")
	other := account
	other.ID, other.Key = "a2", "key-b"
	if err := store.Update(func(cfg *config.Config) error {
		cfg.LocalAccounts = append(cfg.LocalAccounts, other)
		cfg.RemoteAPI["codex"] = config.RemoteAPI{AccountID: other.ID, Model: other.Model}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx := withCommandGuard(context.Background(), commandGuard{})
	if err := c.validateExecutionAccount(ctx, "codex", "codex:s", APIHandle(store.Get(), account.ID)); err == nil {
		t.Fatal("declared A was accepted while worker executes B")
	}
	if err := c.validateExecutionAccount(ctx, "codex", "codex:s", APIHandle(store.Get(), other.ID)); err != nil {
		t.Fatalf("current B rejected: %v", err)
	}
}

func TestExecutionRejectsConflictingSessionAgentInOpenMode(t *testing.T) {
	c, store, account, _ := sourceGuardFixture(t, "api_key")
	ctx := withCommandGuard(context.Background(), commandGuard{})
	if err := c.validateExecutionAccount(ctx, "codex", "claude:s", APIHandle(store.Get(), account.ID)); err == nil {
		t.Fatal("open mode routed through conflicting session agent")
	}
}

func TestSourceGuardDoesNotTrustPhoneFlag(t *testing.T) {
	c, _, _, env := sourceGuardFixture(t, "api_key")
	env.Phone, env.Source = false, nil
	if err := c.validateExecutionSource(env); err == nil {
		t.Fatal("unmarked Hub mutation bypassed source guard")
	}
	env.Command = map[string]any{"type": "device.ping"}
	if err := c.validateExecutionSource(env); err != nil {
		t.Fatal(err)
	}
}

func TestRestrictedHandoverChecksTargetSnapshot(t *testing.T) {
	c, _, _, env := sourceGuardFixture(t, "api_key")
	env.Command["type"] = "remote.station.switch"
	env.Source.ConfigRevision = "stale-target-revision"
	if err := c.validateExecutionSource(env); err == nil {
		t.Fatal("handover accepted stale target credential proof")
	}
}

func TestAccountOriginIPv6AndPorts(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"https://[2001:db8::1]:443/v1", "https://[2001:db8::1]"},
		{"http://[::1]:9000/v1", "http://[::1]:9000"},
		{"https://example.test:80/v1", "https://example.test:80"},
	} {
		if got := accountOrigin(config.LocalAccount{BaseURL: tc.input}); got != tc.want {
			t.Errorf("origin(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestSourceGuardRejectsUnprovenRestrictedNativeExecution(t *testing.T) {
	c, _, _, env := sourceGuardFixture(t, "api_key")
	env.Command["type"], env.Command["controlSurface"] = "desktop.session.send", "desktop"
	env.Source.ControlSurface = "desktop"
	if err := c.validateExecutionSource(env); err == nil {
		t.Fatal("restricted native execution accepted only a CLI source")
	}
	env.Source.Mode = "open"
	if err := c.validateExecutionSource(env); err == nil {
		t.Fatal("open mode accepted unproven native provider")
	}
}
