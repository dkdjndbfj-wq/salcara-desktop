package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

// LocalAccount is one API-library entry, independent of tool-card selections.
type LocalAccount struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Kind       string   `json:"kind"` // api; codex/claude retained only for legacy migration
	BaseURL    string   `json:"baseUrl"`
	Key        string   `json:"key"`
	Model      string   `json:"model,omitempty"`
	Models     []string `json:"models,omitempty"`
	AuthMode   string   `json:"authMode,omitempty"` // bearer or api-key for Anthropic
	Workspace  string   `json:"workspace,omitempty"`
	Target     string   `json:"target,omitempty"` // most recently selected launch target
	CreatedAt  int64    `json:"createdAt"`
	LastUsedAt int64    `json:"lastUsedAt,omitempty"`
	Protocol   string   `json:"protocol,omitempty"` // effective upstream wire protocol, set on tool copies only
	// Wire is the provider's interface type: "" / "auto" picks per model
	// (claude → Messages, gpt/o-series → Responses, others → Chat Completions).
	Wire string `json:"wire,omitempty"`
	// CatalogOverride is set on tool copies only: publish Models into the tool's
	// own model picker (never persisted on the vault entry).
	CatalogOverride    bool              `json:"-"`
	DesktopModelLabels map[string]string `json:"-"` // transient desktop compatibility routes; never stored in the vault
}

// AppliedAPI records a reference, not another copy of the API key.
type AppliedAPI struct {
	AccountID   string `json:"accountId"`
	Fingerprint string `json:"fingerprint"`
	Provider    string `json:"provider,omitempty"`
	Model       string `json:"model,omitempty"`
	Protocol    string `json:"protocol,omitempty"`
	Catalog     bool   `json:"catalog,omitempty"`
}

func AccountFingerprint(a LocalAccount) string {
	parts := []string{a.Kind, a.BaseURL, a.Key, a.Model, a.AuthMode, a.Protocol}
	if a.Wire != "" && a.Wire != "auto" {
		// Empty and auto are equivalent and keep pre-existing auto snapshots valid.
		// Explicit interface changes must invalidate the previously applied route.
		parts = append(parts, "wire:"+a.Wire)
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

func (c Config) SelectedToolAccount(target string) (LocalAccount, bool) {
	if id, found := c.ToolAPISelections[target]; found {
		return c.LocalAccount(id)
	}
	return c.ActiveLocalAccount(strings.Split(target, "-")[0])
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
	if a.Protocol == "" {
		a.Protocol = NativeProtocol(a.Kind)
	}
	return a, found && b.Fingerprint == AccountFingerprint(a)
}

// APIForTool adapts a vault entry without assigning that key to a tool or brand.
func (c Config) APIForTool(a LocalAccount, target string) LocalAccount {
	a.Kind = strings.Split(target, "-")[0]
	a.Target = target
	if selected, ok := c.SelectedToolAccount(target); ok && selected.ID == a.ID {
		if model, set := c.ToolModels[target]; set {
			a.Model = model
		}
	}
	if protocol, set := c.ToolProtocols[target]; set {
		a.Protocol = protocol
	}
	a.CatalogOverride = (a.Kind == "codex" || target == "claude-desktop") && c.ToolCatalogOverride[target]
	if a.Protocol == "" {
		a.Protocol = NativeProtocol(a.Kind)
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
	a.Kind, a.Target = strings.Split(target, "-")[0], target
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
	c.ToolAPIApplied[ToolFamily(target)] = AppliedAPI{AccountID: a.ID, Fingerprint: AccountFingerprint(a), Provider: provider, Model: a.Model, Protocol: a.Protocol, Catalog: a.CatalogOverride}
}

var accountID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

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
	if a.Kind != "api" && a.Kind != "codex" && a.Kind != "claude" {
		return errors.New("无效的密钥条目")
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
	root, _, err := APIBase(a.BaseURL)
	if err != nil {
		return err
	}
	a.BaseURL = root
	if a.AuthMode == "" {
		a.AuthMode = "bearer"
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
	a.Workspace = strings.TrimSpace(a.Workspace)
	if a.Workspace != "" && !filepath.IsAbs(a.Workspace) {
		return errors.New("项目文件夹需要绝对路径")
	}
	return nil
}
