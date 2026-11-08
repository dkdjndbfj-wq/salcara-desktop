package hubclient

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

// sourceGuardExecutionType is deliberately kept in sync with the Hub's
// sourceExecutionCommand set. A read command must never be able to smuggle an
// execution source, while every phone mutation must either carry a Hub-issued
// source envelope or be rejected once the Bridge has armed its process lease.
func sourceGuardExecutionType(typ string) bool {
	if typ == cooperationPrepareCommand || sessionManagementCommand(typ) || sideQuestionCommand(typ) {
		return true
	}
	switch typ {
	case "session.start", "session.send", "session.continue", "session.steer", "approval.respond", "desktop.session.send", "desktop.approval.respond", "agents.api.set", "remote.station.switch", "desktop.navigate", "attachment.put", "models.list", "features.list", "session.context", "session.compact":
		return true
	default:
		return false
	}
}

func commandAgent(cmd map[string]any) string {
	agent := strings.TrimSpace(str(cmd, "agent"))
	tool := strings.TrimSpace(str(cmd, "tool"))
	keyAgent := ""
	if key := strings.TrimSpace(str(cmd, "sessionKey")); key != "" {
		if i := strings.IndexByte(key, ':'); i > 0 {
			keyAgent = strings.TrimSpace(key[:i])
		}
	}
	// The field actually consumed by Dispatch is authoritative. Redundant
	// namespaces are rejected instead of letting a signed Codex envelope route
	// a Claude session (or vice versa).
	candidates := []string{}
	if agent != "" {
		candidates = append(candidates, agent)
	}
	if tool != "" {
		candidates = append(candidates, tool)
	}
	if keyAgent != "" {
		candidates = append(candidates, keyAgent)
	}
	if len(candidates) == 0 {
		return ""
	}
	normalized := config.NormalizeAgentFamily(candidates[0])
	for _, candidate := range candidates[1:] {
		if normalized != config.NormalizeAgentFamily(candidate) {
			return ""
		}
	}
	return normalized
}

func commandSurface(typ string, cmd map[string]any) string {
	if value := strings.TrimSpace(str(cmd, "controlSurface")); value != "" {
		return value
	}
	if strings.HasPrefix(typ, "desktop.") {
		return "desktop"
	}
	return "cli"
}

func stationSourceOnly(cmd map[string]any) (bool, bool) {
	v, exists := cmd["sourceOnly"]
	if !exists {
		return false, true
	}
	value, ok := v.(bool)
	return value, ok
}

func accountOrigin(a config.LocalAccount) string {
	root, _, err := config.APIBase(a.BaseURL)
	if err != nil || root == "" {
		return ""
	}
	u, err := url.Parse(root)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname())
	port := u.Port()
	if port != "" && !((scheme == "https" && port == "443") || (scheme == "http" && port == "80")) {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host
}

func sourceTypeMatches(source string, account config.LocalAccount) bool {
	switch source {
	case "site_api":
		return account.SourceType == "api_key"
	case "verified_proxy":
		return account.SourceType == "reverse_proxy"
	case "official_account_proxy":
		return config.IsProtectedOfficialAccount(account)
	default:
		return false
	}
}

// validateExecutionSource checks the Hub-generated provenance envelope at the
// last hop before a worker/native companion is touched. It is intentionally
// independent of the phone JSON: a caller can choose an API handle, but it
// cannot choose the issuer, origin, policy revision, or source class.
func (c *Client) validateExecutionSource(env protocol.CommandEnvelope) error {
	if c.o.Store == nil {
		return nil
	}
	return c.validateExecutionSourceFor(env, c.o.Store.Get())
}

