package hubclient

import (
	"context"
	"errors"
	"reflect"
	"strings"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/desktopcompanion"
	"salcara/bridge/internal/desktoplink"
	"salcara/bridge/internal/protocol"
)

// Optional so legacy adapters remain text-only, rather than silently dropping
// model fields through the old NativeSend signature.
type nativeOptionsSender interface {
	NativeSendWithOptions(context.Context, string, string, string, desktopcompanion.NativeSendOptions) error
}

func (c *Client) validateNativeExecutionAccountFor(ctx context.Context, cfg config.Config, cmd map[string]any) error {
	if !cfg.RemoteExecutionArmed {
		return nil
	}
	if _, remote := commandGuardFromContext(ctx); !remote {
		return nil
	}
	if err := validateNativeIdentity(cmd); err != nil {
		return err
	}
	applied, _, ok := nativeAppliedAPI(cfg)
	if !ok || str(cmd, "accountId") != APIHandle(cfg, applied.ID) {
		return errors.New("远程任务的 API 与当前桌面已应用来源不一致，请刷新后重试；未使用其他 Key")
	}
	return nil
}

func (c *Client) nativeModelStatus(ctx context.Context, key string) (desktopcompanion.NativeConnection, error) {
	if c.o.Desktop == nil {
		return desktopcompanion.NativeConnection{}, desktopcompanion.ErrActivationRequired
	}
	if _, ok := c.o.Desktop.(nativeOptionsSender); !ok {
		return desktopcompanion.NativeConnection{}, desktopcompanion.ErrDesktopCapability
	}
	st, err := c.o.Desktop.NativeStatus(ctx)
	if err != nil {
		return st, err
	}
	if !desktopcompanion.ValidateNativeConnection(st, nowMS()) {
		return st, desktopcompanion.ErrActivationRequired
	}
	if !st.Capabilities.Send || !st.Capabilities.ModelOverride {
		return st, desktopcompanion.ErrDesktopCapability
	}
	if key != "" && !containsKey(st.SessionKeys, key) {
		return st, desktopcompanion.ErrDesktopScope
	}
	return st, nil
}

func nativeAppliedAPI(cfg config.Config) (config.LocalAccount, config.LocalAccount, bool) {
	a, ok := cfg.AuthorizedAppliedToolAccount("codex-desktop")
	if !ok {
		return config.LocalAccount{}, config.LocalAccount{}, false
	}
	vault, ok := cfg.LocalAccount(a.ID)
	checked := a
	if !ok || config.ValidateLocalAccount(&checked) != nil {
		return config.LocalAccount{}, config.LocalAccount{}, false
	}
	return a, vault, true
}

func sameNativeApplied(before, after config.Config) bool {
	a, av, ok := nativeAppliedAPI(before)
	b, bv, exists := nativeAppliedAPI(after)
	return ok && exists && a.ID == b.ID && config.AccountFingerprint(a) == config.AccountFingerprint(b) &&
		a.GatewayLaunchLease == b.GatewayLaunchLease && catalogAccountFingerprint(av) == catalogAccountFingerprint(bv) &&
		APIHandle(before, a.ID) == APIHandle(after, b.ID)
}

func validateNativeIdentity(cmd map[string]any) error {
	for _, name := range []string{"agent", "tool"} {
		if value, present := cmd[name]; present {
			text, valid := value.(string)
			if !valid || text != "" && text != "codex" && text != "codex-desktop" {
				return errors.New("原生桌面 Agent 与会话不一致")
			}
		}
	}
	return nil
}

