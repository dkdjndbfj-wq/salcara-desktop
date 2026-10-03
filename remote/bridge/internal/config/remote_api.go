package config

import (
	"errors"
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
