package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"salcara/bridge/internal/config"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/desktopcompanion"
	"salcara/bridge/internal/protocol"
)

func TestStationBoundaryOpenRequiresStationContract(t *testing.T) {
	c, _, _, env := sourceGuardFixture(t, "api_key")
	env.Source = &protocol.ExecutionSource{Mode: "open", PolicyRevision: "r1"}
	if err := c.validateExecutionSource(env); err == nil {
		t.Fatal("open Hub without station scope authorized an arbitrary saved API")
	}
}

func TestStationBoundaryModelTrafficStaysWithinStationAcrossKeys(t *testing.T) {
	var aCalls, bCalls atomic.Int32
	aServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aCalls.Add(1)
		w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
	}))
	defer aServer.Close()
	bServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bCalls.Add(1)
		w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
	}))
	defer bServer.Close()
	c, store, account, env := sourceGuardFixture(t, "api_key")
	env.Source.Mode = "open"
	env.Source.Station = &protocol.StationPolicy{Version: 1, APIBases: []string{aServer.URL}}
	env.Command["type"], env.Command["refresh"] = "models.list", true
	for _, tc := range []struct {
		base, key string
		allowed   bool
	}{
		{bServer.URL, "b-key", false}, {bServer.URL, "a-key", false}, {aServer.URL + "/other-tenant", "a-key", false},
		{aServer.URL, "a-key", true}, {aServer.URL + "/v1", "second-a-key", true},
	} {
		account.BaseURL, account.Key = tc.base, tc.key
		if err := store.Update(func(cfg *config.Config) error { cfg.LocalAccounts[0] = account; return nil }); err != nil {
			t.Fatal(err)
		}
		ctx := withExecutionSource(context.Background(), env)
		before := aCalls.Load() + bCalls.Load()
		models, err := c.verifiedAPIModels(ctx, account, true)
		if tc.allowed {
			if err != nil || len(models) != 1 || aCalls.Load()+bCalls.Load() != before+1 {
				t.Fatalf("same station key rejected: %v", err)
			}
		} else if err == nil || aCalls.Load()+bCalls.Load() != before {
			t.Fatalf("foreign station reached upstream: %v", err)
		}
	}
	if bCalls.Load() != 0 || aCalls.Load() != 2 {
		t.Fatalf("wrong routing totals A=%d B=%d", aCalls.Load(), bCalls.Load())
	}
}