func (c *Client) validateExecutionSourceFor(env protocol.CommandEnvelope, cfg config.Config) error {
	typ := str(env.Command, "type")
	if typ == "remote.station.switch" {
		if _, valid := stationSourceOnly(env.Command); !valid {
			return errors.New("中转站切换参数无效")
		}
	}
	// Phone is diagnostic metadata, not authority to bypass a Hub mutation guard.
	if !sourceGuardExecutionType(typ) {
		return nil
	}
	// Embedded callers and pre-lease test fixtures have no process ownership
	// proof. Production main arms the runtime lease before opening Hub SSE.
	if !cfg.RemoteExecutionArmed {
		return nil
	}
	if env.Source == nil {
		return errors.New("中转站未提供来源证明，已拒绝接管其他软件的 API 配置")
	}
	source := env.Source
	if source.Mode != "open" && source.Mode != "restricted" || strings.TrimSpace(source.PolicyRevision) == "" {
		return errors.New("远程来源证明无效，任务未发送")
	}
	if !config.ValidStationPolicy(source.Station) {
		return errors.New("中转站尚不支持站点隔离，请更新站点服务")
	}
	if typ == "remote.station.switch" && source.Mode == "open" {
		return nil
	}
	if observation := cfg.RemoteStationObservation; typ != "remote.station.switch" && observation != nil && (!observation.Available() || observation.Mode != source.Mode || observation.Revision != source.PolicyRevision || !config.StationPolicyEqual(observation.Station, source.Station)) {
		return errors.New("中转站授权已变化，未使用旧来源")
	}
	agent := commandAgent(env.Command)
	if agent == "" || source.Mode == "restricted" && source.Agent != agent {
		return errors.New("远程来源与 Agent 不匹配，任务未发送")
	}
	surface := commandSurface(typ, env.Command)
	if source.Mode == "restricted" && source.ControlSurface != surface {
		return errors.New("远程来源与执行界面不匹配，任务未发送")
	}
	if surface != "cli" && surface != "desktop" || surface == "cli" && strings.HasPrefix(typ, "desktop.") {
		return errors.New("远程来源与实际执行通道不匹配，任务未发送")
	}
	// The native process can change provider outside Salcara. A saved CLI card
	// does not attest its actual connection, including session.send aliases.
	if surface == "desktop" && (typ == "desktop.session.send" || typ == "desktop.approval.respond" || typ == "session.send" || typ == "approval.respond" || typ == "desktop.navigate") {
		return errors.New("当前桌面适配尚不能证明实际 API 来源，请使用受管理的远程会话")
	}
	handle := strings.TrimSpace(str(env.Command, "accountId"))
	if source.Mode == "restricted" && (handle == "" || source.CredentialHandle == "" || handle != source.CredentialHandle) {
		return errors.New("远程来源与 API 不匹配，任务未发送")
	}
	// The target shares the physical vault. Resolve legacy station-scoped handles
	// against its identity instead of skipping type/origin/revision validation.
	if typ == "remote.station.switch" {
		if target, found := cfg.SavedRemoteConnection(str(env.Command, "targetHubUrl"), str(env.Command, "targetDeviceId")); found {
			cfg = activeStationConfig(cfg, target)
		}
	}
	// Older open clients omitted the handle on model queries/attachments and
	// on restoring the computer selection. Resolve only the actual managed
	// source; there is no fallback to a native login or an unrelated saved card.
	if handle == "" && source.Mode == "open" {
		var current config.LocalAccount
		var found bool
		if typ == "agents.api.set" {
			current, found = cfg.AuthorizedAppliedToolAccount(agent)
		} else {
			current, found = remoteModelAccount(cfg, agent)
		}
		if typ == "models.list" && surface == "desktop" {
			current, _, found = nativeAppliedAPI(cfg)
		}
		if found {
			handle = APIHandle(cfg, current.ID)
		}
	}
	// Resolve the handle carried by this command, rather than the currently
	// selected card. This permits an explicit phone API switch while still
	// binding the command to the exact vault snapshot used below.
	accountID, ok := AccountForHandle(cfg, handle)
	var selected config.LocalAccount
	if ok {
		selected, ok = cfg.LocalAccount(accountID)
	}
	if !ok || APIHandle(cfg, selected.ID) != handle {
		return errors.New("当前 Agent 没有与来源证明一致的 Salcara API")
	}
	// A handover grants transport movement only; switchStation independently
	// probes the saved target Hub before any target source can be used.
	if typ != "remote.station.switch" && !cfg.RemoteHubAllowsAccount(cfg.EffectiveHubURL(), selected) {
		return errors.New("此 API 尚未加入当前中转站，请在电脑端 API 设置中开启")
	}
	if typ != "remote.station.switch" && !config.StationAllowsAccount(source.Station, selected) {
		if selected.SourceType == "official_account" || selected.SourceType == "oauth" {
			return errors.New("本站暂不支持账号远程，请切换支持的中转站")
		}
		if source.Station.Version == 2 {
			if selected.SourceType == "reverse_proxy" {
				return errors.New("管理员未授权此逆向代理用于远程任务，请选择已授权的来源")
			}
			return errors.New("此 API 地址不在管理员允许的本站或外站范围内，请选择已授权的来源")
		}
		return errors.New("此 API 不属于当前中转站，请先切换到对应站点")
	}
	if typ == "models.list" {
		actual, found := remoteModelAccount(cfg, agent)
		if surface == "desktop" {
			actual, _, found = nativeAppliedAPI(cfg)
		}
		if !found || actual.ID != selected.ID {
			return errors.New("模型查询来源已变化，请重新选择 API")
		}
	}
	if source.Mode == "open" {
		return nil
	}
	if !sourceTypeMatches(source.SourceType, selected) {
		return errors.New("API 来源类型与中转站来源证明不匹配")
	}
	origin := accountOrigin(selected)
	if source.APIBase != "" {
		base, err := config.NormalizeStationAPIBase(selected.BaseURL)
		if err != nil || base != source.APIBase {
			return errors.New("API 路径与中转站来源证明不匹配")
		}
	}
	if origin == "" || !strings.EqualFold(origin, source.Origin) {
		return errors.New("API 地址与中转站来源证明不匹配")
	}
	if source.Issuer == "" || source.ConfigRevision == "" {
		return errors.New("远程来源证明不完整，任务未发送")
	}
	if source.ConfigRevision != cfg.AccountSourceRevision(selected) {
		return errors.New("API 配置已变化，请重新选择 API；未使用旧来源")
	}
	if model := strings.TrimSpace(str(env.Command, "model")); model != "" && source.Model != model {
		return errors.New("模型与远程来源证明不匹配")
	}
	return nil
}

// Called under taskConfigMu, separately from the pure validation helper which
// also runs inside Store.Update. Never acquire Store's lock from that helper.
func (c *Client) bindExecutionStation(ctx context.Context, env protocol.CommandEnvelope) error {
	if c.o.Store == nil || !sourceGuardExecutionType(str(env.Command, "type")) || str(env.Command, "type") == "remote.station.switch" {
		return nil
	}
	cfg := c.o.Store.Get()
	if !cfg.RemoteExecutionArmed {
		return nil
	}
	if err := c.validateExecutionSourceFor(env, cfg); err != nil {
		return err
	}
	return c.o.Store.BindRemoteStation(config.RemoteIdentity(cfg), env.Source.PolicyRevision, env.Source.Station, func(current config.Config) error {
		if guard, ok := commandGuardFromContext(ctx); ok && (current.DeviceID != guard.deviceID || config.RemoteIdentity(current) != guard.identity || !c.commandPhoneMatches(current, guard.bindingID, guard.phoneHash, guard.eventScope) || !c.commandEventScopeValid(current, guard.eventScope)) {
			return errors.New("手机绑定或中转站已变化，请重新连接")
		}
		return c.validateExecutionSourceFor(env, current)
	})
}
