package config

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

func validDirectFamily(family string) bool {
	return ValidRemoteFamily(family) || IsAdditionalTool(family)
}

// DirectGatewayTarget is exclusive to a pinned phone's Bridge-owned workers.
func DirectGatewayTarget(family string) string { return "direct-" + family }

// DirectToolAccount resolves only a saved direct choice. An absent choice means
// follow the computer; an invalid explicit choice must not inherit a Hub choice.
func (c Config) DirectToolAccount(family string) (LocalAccount, bool) {
	if !validDirectFamily(family) {
		return LocalAccount{}, false
	}
	// Reuse the account/model adaptation without changing either selection map.
	c.RemoteAPI = c.DirectAPI
	return c.RemoteToolAccount(family)
}

// SetDirectAPIForAgent affects worker selection only, never native tool files,
// applied tool bindings, Hub authorization, or the Hub's selected account.
func (c *Config) SetDirectAPIForAgent(agent, accountID, model string) error {
	family := NormalizeAgentFamily(agent)
	if !validDirectFamily(family) {
		return errors.New("不支持的直连 Agent")
	}
	model = strings.TrimSpace(model)
	if len(model) > 200 || strings.ContainsAny(model, "\r\n\x00") {
		return errors.New("模型名称无效")
	}
	if accountID == "" {
		delete(c.DirectAPI, family)
		return nil
	}
	if _, ok := c.LocalAccount(accountID); !ok {
		return errors.New("电脑上没有这个 API，请刷新后重选")
	}
	if c.DirectAPI == nil {
		c.DirectAPI = map[string]RemoteAPI{}
	}
	c.DirectAPI[family] = RemoteAPI{AccountID: accountID, Model: model}
	return nil
}

// DirectGatewayCredential retires on source/model changes, pairing revocation,
// A -> B -> A switches, and Bridge restarts. It never uses the persisted key.
func (c Config) DirectGatewayCredential(target string) string {
	family, direct := strings.CutPrefix(target, "direct-")
	if !direct || !validDirectFamily(family) || c.GatewayRuntimeKey == "" || c.GatewayAuthorizationRevision(target) == "" ||
		!c.DirectRelayEnabled || c.DirectPhoneID == "" || c.DirectPhoneHash == "" {
		return ""
	}
	a, ok := c.DirectToolAccount(family)
	if !ok {
		return ""
	}
	vault, ok := c.LocalAccount(a.ID)
	if !ok {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(c.GatewayRuntimeKey))
	_, _ = mac.Write([]byte(strings.Join([]string{"salcara-direct-route-v1", target, c.GatewayAuthorizationRevision(target),
		a.ID, AccountFingerprint(vault), AccountFingerprint(a), c.DeviceID, c.DirectPhoneID, c.DirectPhoneHash}, "\x00")))
	return "dt-salcara-route-" + hex.EncodeToString(mac.Sum(nil))
}
