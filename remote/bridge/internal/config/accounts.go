package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

// AccountTagMetadata is display-only library metadata. It must never take part
// in credential, gateway, launch, usage-source, or API-client fingerprints.
// Official entries are bound to a protected identity without attaching it.
type AccountTagMetadata struct {
	Tags          []string `json:"tags"`
	IdentityEpoch uint64   `json:"identityEpoch,omitempty"`
	Revision      uint64   `json:"revision"`
}

// LocalAccount is one API-library entry, independent of tool-card selections.
type LocalAccount struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"` // api; codex/claude retained only for legacy migration
	// SourceType describes where the credential comes from. It is deliberately
	// separate from Kind (the Agent that consumes it): one account can be used
	// by Codex, Claude Code, or another adapter without being owned by any one
	// tool card. Existing records default to api_key during validation.
	SourceType   string `json:"sourceType,omitempty"` // api_key, oauth, reverse_proxy, official_account
	ProviderID   string `json:"providerId,omitempty"`
	ProviderName string `json:"providerName,omitempty"`
	// CredentialRef points into the protected official-account vault, never to
	// a native application's auth.json or an access/refresh token.
	CredentialRef   string `json:"credentialRef,omitempty"`
	CredentialEpoch uint64 `json:"credentialEpoch,omitempty"`
	// PoolGroup is an optional Cockpit-style account pool. Empty means the
	// account remains manual-only; accounts in the same group may be selected
	// for new requests while a conversation remains sticky to one account.
	PoolGroup           string `json:"poolGroup,omitempty"`
	PoolEnabled         bool   `json:"poolEnabled,omitempty"`
	PoolStrategy        string `json:"poolStrategy,omitempty"`
	PoolBackup          bool   `json:"poolBackup,omitempty"`
	Priority            int    `json:"priority,omitempty"`
	Weight              int    `json:"weight,omitempty"`
	MaxConcurrency      int    `json:"maxConcurrency,omitempty"`
	QuotaReservePercent int    `json:"quotaReservePercent,omitempty"`
	BaseURL             string `json:"baseUrl"`
	Key                 string `json:"key"`
	// RemoteHubURLs limits which saved Hubs may use this API account from a
	// phone. An unset binding preserves legacy behavior; new accounts are
	// explicitly scoped by the console to the active Hub.
	RemoteHubURLs        []string `json:"remoteHubUrls,omitempty"`
	RemoteHubBindingsSet bool     `json:"remoteHubBindingsSet,omitempty"`
	Model                string   `json:"model,omitempty"`
	Models               []string `json:"models,omitempty"`
	ModelsManual         bool     `json:"modelsManual,omitempty"` // preserve an explicitly edited picker across automatic discovery
	// ModelAllowlist/ModelDenylist are optional provider-side routing rules.
	// An empty allowlist means "use the discovered catalog"; deny always wins.
	// They are deliberately stored as model IDs, never as prompt or response data.
	ModelAllowlist []string `json:"modelAllowlist,omitempty"`
	ModelDenylist  []string `json:"modelDenylist,omitempty"`
	AuthMode       string   `json:"authMode,omitempty"` // bearer or api-key for Anthropic
	Workspace      string   `json:"workspace,omitempty"`
	Target         string   `json:"target,omitempty"` // most recently selected launch target
	CreatedAt      int64    `json:"createdAt"`
	LastUsedAt     int64    `json:"lastUsedAt,omitempty"`
	Protocol       string   `json:"protocol,omitempty"` // effective upstream wire protocol, set on tool copies only
	// Wire is the provider's interface type: "" / "auto" picks per model
	// (claude → Messages, gpt/o-series → Responses, others → Chat Completions).
	Wire string `json:"wire,omitempty"`
	// SpeedMode requests an upstream service tier on OpenAI-compatible text
	// routes. It never changes the model or reasoning and is not a latency guarantee.
	SpeedMode string `json:"speedMode,omitempty"` // standard, fast, ultrafast
	// CatalogOverride is set on tool copies only: publish Models into the tool's
	// own model picker (never persisted on the vault entry).
	CatalogOverride    bool              `json:"-"`
	DesktopModelLabels map[string]string `json:"-"` // transient desktop compatibility routes; never stored in the vault
	GatewayLaunchLease string            `json:"-"` // prospective/applied local connection, never provider credentials

	// ExtraHeaders are static key→value headers injected on every upstream request
	// to this account. Use for channel-specific routing hints, beta feature flags,
	// or supplemental auth headers. Never overrides Authorization or Host.
	// These are routing metadata and are intentionally excluded from AccountFingerprint.
	ExtraHeaders map[string]string `json:"extraHeaders,omitempty"`

	// Capabilities declares optional features this account supports. The zero
	// value treats all capabilities as "unknown / probably supported" (fail-open).
	Capabilities AccountCapabilities `json:"capabilities,omitempty"`

	// SystemPromptPrefix is prepended to every request's system prompt before
	// forwarding to this account. Useful for injecting channel-specific instructions,
	// safety guardrails, or watermarking without modifying client requests.
	// Applied only when non-empty. Never stored in AccountFingerprint.
	SystemPromptPrefix string `json:"systemPromptPrefix,omitempty"`

	// FallbackModel is used when the requested model is unavailable on this account.
	// If empty, the request is rejected instead of falling back. The fallback model
	// must itself be in the account's Models list (or equal to Model).
	FallbackModel string `json:"fallbackModel,omitempty"`

	// MaxRPM is the maximum requests per minute for this account. 0 means unlimited.
	// Enforced by the local rate limiter; takes effect independently of concurrency limits.
	MaxRPM int `json:"maxRpm,omitempty"`

	// MaxTPM is the maximum tokens per minute for this account. 0 means unlimited.
	// The limiter uses the estimated request size before forwarding, and adjusts
	// after receiving actual usage from the provider.
	MaxTPM int `json:"maxTpm,omitempty"`
}

