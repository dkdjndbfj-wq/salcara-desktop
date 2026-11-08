package hubclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

const (
	// Both sources are hints only: neither proves official-account entitlement.
	OfficialNativeCatalogSource = "codex-native-picker-hint"
	OfficialSavedCatalogSource  = "saved-official-picker-hint"
)

func isOfficialModelAccount(a config.LocalAccount) bool {
	return a.CredentialRef != "" || a.SourceType == "official_account"
}

func validateOfficialModel(a config.LocalAccount, model string) error {
	if a.CredentialRef == "" || a.SourceType != "official_account" || a.ProviderID != "openai" {
		return errors.New("官方账号引用无效")
	}
	model = strings.TrimSpace(model)
	if model == "" || len(model) > 200 || strings.ContainsAny(model, "\r\n\x00") {
		return errors.New("模型名称无效")
	}
	if !config.ModelAllowed(a, model) {
		return errors.New("该模型不符合当前账号的模型允许/拒绝规则")
	}
	return nil
}

// Bind a native read to its configuration, without treating saved catalogue
// hints as configuration changes caused by our own successful publication.
func officialCatalogRuntimeFingerprint(cfg config.Config) string {
	copy := cfg.Clone()
	for i := range copy.LocalAccounts {
		copy.LocalAccounts[i].Models = nil
	}
	snapshot := struct {
		Settings agents.Settings
		Applied  config.AppliedAPI
		Remote   config.RemoteAPI
	}{agents.SettingsFromConfig(copy), copy.ToolAPIApplied["codex"], copy.RemoteAPI["codex"]}
	data, _ := json.Marshal(snapshot)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func officialCatalogHints(models []string, account config.LocalAccount, source string) (protocol.ModelCatalog, error) {
	if len(models) > 1000 {
		return protocol.ModelCatalog{}, errors.New("原生 Codex 模型目录过大")
	}
	clean, seen := []string{}, map[string]bool{}
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" || len(model) > 200 || strings.ContainsAny(model, "\r\n\x00") || seen[model] || !config.ModelAllowed(account, model) {
			continue
		}
		seen[model] = true
		clean = append(clean, model)
	}
	return protocol.ModelCatalog{Models: clean, ModelCapabilities: protocol.UnknownModelCapabilities(clean, source)}, nil
}

