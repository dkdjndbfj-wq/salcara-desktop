package hubclient

import (
	"context"
	"errors"
	"strings"

	"salcara/bridge/internal/config"
)

// A context marker is not a pairing lease. Only the authenticated direct
// listener can supply the live validator; Hub envelopes always take precedence.
func validateDirectBinding(ctx context.Context, cfg config.Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, validate, ok := DirectExecutionValidator(ctx)
	if !ok || !cfg.DirectRelayEnabled || cfg.DirectPhoneID == "" || cfg.DirectPhoneHash == "" {
		return errors.New("直连授权已失效，请重新连接电脑")
	}
	if err := validate(); err != nil {
		return errors.New("直连授权已失效，请重新连接电脑")
	}
	return nil
}

func (c *Client) validateDirectAdmission(ctx context.Context, cfg config.Config, cmd map[string]any) error {
	if err := validateDirectBinding(ctx, cfg); err != nil {
		return err
	}
	typ := str(cmd, "type")
	if typ == "agents.api.set" {
		if cfg.GatewayRuntimeKey == "" {
			return errors.New("直连 API 服务尚未就绪，请重启 Bridge 后重试")
		}
		return nil // The selected source is validated by setDirectAPI, including a clear.
	}
	if !sourceGuardExecutionType(typ) {
		return nil
	}
	family := c.apiFamilyForAgent(commandAgent(cmd))
	choice, selected := cfg.DirectAPI[family]
	if !selected || choice.AccountID == "" {
		return nil // The original follow-computer/native-login behavior remains available.
	}
	a, ok := cfg.DirectToolAccount(family)
	verified := a
	if !ok || config.ValidateLocalAccount(&verified) != nil || cfg.DirectGatewayCredential(config.DirectGatewayTarget(family)) == "" {
		return errors.New("直连 API 已失效，请重新选择；未使用其他 API")
	}
	if handle := strings.TrimSpace(str(cmd, "accountId")); handle != "" && handle != APIHandle(cfg, a.ID) {
		return errors.New("直连 API 来源已变化，请刷新后重试")
	}
	return nil
}