// AppliedAPI records a reference, not another copy of the API key.
type AppliedAPI struct {
	// LocalClientID is a managed local-service binding, not a vault account.
	// Its authorization is deliberately process-local and never a remote source.
	LocalClientID string   `json:"localClientId,omitempty"`
	ClientEpoch   string   `json:"-"`
	ServiceEpoch  string   `json:"-"`
	ManagedModels []string `json:"-"`
	AccountID     string   `json:"accountId"`
	Fingerprint   string   `json:"fingerprint"`
	Provider      string   `json:"provider,omitempty"`
	Model         string   `json:"model,omitempty"`
	Protocol      string   `json:"protocol,omitempty"`
	Catalog       bool     `json:"catalog,omitempty"`
	// ManagedBy is a provenance marker. A persisted selection alone is not
	// authorization to expose an API to a Bridge worker: only an explicit
	// Salcara apply operation writes this marker. Older configs without it are
	// treated as local-tool state and are never auto-adopted remotely.
	ManagedBy   string `json:"managedBy,omitempty"`
	LaunchLease string `json:"-"` // minted by an explicit apply, lost on Bridge restart
}

func AccountFingerprint(a LocalAccount) string {
	parts := []string{a.Kind, a.SourceType, a.ProviderID, a.PoolGroup, a.BaseURL, a.Key, a.Model, a.AuthMode, a.Protocol, strings.Join(a.ModelAllowlist, "\x1f"), strings.Join(a.ModelDenylist, "\x1f")}
	if a.CredentialRef != "" {
		parts = append(parts, a.CredentialRef, fmt.Sprint(a.CredentialEpoch))
	}
	if a.PoolEnabled {
		parts = append(parts, "pool-enabled")
	}
	if a.PoolStrategy != "" {
		parts = append(parts, "pool-strategy:"+a.PoolStrategy)
	}
	if a.PoolBackup {
		parts = append(parts, "pool-backup")
	}
	if a.QuotaReservePercent != 0 {
		parts = append(parts, fmt.Sprintf("quota-reserve:%d", a.QuotaReservePercent))
	}
	if a.Wire != "" && a.Wire != "auto" {
		// Empty and auto are equivalent and keep pre-existing auto snapshots valid.
		// Explicit interface changes must invalidate the previously applied route.
		parts = append(parts, "wire:"+a.Wire)
	}
	if mode := AccountSpeedMode(a.SpeedMode); mode != "standard" {
		parts = append(parts, "speed:"+mode)
	}
	if a.CatalogOverride {
		// Only present when on, so fingerprints recorded before this option stay valid.
		parts = append(parts, "catalog")
	}
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])
}

func ToolFamily(target string) string {
	if target == "codex-desktop" {
		return "codex"
	}
	return target
}

// ToolKind preserves extension IDs containing hyphens. Only the two built-in
// desktop surfaces alias a worker kind; a name prefix is not an adapter type.
func ToolKind(target string) string {
	if target == "codex-desktop" {
		return "codex"
	}
	if target == "claude-desktop" {
		return "claude"
	}
	return target
}

func IsAdditionalTool(kind string) bool { return kind == "deepseek-harness" || kind == "hermes" }