// OfficialModelCatalog reads shared native Codex picker hints, never /models
// with a protected credential marker or a real account token. The native CLI
// may describe a shared catalogue; this is deliberately not entitlement proof.
// It never selects/applies an account, starts a turn, or falls back to another
// account. Ordinary reads may use saved hints; refresh must read model/list.
func (c *Client) OfficialModelCatalog(ctx context.Context, account config.LocalAccount, refresh bool) (protocol.ModelCatalog, error) {
	if err := c.validateStationUpstream(ctx, account); err != nil {
		return protocol.ModelCatalog{}, err
	}
	if c.o.Store == nil {
		return protocol.ModelCatalog{}, errors.New("程序还在启动，请稍后再试")
	}
	if !isOfficialModelAccount(account) || account.CredentialRef == "" || account.SourceType != "official_account" || account.ProviderID != "openai" {
		return protocol.ModelCatalog{}, errors.New("官方账号引用无效")
	}
	if ctx.Err() != nil {
		return protocol.ModelCatalog{}, errors.New("原生 Codex 模型目录读取已取消")
	}
	cfg := executionConfig(ctx, c.o.Store.Get())
	fingerprint := catalogAccountFingerprint(account)
	current, exists := cfg.LocalAccount(account.ID)
	if !exists || catalogAccountFingerprint(current) != fingerprint {
		return protocol.ModelCatalog{}, errors.New("官方账号已更换，请刷新模型后重试")
	}
	runtimeFingerprint := officialCatalogRuntimeFingerprint(cfg)
	c.modelCatalogMu.Lock()
	if c.modelCatalog == nil {
		c.modelCatalog = map[string]verifiedModelCatalog{}
	}
	cached := c.modelCatalog[account.ID]
	matching := cached.fingerprint == fingerprint && cached.runtimeFingerprint == runtimeFingerprint
	if !refresh && matching && cached.source != "" && time.Now().Before(cached.expiresAt) {
		catalog, err := officialCatalogHints(cached.models, current, cached.source)
		c.modelCatalogMu.Unlock()
		return catalog, err
	}
	// A failed explicit refresh must not resurrect a saved previous result.
	suppressStored := refresh || (matching && cached.suppressStoredHints)
	c.modelCatalogEpoch++
	epoch := c.modelCatalogEpoch
	c.modelCatalog[account.ID] = verifiedModelCatalog{fingerprint: fingerprint, runtimeFingerprint: runtimeFingerprint, epoch: epoch, suppressStoredHints: suppressStored}
	if !suppressStored && len(current.Models) > 0 {
		catalog, err := officialCatalogHints(current.Models, current, OfficialSavedCatalogSource)
		if err == nil {
			c.modelCatalog[account.ID] = verifiedModelCatalog{fingerprint: fingerprint, runtimeFingerprint: runtimeFingerprint, models: append([]string(nil), catalog.Models...), expiresAt: time.Now().Add(verifiedModelCatalogTTL), epoch: epoch, source: OfficialSavedCatalogSource}
		}
		c.modelCatalogMu.Unlock()
		return catalog, err
	}
	c.modelCatalogMu.Unlock()
	manager := c.manager()
	if manager == nil {
		return protocol.ModelCatalog{}, errors.New("原生 Codex 模型目录不可用，请确认 Codex CLI 已安装并已启动")
	}
	agent := manager.Get("codex")
	reader, ok := agent.(agents.ModelCatalogReader)
	if !ok || agent == nil || agent.ID() != "codex" {
		return protocol.ModelCatalog{}, errors.New("原生 Codex 模型目录不可用，当前 Codex CLI 未提供目录读取接口")
	}
	if err := c.validateStationUpstream(ctx, account); err != nil {
		return protocol.ModelCatalog{}, err
	}
	rctx, cancel := context.WithTimeout(withWorkerSnapshot(ctx, cfg), 20*time.Second)
	defer cancel()
	native, err := reader.ModelCatalog(rctx)
	if err != nil || rctx.Err() != nil {
		// Native stderr/RPC errors can contain paths or credentials. Never relay
		// them, and never invoke a provider endpoint as a fallback.
		return protocol.ModelCatalog{}, errors.New("读取原生 Codex 模型目录失败，请确认 Codex CLI 可用后刷新；目录仅作模型选择提示")
	}
	if err := c.validateStationUpstream(ctx, account); err != nil {
		return protocol.ModelCatalog{}, err
	}
	catalog, err := officialCatalogHints(native.Models, current, OfficialNativeCatalogSource)
	if err != nil {
		return protocol.ModelCatalog{}, err
	}
	c.modelCatalogMu.Lock()
	defer c.modelCatalogMu.Unlock()
	if c.modelCatalog[account.ID].epoch != epoch {
		return protocol.ModelCatalog{}, errors.New("官方账号或模型目录已更新，请刷新后重试")
	}
	if err := c.o.Store.Update(func(latest *config.Config) error {
		a, exists := latest.LocalAccount(account.ID)
		if !exists || catalogAccountFingerprint(a) != fingerprint || officialCatalogRuntimeFingerprint(executionConfig(ctx, *latest)) != runtimeFingerprint {
			return errors.New("官方账号或 Codex 配置已更换，请刷新模型后重试")
		}
		for i := range latest.LocalAccounts {
			if latest.LocalAccounts[i].ID == account.ID {
				latest.LocalAccounts[i].Models = append([]string(nil), catalog.Models...)
				break
			}
		}
		return nil
	}); err != nil {
		return protocol.ModelCatalog{}, err
	}
	c.modelCatalog[account.ID] = verifiedModelCatalog{fingerprint: fingerprint, runtimeFingerprint: runtimeFingerprint, models: append([]string(nil), catalog.Models...), expiresAt: time.Now().Add(verifiedModelCatalogTTL), epoch: epoch, source: OfficialNativeCatalogSource}
	return catalog, nil
}