// Native model hints must come from the API actually applied to the desktop,
// not a newer phone-only RemoteAPI selection used by a background CLI worker.
func (c *Client) dispatchNativeModels(ctx context.Context, cmd map[string]any) (any, error) {
	if err := validateNativeIdentity(cmd); err != nil {
		return nil, err
	}
	key := str(cmd, "sessionKey")
	if _, err := desktoplink.CodexURL(key); err != nil {
		return nil, desktopcompanion.ErrDesktopScope
	}
	before, err := c.nativeModelStatus(ctx, key)
	if err != nil {
		return nil, err
	}
	if c.o.Store == nil {
		return nil, errors.New("尚无已应用的桌面 API，不能读取模型目录")
	}
	cfg := c.o.Store.Get()
	a, vault, ok := nativeAppliedAPI(cfg)
	if !ok {
		return nil, errors.New("尚无已应用的桌面 API，请在电脑端选择模型")
	}
	if raw, present := cmd["accountId"]; present && raw != APIHandle(cfg, a.ID) {
		return nil, errors.New("桌面 API 已更换，请重新读取模型")
	}
	refresh, _ := cmd["refresh"].(bool)
	var models []string
	var capabilities map[string]protocol.ModelCapability
	if isOfficialModelAccount(vault) {
		var catalog protocol.ModelCatalog
		catalog, err = c.OfficialModelCatalog(ctx, vault, refresh)
		models, capabilities = catalog.Models, catalog.ModelCapabilities
	} else {
		models, err = c.verifiedAPIModels(ctx, vault, refresh)
		capabilities = protocol.UnknownModelCapabilities(models, "desktop-applied-api-hint")
	}
	if err != nil {
		return nil, err
	}
	after, err := c.nativeModelStatus(ctx, key)
	if err != nil {
		return nil, err
	}
	current := c.o.Store.Get()
	if !sameNativeApplied(cfg, current) || before.ExpiresAt != after.ExpiresAt || !reflect.DeepEqual(before.ModelOptions, after.ModelOptions) {
		return nil, errors.New("桌面来源或授权已改变，请重新读取模型")
	}
	// The host validates model availability on send. An upstream catalogue is
	// not a promise that a desktop version supports every advertised model.
	return map[string]any{"models": models, "modelCapabilities": capabilities,
		"api": safeConfirmationLabel(a.Name, "已命名 API", current), "accountId": APIHandle(current, a.ID),
		"controlSurface": "desktop", "modelOverrideTransport": after.ModelOverrideTransport, "modelOptions": after.ModelOptions}, nil
}

func (c *Client) validateNativeModelSource(ctx context.Context, cmd map[string]any, model string) (func() error, error) {
	if c.o.Store == nil {
		return nil, errors.New("桌面 API 来源未确认；消息未发送")
	}
	cfg := c.o.Store.Get()
	a, vault, ok := nativeAppliedAPI(cfg)
	if !ok {
		return nil, errors.New("桌面 API 来源未确认，请在电脑端选择模型；消息未发送")
	}
	if raw, present := cmd["accountId"]; present && raw != APIHandle(cfg, a.ID) {
		return nil, errors.New("桌面 API 已更换；消息未发送")
	}
	if model != "" {
		if isOfficialModelAccount(vault) {
			if err := validateOfficialModel(vault, model); err != nil {
				return nil, err
			}
		} else {
			models, err := c.verifiedAPIModels(ctx, vault, false)
			if err != nil {
				return nil, err
			}
			if !containsKey(models, model) {
				return nil, errors.New("当前桌面 API 不包含此模型；消息未发送")
			}
		}
	}
	guard := func() error {
		if !sameNativeApplied(cfg, c.o.Store.Get()) {
			return errors.New("桌面 API 来源已改变；消息未发送")
		}
		return nil
	}
	return guard, guard()
}

func nativeOptionsFromCommand(cmd map[string]any) (desktopcompanion.NativeSendOptions, error) {
	options := desktopcompanion.NativeSendOptions{}
	for _, field := range []struct {
		name  string
		value *string
	}{{"model", &options.Model}, {"effort", &options.Thinking}} {
		if raw, present := cmd[field.name]; present {
			text, ok := raw.(string)
			if !ok || validateExplicitModel(text) != nil || text != "" && strings.TrimSpace(text) != text {
				return options, errors.New("桌面模型选项格式无效；消息未发送")
			}
			*field.value = text
		}
	}
	return options, nil
}
