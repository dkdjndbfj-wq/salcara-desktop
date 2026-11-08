package hubclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/desktopcompanion"
)

const nativeModelKey = "codex:01a0ae56-e9f6-4933-9ad4-5e08cd7874e5"

type optionsRouteDesktop struct {
	routeDesktop
	options  []desktopcompanion.NativeSendOptions
	onStatus func()
}

func (d *optionsRouteDesktop) NativeStatus(ctx context.Context) (desktopcompanion.NativeConnection, error) {
	if d.onStatus != nil {
		d.onStatus()
	}
	return d.routeDesktop.NativeStatus(ctx)
}
func (d *optionsRouteDesktop) NativeSendWithOptions(ctx context.Context, key, text, op string, options desktopcompanion.NativeSendOptions) error {
	if options.ValidateSource != nil {
		if err := options.ValidateSource(); err != nil {
			return err
		}
	}
	d.options = append(d.options, options)
	return d.NativeSend(ctx, key, text, op)
}

func nativeModelsFixture(t *testing.T) (*Client, *optionsRouteDesktop, func()) {
	t.Helper()
	var onModels func()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-native-key" {
			t.Error("used another API source")
		}
		if onModels != nil {
			onModels()
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-native-fixture"}]}`))
	}))
	t.Cleanup(server.Close)
	a := config.LocalAccount{ID: "desktop-a", Name: "Desktop API", Kind: "api", Key: "synthetic-native-key", BaseURL: server.URL}
	b := config.LocalAccount{ID: "background-b", Name: "Background API", Kind: "api", Key: "synthetic-other-key", BaseURL: "https://fixture.invalid"}
	cfg := config.Config{LocalAccounts: []config.LocalAccount{a, b}, RemoteAPI: map[string]config.RemoteAPI{"codex": {AccountID: b.ID}}, ComputerID: "synthetic-computer"}
	cfg.RecordAppliedAPI("codex-desktop", a, "fixture-provider")
	c := statusFixture(t, cfg)
	d := &optionsRouteDesktop{routeDesktop: routeDesktop{st: liveDesktop(nativeModelKey)}}
	d.st.Capabilities.ModelOverride = true
	d.st.ModelOverrideTransport = desktopcompanion.NativeModelOverrideTransport
	d.st.ModelOptions = &desktopcompanion.NativeModelSupport{Model: true, Thinking: []string{"low", "high"}}
	c.o.Desktop = d
	mutate := func() {
		onModels = func() {
			_ = c.o.Store.Update(func(cfg *config.Config) error { delete(cfg.ToolAPIApplied, "codex"); return nil })
		}
	}
	return c, d, mutate
}

func TestNativeModelCatalogUsesAppliedNotRemoteAPIAndNeedsExactLease(t *testing.T) {
	c, d, _ := nativeModelsFixture(t)
	cmd := map[string]any{"type": "models.list", "tool": "codex", "agent": "codex-desktop", "controlSurface": "desktop", "sessionKey": nativeModelKey,
		"accountId": APIHandle(c.o.Store.Get(), "desktop-a")}
	result, err := c.Dispatch(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.(map[string]any)["models"], []string{"gpt-native-fixture"}) {
		t.Fatal(result)
	}
	if result.(map[string]any)["accountId"] != cmd["accountId"] {
		t.Fatal("wrong source handle", result)
	}
	cmd["accountId"] = APIHandle(c.o.Store.Get(), "background-b")
	if _, err := c.Dispatch(context.Background(), cmd); err == nil {
		t.Fatal("background source adopted by native catalog")
	}
	delete(cmd, "accountId")
	d.st.ModelOptions = nil
	if _, err := c.Dispatch(context.Background(), cmd); err == nil {
		t.Fatal("unproven host support opened model list")
	}
}

func TestNativeModelSendPreservesHostOptionsWithoutCLI(t *testing.T) {
	c, d, _ := nativeModelsFixture(t)
	cmd := map[string]any{"type": "desktop.session.send", "controlSurface": "desktop", "sessionKey": nativeModelKey, "text": "fixture", "model": "gpt-native-fixture", "effort": "high", "accountId": APIHandle(c.o.Store.Get(), "desktop-a"), "operationId": "11111111-2222-4333-8444-555555555555"}
	result, err := c.Dispatch(context.Background(), cmd)
	if err != nil || result.(map[string]any)["via"] != "desktop" {
		t.Fatal(result, err)
	}
	if len(d.options) != 1 || d.options[0].Model != "gpt-native-fixture" || d.options[0].Thinking != "high" || d.options[0].ValidateSource == nil {
		t.Fatal(d.options)
	}
	for _, change := range []map[string]any{{"effort": "max"}, {"model": "other"}, {"accountId": APIHandle(c.o.Store.Get(), "background-b")}, {"permissionMode": "full-access"}} {
		copy := map[string]any{}
		for k, v := range cmd {
			copy[k] = v
		}
		for k, v := range change {
			copy[k] = v
		}
		if _, err := c.Dispatch(context.Background(), copy); err == nil {
			t.Fatal("unsupported option dispatched", change)
		}
	}
	if len(d.options) != 1 {
		t.Fatal("rejected request reached native")
	}
	d.sendErr = errors.New("synthetic host rejected model")
	if _, err := c.Dispatch(context.Background(), cmd); err == nil {
		t.Fatal("host rejection swallowed")
	}
}

func TestNativeModelSourceRaceDoesNotPublishOrSend(t *testing.T) {
	for _, typ := range []string{"models.list", "desktop.session.send"} {
		t.Run(typ, func(t *testing.T) {
			c, d, mutate := nativeModelsFixture(t)
			mutate()
			_, err := c.Dispatch(context.Background(), map[string]any{"type": typ, "tool": "codex", "controlSurface": "desktop", "sessionKey": nativeModelKey, "text": "fixture", "model": "gpt-native-fixture"})
			if err == nil || len(d.options) != 0 {
				t.Fatal("changed source crossed model boundary", err, d.options)
			}
		})
	}
}

func TestNativeModelsSeparatePublicAppliedMetadataAndExecutionGuard(t *testing.T) {
	c, _, _ := nativeModelsFixture(t)
	cfg := c.o.Store.Get()
	for _, st := range dispatchStatus(t, c) {
		if st.ID == "codex" && (st.API.AccountID != APIHandle(cfg, "background-b") || st.DesktopLive == nil || st.DesktopLive.AccountID != APIHandle(cfg, "desktop-a") || !st.DesktopLive.Capabilities.ModelOverride) {
			t.Fatalf("sources merged: %+v", st)
		}
		if st.ID == "codex-desktop" && (st.API.AccountID != APIHandle(cfg, "desktop-a") || !st.Capabilities.ModelOverride) {
			t.Fatalf("native source missing: %+v", st)
		}
	}
	cfg.RemoteExecutionArmed = true
	ctx := withCommandGuard(context.Background(), commandGuard{})
	for _, typ := range []string{"desktop.session.send", "desktop.approval.respond"} {
		cmd := map[string]any{"type": typ, "tool": "codex", "accountId": APIHandle(cfg, "desktop-a")}
		if err := c.validateNativeExecutionAccountFor(ctx, cfg, cmd); err != nil {
			t.Fatal(err)
		}
		cmd["accountId"] = APIHandle(cfg, "background-b")
		if err := c.validateNativeExecutionAccountFor(ctx, cfg, cmd); err == nil {
			t.Fatal("background key authorized native command", typ)
		}
	}
}