func (c Config) SelectedToolAccount(target string) (LocalAccount, bool) {
	if id, found := c.ToolAPISelections[target]; found {
		return c.LocalAccount(id)
	}
	kind := ToolKind(target)
	if kind != "codex" && kind != "claude" {
		return LocalAccount{}, false
	}
	return c.ActiveLocalAccount(kind)
}

func (c Config) AppliedToolAccount(target string) (LocalAccount, bool) {
	b, ok := c.ToolAPIApplied[ToolFamily(target)]
	if !ok {
		return LocalAccount{}, false
	}
	a, found := c.LocalAccount(b.AccountID)
	a = c.APIForTool(a, target)
	// A pending selection must never rewrite the connection already in use.
	a.Model, a.Protocol, a.CatalogOverride = b.Model, b.Protocol, b.Catalog
	a.GatewayLaunchLease = b.LaunchLease
	if a.Protocol == "" {
		a.Protocol = NativeProtocol(a.Kind)
	}
	return a, found && b.Fingerprint == AccountFingerprint(a)
}

// AppliedToolBinding returns only a binding explicitly committed by Salcara.
// This is intentionally stricter than AppliedToolAccount, which remains a
// compatibility/status helper for displaying the last local tool snapshot.
func (c Config) AppliedToolBinding(target string) (AppliedAPI, bool) {
	b, ok := c.ToolAPIApplied[ToolFamily(target)]
	if !ok || b.ManagedBy != "salcara" {
		return AppliedAPI{}, false
	}
	return b, true
}

// AuthorizedAppliedToolAccount resolves a Salcara-managed binding. It is the
// only accessor remote workers and the local protocol gateway should use.
func (c Config) AuthorizedAppliedToolAccount(target string) (LocalAccount, bool) {
	b, ok := c.AppliedToolBinding(target)
	if !ok || c.GatewayRuntimeKey != "" && b.LaunchLease == "" {
		return LocalAccount{}, false
	}
	return c.AppliedToolAccount(target)
}

// Invalidating an applied source also retires its launch lease. Restoring the
// same account policy later must require a fresh explicit apply, rather than
// silently reactivating credentials left in an older native profile.
func (c *Config) retireInvalidAppliedLaunches() {
	for target, binding := range c.ToolAPIApplied {
		if binding.ManagedBy == "salcara-local-api" {
			continue // checked after named-client epochs have been fenced
		}
		if binding.LaunchLease == "" {
			continue
		}
		if _, valid := c.AppliedToolAccount(target); !valid || binding.ManagedBy != "salcara" {
			binding.LaunchLease = ""
			c.ToolAPIApplied[target] = binding
		}
	}
}

// APIForTool adapts a vault entry without assigning that key to a tool or brand.
func (c Config) APIForTool(a LocalAccount, target string) LocalAccount {
	a.Kind = ToolKind(target)
	a.Target = target
	if selected, ok := c.SelectedToolAccount(target); ok && selected.ID == a.ID {
		if model, set := c.ToolModels[target]; set {
			a.Model = model
		}
		if protocol, set := c.ToolProtocols[target]; set {
			a.Protocol = protocol
		}
	}
	a.CatalogOverride = (a.Kind == "codex" || target == "claude-desktop") && c.ToolCatalogOverride[target]
	if a.Protocol == "" {
		a.Protocol = NativeProtocol(a.Kind)
		// Legacy configs may predate the workbench's explicit protocol field.
		// Infer the selected default model here so a Claude/Grok/DeepSeek model
		// cannot accidentally be written straight into Codex's native Responses
		// profile.  NeedsGateway will then force the local translator route.
		if inferred := InferProtocol(a.Model, a.Wire); inferred != "" && inferred != NativeProtocol(a.Kind) {
			a.Protocol = inferred
		}
	}
	return a
}

func (c Config) SelectedToolAPI(target string) (LocalAccount, bool) {
	a, ok := c.SelectedToolAccount(target)
	return c.APIForTool(a, target), ok
}

func (c *Config) RecordAppliedAPI(target string, a LocalAccount, provider string) {
	// The caller supplies the effective snapshot used to write the original tool.
	// Do not re-read selections here: they may change while the app is restarting.
	a.Kind, a.Target = ToolKind(target), target
	if a.Protocol == "" {
		a.Protocol = NativeProtocol(a.Kind)
	}
	if c.ToolAPISelections == nil {
		c.ToolAPISelections = map[string]string{}
	}
	if c.ToolAPIApplied == nil {
		c.ToolAPIApplied = map[string]AppliedAPI{}
	}
	c.ToolAPISelections[target] = a.ID
	lease := a.GatewayLaunchLease
	if lease == "" {
		lease = RandomToken(32)
	}
	c.ToolAPIApplied[ToolFamily(target)] = AppliedAPI{AccountID: a.ID, Fingerprint: AccountFingerprint(a), Provider: provider, Model: a.Model, Protocol: a.Protocol, Catalog: a.CatalogOverride, ManagedBy: "salcara", LaunchLease: lease}
}

