package hubclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/launcher"
)

const verifiedModelCatalogTTL = 5 * time.Minute

type verifiedModelCatalog struct {
	fingerprint string
	models      []string
	expiresAt   time.Time
	epoch       uint64
}

// Catalog identity is the actual vault connection, not a transient tool copy
// whose Kind/Model/Protocol can change for protocol conversion.
func catalogAccountFingerprint(a config.LocalAccount) string {
	parts := []string{a.Kind, a.BaseURL, a.Key, a.AuthMode, a.Wire}
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])
}

func (c *Client) invalidateAPIModels(id string) {
	c.modelCatalogMu.Lock()
	defer c.modelCatalogMu.Unlock()
	if c.modelCatalog == nil {
		c.modelCatalog = map[string]verifiedModelCatalog{}
	}
	c.modelCatalogEpoch++
	c.modelCatalog[id] = verifiedModelCatalog{epoch: c.modelCatalogEpoch}
}

// verifiedAPIModels only trusts a successful GET /models made in this process
// for this key/connection. Stored Models can belong to old credentials. Cache
// hits avoid another provider request per message; explicit refresh invalidates
// first, so a failed refresh cannot silently fall back to the previous list.
func (c *Client) verifiedAPIModels(ctx context.Context, account config.LocalAccount, refresh bool) ([]string, error) {
	if c.o.Store == nil {
		return nil, errors.New("程序还在启动，请稍后再试")
	}
	fingerprint := catalogAccountFingerprint(account)
	current, exists := c.o.Store.Get().LocalAccount(account.ID)
	if !exists || catalogAccountFingerprint(current) != fingerprint {
		return nil, errors.New("API 已更换，请刷新模型后重试")
	}
	c.modelCatalogMu.Lock()
	if c.modelCatalog == nil {
		c.modelCatalog = map[string]verifiedModelCatalog{}
	}
	cached := c.modelCatalog[account.ID]
	if !refresh && cached.fingerprint == fingerprint && len(cached.models) > 0 && time.Now().Before(cached.expiresAt) {
		models := append([]string(nil), cached.models...)
		c.modelCatalogMu.Unlock()
		return models, nil
	}
	c.modelCatalogEpoch++
	epoch := c.modelCatalogEpoch
	c.modelCatalog[account.ID] = verifiedModelCatalog{fingerprint: fingerprint, epoch: epoch}
	c.modelCatalogMu.Unlock()

	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	models, err := launcher.Models(rctx, account)
	if err != nil {
		// The desktop library permits manual model entry, but this phone path
		// deliberately requires a verified catalog. Do not offer a dead-end action.
		detail := strings.SplitN(strings.SplitN(err.Error(), "；", 2)[0], "，", 2)[0]
		return nil, fmt.Errorf("读取模型列表失败，请刷新模型或换 API：%s", detail)
	}
	// Do not publish an older in-flight request after a refresh or API switch.
	c.modelCatalogMu.Lock()
	defer c.modelCatalogMu.Unlock()
	if c.modelCatalog[account.ID].epoch != epoch {
		return nil, errors.New("API 或模型列表已更新，请刷新后重试")
	}
	if err := c.o.Store.Update(func(cfg *config.Config) error {
		for i := range cfg.LocalAccounts {
			if cfg.LocalAccounts[i].ID == account.ID {
				if catalogAccountFingerprint(cfg.LocalAccounts[i]) != fingerprint {
					return errors.New("API 已更换，请刷新模型后重试")
				}
				cfg.LocalAccounts[i].Models = append([]string(nil), models...)
				return nil
			}
		}
		return errors.New("API 已删除，请重新选择 API")
	}); err != nil {
		return nil, err
	}
	c.modelCatalog[account.ID] = verifiedModelCatalog{
		fingerprint: fingerprint, models: append([]string(nil), models...), expiresAt: time.Now().Add(verifiedModelCatalogTTL), epoch: epoch,
	}
	return models, nil
}

// Only actually applied APIs (or an explicit phone selection) manage the worker
// connection. A pending tool-card choice must not replace the tool's own login.
func managedModelAccount(cfg config.Config, family string) (config.LocalAccount, bool, error) {
	if choice, set := cfg.RemoteAPI[family]; set && choice.AccountID != "" {
		a, ok := cfg.RemoteToolAccount(family)
		if !ok {
			return config.LocalAccount{}, true, errors.New("API 已删除，请重新选择 API")
		}
		return a, true, nil
	}
	if _, set := cfg.ToolAPIApplied[config.ToolFamily(family)]; set {
		a, ok := cfg.AppliedToolAccount(family)
		if !ok {
			return config.LocalAccount{}, true, errors.New("电脑 API 已更换，请重新选择 API")
		}
		return a, true, nil
	}
	return config.LocalAccount{}, false, nil
}

// validateManagedModel runs before any background start/resume/send. It never
// substitutes an earlier thread's model or legacy Config defaults after a key
// switch, and never starts a worker just to validate a model.
func (c *Client) validateManagedModel(ctx context.Context, cfg config.Config, family, model string) (bool, error) {
	account, managed, err := managedModelAccount(cfg, family)
	if err != nil || !managed {
		return managed, err
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return true, errors.New("请先选择当前 API 支持的模型")
	}
	if len(model) > 200 || strings.ContainsAny(model, "\r\n\x00") {
		return true, errors.New("模型名称无效")
	}
	vault, exists := cfg.LocalAccount(account.ID)
	if !exists {
		return true, errors.New("API 已删除，请重新选择 API")
	}
	models, err := c.verifiedAPIModels(ctx, vault, false)
	if err != nil {
		return true, err
	}
	latest := c.o.Store.Get()
	current, stillManaged, err := managedModelAccount(latest, family)
	if err != nil {
		return true, err
	}
	if !stillManaged || current.ID != account.ID || config.AccountFingerprint(current) != config.AccountFingerprint(account) {
		return true, errors.New("API 已更换，请刷新模型后重试")
	}
	if !containsKey(models, model) {
		return true, errors.New("这个 API 不支持该模型，请刷新后重选模型")
	}
	return true, nil
}