// Called while Dispatch holds taskConfigMu. This only changes the independent
// direct worker choice; desktop bindings, native files and Hub leases are intact.
func (c *Client) setDirectAPI(ctx context.Context, cmd map[string]any) (any, error) {
	if c.o.Store == nil {
		return nil, errors.New("程序还在启动，请稍后再试")
	}
	snapshot := c.o.Store.Get()
	if err := validateDirectBinding(ctx, snapshot); err != nil {
		return nil, err
	}
	agentID, handle := str(cmd, "agent"), str(cmd, "accountId")
	if agentID == "codex-desktop" || agentID == "claude-desktop" || str(cmd, "controlSurface") != "" && str(cmd, "controlSurface") != "cli" {
		return nil, errors.New("电脑直连只允许 CLI Agent 的 API 配置")
	}
	route, err := c.routeForAgent(agentID)
	if err != nil {
		return nil, err
	}
	model, err := explicitModelField(cmd)
	if err != nil {
		return nil, err
	}
	if err := c.requireExplicitModelCapability(agentID, model); err != nil {
		return nil, err
	}
	if err := c.checkRemoteAPIChange(ctx, route.Family, str(cmd, "sessionKey")); err != nil {
		return nil, err
	}
	var selected config.LocalAccount
	if handle != "" {
		id, exists := AccountForHandle(snapshot, handle)
		if !exists {
			return nil, errors.New("电脑上没有这个 API，请刷新后重选")
		}
		selected, _ = snapshot.LocalAccount(id)
		verified := selected
		if err := config.ValidateLocalAccount(&verified); err != nil {
			return nil, errors.New("这个 API 配置不完整，请在电脑端检查")
		}
		if model != "" {
			if isOfficialModelAccount(selected) {
				if err := validateOfficialModel(selected, model); err != nil {
					return nil, err
				}
			} else {
				models, err := c.verifiedAPIModels(ctx, selected, false)
				if err != nil {
					return nil, err
				}
				if !containsKey(models, strings.TrimSpace(model)) {
					return nil, errors.New("这个 API 不支持该模型，请刷新后重选模型")
				}
			}
		}
	}
	if err := c.checkRemoteAPIChange(ctx, route.Family, str(cmd, "sessionKey")); err != nil {
		return nil, err
	}
	if err := c.requireExplicitModelCapability(agentID, model); err != nil {
		return nil, err
	}
	// The relay validator may acquire the config lock. Never invoke it from
	// Store.Update: compare the captured phone/process identity in that callback.
	if err := validateDirectBinding(ctx, c.o.Store.Get()); err != nil {
		return nil, err
	}
	if c.o.Store.Get().RemoteConnectionEpoch != snapshot.RemoteConnectionEpoch {
		return nil, errors.New("连接方式已变化，请刷新后重选 API")
	}
	err = c.o.Store.Update(func(cfg *config.Config) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if cfg.RemoteConnectionEpoch != snapshot.RemoteConnectionEpoch || cfg.DeviceID != snapshot.DeviceID || !cfg.DirectRelayEnabled || cfg.DirectPhoneID != snapshot.DirectPhoneID || cfg.DirectPhoneHash != snapshot.DirectPhoneHash ||
			cfg.GatewayRuntimeKey == "" || cfg.GatewayRuntimeKey != snapshot.GatewayRuntimeKey {
			return errors.New("直连授权已变化，请重新连接电脑")
		}
		if handle != "" {
			id, exists := AccountForHandle(*cfg, handle)
			current, found := cfg.LocalAccount(id)
			if !exists || !found || id != selected.ID || catalogAccountFingerprint(current) != catalogAccountFingerprint(selected) {
				return errors.New("API 已更换，请刷新后重选")
			}
		}
		return cfg.SetDirectAPIForAgent(route.Family, selected.ID, model)
	})
	if err != nil {
		return nil, err
	}
	if selected.ID != "" && strings.TrimSpace(model) == "" {
		c.invalidateAPIModels(selected.ID)
	}
	return c.agentStatus(ctx), nil
}

func directAPIStatus(cfg config.Config, family string) agentAPIStatus {
	choice, selected := cfg.DirectAPI[family]
	if !selected || choice.AccountID == "" {
		cfg.RemoteAPI = nil
		return remoteAPIStatus(cfg, family)
	}
	st := agentAPIStatus{Source: "phone"}
	a, ok := cfg.DirectToolAccount(family)
	if !ok {
		return st
	}
	st.Name, st.Model = safeConfirmationLabel(a.Name, "已命名 API", cfg), safeConfirmationLabel(a.Model, "", cfg)
	st.AccountID = APIHandle(cfg, a.ID)
	if a.Protocol == "responses" || a.Protocol == "chat" || a.Protocol == "anthropic" {
		st.Protocol = a.Protocol
	}
	verified := a
	st.Configured = config.ValidateLocalAccount(&verified) == nil && cfg.DirectGatewayCredential(config.DirectGatewayTarget(family)) != ""
	return st
}

func directAPIOptions(cfg config.Config) []remoteAPIOption {
	list := remoteAPIOptions(cfg)
	for i := range list {
		list[i].Station, list[i].Stations = nil, []remoteAPIStation{}
		id, exists := AccountForHandle(cfg, list[i].ID)
		a, found := cfg.LocalAccount(id)
		valid := exists && found && config.ValidateLocalAccount(&a) == nil
		list[i].RemoteEligibility = remoteAPIEligibility{Supported: valid, Reason: "supported", APIRemote: true, AccountRemote: true}
		if !valid {
			list[i].RemoteEligibility.Reason = "unavailable"
		}
	}
	return list
}