var accountID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

const (
	PoolStrategyAuto            = "auto"
	PoolStrategyRandom          = "random"
	PoolStrategySingleAccount   = "single_account"
	PoolStrategyQuotaHighFirst  = "quota_high_first"
	PoolStrategyQuotaLowFirst   = "quota_low_first"
	PoolStrategyPlanHighFirst   = "plan_high_first"
	PoolStrategyPlanLowFirst    = "plan_low_first"
	PoolStrategyExpirySoonFirst = "expiry_soon_first"
	PoolStrategyCustom          = "custom"
)

// ValidPoolStrategy accepts the legacy empty value and the explicit Cockpit
// routing modes. Empty intentionally keeps Salcara's existing priority/load
// scheduler; explicit auto uses verified plan and quota observations.
func ValidPoolStrategy(strategy string) bool {
	switch strategy {
	case "", PoolStrategyAuto, PoolStrategyRandom, PoolStrategySingleAccount,
		PoolStrategyQuotaHighFirst, PoolStrategyQuotaLowFirst,
		PoolStrategyPlanHighFirst, PoolStrategyPlanLowFirst,
		PoolStrategyExpirySoonFirst, PoolStrategyCustom:
		return true
	default:
		return false
	}
}

func ValidAccountID(id string) bool { return accountID.MatchString(id) }

func (c Config) LocalAccount(id string) (LocalAccount, bool) {
	for _, a := range c.LocalAccounts {
		if a.ID == id {
			return a, true
		}
	}
	return LocalAccount{}, false
}

func (c Config) ActiveLocalAccount(kind string) (LocalAccount, bool) {
	id := c.ActiveCodexAccount
	if kind == "claude" {
		id = c.ActiveClaudeAccount
	}
	a, ok := c.LocalAccount(id)
	if !ok && len(c.LocalAccounts) != 0 {
		return c.LocalAccounts[0], true
	}
	return a, ok
}

// APIBase preserves path prefixes, accepts root and /v1, rejects URL credentials.
func APIBase(raw string) (root, v1 string, err error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if !strings.Contains(raw, "://") && raw != "" {
		raw = "https://" + raw
	}
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", "", errors.New("API 地址需要完整的 HTTP/HTTPS 地址，不能包含用户名、查询参数或片段")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if strings.HasSuffix(strings.ToLower(u.Path), "/v1") {
		u.Path = u.Path[:len(u.Path)-3]
	}
	u.RawPath = ""
	root = strings.TrimRight(u.String(), "/")
	return root, root + "/v1", nil
}

