package config

import (
	"errors"
	"regexp"
	"strings"
)

// RemoteAPI is the API a paired phone picked for tasks the Bridge runs on its
// behalf. It only affects Bridge-run Codex/Claude Code workers; the original
// desktop tools and their applied configuration are never rewritten.
type RemoteAPI struct {
	AccountID string `json:"accountId"`
	Model     string `json:"model,omitempty"`
}

// RemoteFamilies are the agents a phone can drive.
var RemoteFamilies = []string{"codex", "claude"}

func ValidRemoteFamily(f string) bool { return f == "codex" || f == "claude" }

var agentFamilyID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// NormalizeAgentFamily maps a control-surface id to the one API-selection
// namespace used by the Bridge. Only the reserved built-in desktop cards are
// aliases. Extension families retain their exact id, including a -desktop
// suffix, unless their descriptor explicitly names a shared APIFamily.
func NormalizeAgentFamily(agent string) string {
	f := strings.TrimSpace(agent)
	switch f {
	case "codex-desktop":
		return "codex"
	case "claude-desktop":
		return "claude"
	}
	return f
}

// ValidAgentFamily is intentionally broader than ValidRemoteFamily. The
// latter remains the legacy built-in allow-list; this function validates a
// descriptor-provided extension family without allowing path/query syntax.
func ValidAgentFamily(f string) bool {
	f = NormalizeAgentFamily(f)
	return agentFamilyID.MatchString(f) && !strings.EqualFold(f, "remote")
}

// RemoteGatewayTarget is the loopback gateway route for a phone-selected API
// that needs protocol conversion. It never collides with applied tool routes.
func RemoteGatewayTarget(family string) string { return "remote-" + family }

// RemoteToolAccount resolves the phone-selected API for a family.
// ok=false means "follow the computer" (applied tool API or the tool's own login).
func (c Config) RemoteToolAccount(family string) (LocalAccount, bool) {
	choice, set := c.RemoteAPI[family]
	if !set || choice.AccountID == "" {
		return LocalAccount{}, false
	}
	a, found := c.LocalAccount(choice.AccountID)
	if !found {
		return LocalAccount{}, false
	}
	a = c.APIForTool(a, family)
	// An empty phone choice means "choose a model from this API", never
	// "reuse the tool card / vault default from the previous API".
	a.Model = strings.TrimSpace(choice.Model)
	if selected, ok := c.SelectedToolAccount(family); !ok || selected.ID != a.ID {
		// Per-tool protocol choices belong to the account selected on that tool card;
		// otherwise follow the provider's interface type / the model family.
		a.Protocol = NativeProtocol(a.Kind)
		if p := InferProtocol(a.Model, a.Wire); p != "" {
			a.Protocol = p
		}
	}
	return a, true
}

// SetRemoteAPI stores (or clears, with an empty account) the phone choice.
func (c *Config) SetRemoteAPI(family, accountID, model string) error {
	if !ValidRemoteFamily(family) {
		return errors.New("不支持的 Agent")
	}
	return c.setRemoteAPI(family, accountID, model)
}

// SetRemoteAPIForAgent is the aggregation-layer entry point. Built-ins keep
// their historical validation, while a registered adapter can use a stable
// descriptor family (for example Hermes) without adding another config field
// or duplicating API-account logic.
func (c *Config) SetRemoteAPIForAgent(agent, accountID, model string) error {
	family := NormalizeAgentFamily(agent)
	if !ValidAgentFamily(family) {
		return errors.New("不支持的 Agent")
	}
	return c.setRemoteAPI(family, accountID, model)
}

func (c *Config) setRemoteAPI(family, accountID, model string) error {
	model = strings.TrimSpace(model)
	if len(model) > 200 || strings.ContainsAny(model, "\r\n\x00") {
		return errors.New("模型名称无效")
	}
	if accountID == "" {
		delete(c.RemoteAPI, family)
		return nil
	}
	if _, ok := c.LocalAccount(accountID); !ok {
		return errors.New("电脑上没有这个 API，请刷新后重选")
	}
	if c.RemoteAPI == nil {
		c.RemoteAPI = map[string]RemoteAPI{}
	}
	c.RemoteAPI[family] = RemoteAPI{AccountID: accountID, Model: model}
	return nil
}