func TestStationBoundaryOfficialNeedsExplicitSupportAndProtectedSource(t *testing.T) {
	c, store, account, env := sourceGuardFixture(t, "api_key")
	env.Source.Mode = "open"
	account.SourceType, account.ProviderID, account.CredentialRef, account.BaseURL, account.Wire = "official_account", "openai", "synthetic-vault-ref", "https://chatgpt.com/backend-api/codex", "responses"
	set := func() {
		t.Helper()
		if err := store.Update(func(cfg *config.Config) error { cfg.LocalAccounts[0] = account; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	set()
	if err := c.validateExecutionSource(env); err == nil {
		t.Fatal("official account accepted without site opt-in")
	}
	env.Source.Station.AllowOfficialAccounts = true
	if err := c.validateExecutionSource(env); err != nil {
		t.Fatalf("explicit official opt-in rejected: %v", err)
	}
	for _, spoof := range []string{"oauth", "reverse_proxy", "api_key"} {
		account.SourceType = spoof
		set()
		if err := c.validateExecutionSource(env); err == nil {
			t.Fatalf("source label %s impersonated official account", spoof)
		}
	}
}

func TestStationBoundaryNativeAliasesRemainClosedInOpenMode(t *testing.T) {
	c, _, _, env := sourceGuardFixture(t, "api_key")
	env.Source.Mode = "open"
	env.Command["controlSurface"] = "desktop"
	for _, typ := range []string{"session.send", "approval.respond", "desktop.session.send", "desktop.approval.respond", "desktop.navigate"} {
		env.Command["type"] = typ
		if err := c.validateExecutionSource(env); err == nil {
			t.Fatalf("native %s bypassed provider proof", typ)
		}
	}
}

func TestStationBoundaryModelRefreshRequiresSource(t *testing.T) {
	c, _, _, env := sourceGuardFixture(t, "api_key")
	env.Command["type"] = "models.list"
	env.Command["refresh"] = true
	env.Source = nil
	if err := c.validateExecutionSource(env); err == nil {
		t.Fatal("upstream model refresh bypassed source guard")
	}
}

func TestStationBoundaryQueuedEnvelopeCannotRestoreRetiredScope(t *testing.T) {
	for _, change := range []string{"failure", "disabled", "revision", "scope", "expired"} {
		t.Run(change, func(t *testing.T) {
			c, store, _, env := sourceGuardFixture(t, "api_key")
			env.Source.Mode = "open"
			if err := c.bindExecutionStation(context.Background(), env); err != nil {
				t.Fatal(err)
			}
			cfg := store.Get()
			observation := &config.StationObservation{Mode: "open", Revision: env.Source.PolicyRevision, Station: config.CloneStationPolicy(env.Source.Station), CheckedAt: time.Now().UnixMilli()}
			switch change {
			case "failure":
				observation = nil
			case "disabled":
				observation.Mode = "disabled"
			case "revision":
				observation.Revision = "r2"
			case "scope":
				observation.Station.APIBases = []string{}
			case "expired":
				observation.CheckedAt = time.Now().Add(-10 * time.Minute).UnixMilli()
			}
			store.ObserveRemoteStation(cfg, observation, store.BeginRemoteStationObservation(cfg))
			if err := c.bindExecutionStation(context.Background(), env); err == nil || store.Get().RemoteGatewayCredential("remote-codex") != "" {
				t.Fatal("queued envelope restored retired station authorization")
			}
			observation = &config.StationObservation{Mode: "open", Revision: "new-current", Station: config.CloneStationPolicy(env.Source.Station), CheckedAt: time.Now().UnixMilli()}
			store.ObserveRemoteStation(cfg, observation, store.BeginRemoteStationObservation(cfg))
			env.Source.PolicyRevision = observation.Revision
			if err := c.bindExecutionStation(context.Background(), env); err != nil || store.Get().RemoteGatewayCredential("remote-codex") == "" {
				t.Fatalf("fresh same-station command failed: %v", err)
			}
		})
	}
}

func TestStationExecutionRefreshRequiresCurrentHubPolicy(t *testing.T) {
	c, store, _, env := sourceGuardFixture(t, "api_key")
	env.Source.Mode = "open"
	policy := &config.StationObservation{Mode: "open", Revision: "r1", Station: config.CloneStationPolicy(env.Source.Station)}
	var policyMu sync.Mutex
	capabilities := []string{"pair.qr.v1", "device.identity.v1", "session.remote.v1", "pair.revoke.v1", "station.guard.v1"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		policyMu.Lock()
		defer policyMu.Unlock()
		expected := store.Get()
		if r.URL.Path != "/salcara-hub/v1/ping" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Salcara-Device-Secret") != expected.DeviceSecret || r.Header.Get("X-Salcara-Device-Id") != expected.DeviceID {
			t.Error("capability refresh did not use the selected station's device identity")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"service": "salcara-hub", "protocol": "salcara-remote", "protocolVersion": 1, "authModes": []string{"device-pairing"}, "capabilities": capabilities, "remoteAccess": policy})
	}))
	defer server.Close()
	if err := store.Update(func(cfg *config.Config) error { cfg.ConnectRemote(server.URL, server.URL+"/salcara-hub"); return nil }); err != nil {
		t.Fatal(err)
	}
	cfg := store.Get()
	if err := c.refreshExecutionStation(context.Background(), cfg, env); err != nil {
		t.Fatalf("current same-station command rejected: %v", err)
	}
	policyMu.Lock()
	policy.Revision = "r2"
	policyMu.Unlock()
	c.refreshStationCapabilities(context.Background(), cfg)
	if err := c.refreshExecutionStation(context.Background(), cfg, env); err == nil {
		t.Fatal("new discovery permitted an old queued policy")
	}
	env.Source.PolicyRevision = "r2"
	if err := c.refreshExecutionStation(context.Background(), cfg, env); err != nil {
		t.Fatalf("current policy rejected after refresh: %v", err)
	}
	policyMu.Lock()
	capabilities = capabilities[:len(capabilities)-1]
	policyMu.Unlock()
	c.refreshStationCapabilities(context.Background(), cfg)
	if err := c.refreshExecutionStation(context.Background(), cfg, env); err == nil {
		t.Fatal("Hub without station contract authorized execution")
	}
}

func TestStationNativeCapabilitiesDoNotAdvertiseUnattestedExecution(t *testing.T) {
	c, desktop, _ := nativeModelsFixture(t)
	c.o.Store.SetRuntimeGatewayKey("synthetic-native-process")
	if err := c.o.Store.Update(func(cfg *config.Config) error {
		account, _ := cfg.LocalAccount("desktop-a")
		cfg.RecordAppliedAPI("codex-desktop", account, "fixture-provider")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	desktop.st.Capabilities.Approval, desktop.st.Capabilities.Navigate = true, true
	desktop.st.ApprovalTransport = desktopcompanion.NativeApprovalTransport
	c.o.Desktop = desktop
	seen := false
	for _, agent := range dispatchStatus(t, c) {
		if agent.ID != "codex" || agent.DesktopLive == nil {
			continue
		}
		seen = true
		live := agent.DesktopLive
		if live.Capabilities.Send || live.Capabilities.Approval || live.Capabilities.Navigate || live.Capabilities.ModelOverride || live.ApprovalTransport != "" || live.ModelOverrideTransport != "" || live.ModelOptions != nil || live.AccountID != "" || live.APIName != "" || live.APIModel != "" || !live.Capabilities.Read {
			t.Fatal("remote native capabilities bypass station boundary or hide safe history")
		}
	}
	if !seen {
		t.Fatal("native history availability lost")
	}
}