func ValidateLocalAccount(a *LocalAccount) error {
	if !ValidAccountID(a.ID) {
		return errors.New("无效的 API 账号编号")
	}
	a.Name, a.Key, a.Model = strings.TrimSpace(a.Name), strings.TrimSpace(a.Key), strings.TrimSpace(a.Model)
	if a.Name == "" || len([]rune(a.Name)) > 80 {
		return errors.New("账号名称需要 1–80 个字")
	}
	if a.Kind != "api" && a.Kind != "codex" && a.Kind != "claude" && !IsAdditionalTool(a.Kind) {
		return errors.New("无效的密钥条目")
	}
	if a.SourceType == "" {
		a.SourceType = "api_key"
	}
	switch a.SourceType {
	case "api_key", "oauth", "reverse_proxy", "official_account":
	default:
		return errors.New("无效的账号来源类型")
	}
	if len(a.ProviderID) > 128 || strings.ContainsAny(a.ProviderID, "\r\n\x00") || len(a.ProviderName) > 160 || strings.ContainsAny(a.ProviderName, "\r\n\x00") {
		return errors.New("服务商标识无效")
	}
	if a.CredentialRef != "" && (!ValidAccountID(a.CredentialRef) || a.SourceType != "official_account" || a.ProviderID != "openai") {
		return errors.New("官方账号引用无效")
	}
	a.ProviderID, a.ProviderName, a.PoolGroup, a.PoolStrategy = strings.TrimSpace(a.ProviderID), strings.TrimSpace(a.ProviderName), strings.TrimSpace(a.PoolGroup), strings.TrimSpace(a.PoolStrategy)
	if len(a.PoolGroup) > 128 || strings.ContainsAny(a.PoolGroup, "\r\n\x00") {
		return errors.New("账号池名称无效")
	}
	if a.Priority < -1000 || a.Priority > 1000 {
		return errors.New("账号池优先级需要在 -1000 到 1000 之间")
	}
	if a.Weight < 0 || a.Weight > 1000 || a.MaxConcurrency < 0 || a.MaxConcurrency > 10000 {
		return errors.New("账号池权重或并发上限无效")
	}
	if a.PoolEnabled && a.PoolGroup == "" {
		return errors.New("启用账号池前需要填写账号池名称")
	}
	if !ValidPoolStrategy(a.PoolStrategy) {
		return errors.New("账号池路由策略无效")
	}
	if a.QuotaReservePercent < 0 || a.QuotaReservePercent > 99 {
		return errors.New("额度保留比例需要在 0 到 99 之间")
	}
	if a.QuotaReservePercent > 0 && (a.SourceType != "official_account" || a.CredentialRef == "") {
		return errors.New("额度保留需要已导入的官方账号与经验证的额度数据")
	}
	if a.Kind != "api" && a.Target != "" && a.Target != a.Kind && a.Target != a.Kind+"-desktop" {
		return errors.New("启动工具与账号协议不匹配")
	}
	if a.Key == "" || len(a.Key) > 8192 || strings.ContainsAny(a.Key, "\r\n\x00") {
		return errors.New("请填写有效的 API Key")
	}
	if len(a.Model) > 200 || strings.ContainsAny(a.Model, "\r\n\x00") {
		return errors.New("模型名称过长或包含无效字符")
	}
	var err error
	a.ModelAllowlist, err = normalizeModelRules(a.ModelAllowlist)
	if err != nil {
		return errors.New("模型允许列表无效")
	}
	a.ModelDenylist, err = normalizeModelRules(a.ModelDenylist)
	if err != nil {
		return errors.New("模型拒绝列表无效")
	}
	root, _, err := APIBase(a.BaseURL)
	if err != nil {
		return err
	}
	a.BaseURL = root
	if a.CredentialRef != "" && (root != "https://chatgpt.com/backend-api/codex" || a.Wire != "responses") {
		return errors.New("官方账号必须使用其固定执行接口")
	}
	if a.AuthMode == "" {
		a.AuthMode = "bearer"
		if endpoint, parseErr := url.Parse(root); parseErr == nil && strings.EqualFold(endpoint.Hostname(), "api.anthropic.com") {
			a.AuthMode = "api-key"
		}
	}
	if a.AuthMode != "bearer" && a.AuthMode != "api-key" {
		return errors.New("认证方式需要 Bearer 或 x-api-key")
	}
	if a.Protocol != "" && a.Protocol != "responses" && a.Protocol != "chat" && a.Protocol != "anthropic" {
		return errors.New("不支持的上游接口协议")
	}
	if a.Wire != "" && a.Wire != "auto" && a.Wire != "responses" && a.Wire != "chat" && a.Wire != "anthropic" {
		return errors.New("不支持的接口类型")
	}
	if !ValidAccountSpeedMode(a.SpeedMode) {
		return errors.New("不支持的速度档位")
	}
	a.Workspace = strings.TrimSpace(a.Workspace)
	if a.Workspace != "" && !filepath.IsAbs(a.Workspace) {
		return errors.New("项目文件夹需要绝对路径")
	}
	return nil
}

func normalizeModelRules(in []string) ([]string, error) {
	if len(in) > 1000 {
		return nil, errors.New("模型规则过多")
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		model := strings.TrimSpace(raw)
		if model == "" {
			continue
		}
		if len(model) > 200 || strings.ContainsAny(model, "\r\n\x00") {
			return nil, errors.New("模型规则包含无效名称")
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		out = append(out, model)
	}
	return out, nil
}

// ModelAllowed applies account-local visibility rules. A deny rule always
// wins. This is used both during pool selection and before forwarding a
// request, so a stale or manually crafted model ID cannot bypass the card's
// model policy.
func ModelAllowed(a LocalAccount, model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	for _, denied := range a.ModelDenylist {
		if denied == model {
			return false
		}
	}
	if len(a.ModelAllowlist) == 0 {
		return true
	}
	for _, allowed := range a.ModelAllowlist {
		if allowed == model {
			return true
		}
	}
	return false
}
