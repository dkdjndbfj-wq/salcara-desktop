package config

import (
	"reflect"
	"strings"
)

var gatewayAuthorizationTargets = [...]string{"codex", "claude", "claude-desktop", "remote-codex", "remote-claude", "deepseek-harness", "hermes", "remote-deepseek-harness", "remote-hermes", "direct-codex", "direct-claude", "direct-deepseek-harness", "direct-hermes"}

// GatewaySourceAccount is the single source resolver used by the listener and
// its authorization epochs. Phone-selected workers never inherit the local pool.
func (c Config) GatewaySourceAccount(target string) (LocalAccount, bool) {
	if family, direct := strings.CutPrefix(target, "direct-"); direct {
		if !c.DirectRelayEnabled || c.DirectPhoneID == "" || c.DirectPhoneHash == "" || c.GatewayRuntimeKey == "" {
			return LocalAccount{}, false
		}
		return c.DirectToolAccount(family)
	}
	if family, remote := strings.CutPrefix(target, "remote-"); remote {
		selected, ok := c.RemoteToolAccount(family)
		if !ok {
			selected, ok = c.AuthorizedAppliedToolAccount(family)
		}
		if !ok {
			return LocalAccount{}, false
		}
		vault, found := c.LocalAccount(selected.ID)
		if !found || !c.RemoteStationAllows(vault) {
			return LocalAccount{}, false
		}
		return selected, true
	}
	return c.AuthorizedAppliedToolAccount(target)
}

// GatewayAuthorizationRevision changes on committed route authorization changes,
// including revocation followed by restoration. It is not a credential and must
// be captured before reading a body or waiting for discovery/bootstrap.
func (c Config) GatewayAuthorizationRevision(target string) string {
	return c.GatewayAuthEpochs[target]
}

type gatewayPoolAuthorization struct {
	Fingerprint, VaultFingerprint string
	Priority, Weight, Concurrency int
}
type gatewayRouteAuthorization struct {
	Credential, AccountID, Fingerprint, VaultFingerprint string
	Authorized, BindingPresent, RemoteChoicePresent      bool
	Binding                                              AppliedAPI
	RemoteChoice                                         RemoteAPI
	RemoteArmed                                          bool
	RemoteIdentity, PhoneBindingID, PhoneHash            string
	Pool                                                 map[string]gatewayPoolAuthorization
}

func (c Config) gatewayRouteAuthorization(target string) gatewayRouteAuthorization {
	state := gatewayRouteAuthorization{Credential: c.GatewayCredential()}
	family, remote := strings.CutPrefix(target, "remote-")
	directFamily, direct := strings.CutPrefix(target, "direct-")
	if direct {
		family = directFamily
		state.Credential = c.DirectGatewayCredential(target)
		state.RemoteArmed = c.DirectRelayEnabled
		state.RemoteIdentity, state.PhoneBindingID, state.PhoneHash = c.DeviceID, c.DirectPhoneID, c.DirectPhoneHash
		state.RemoteChoice, state.RemoteChoicePresent = c.DirectAPI[family]
	} else if remote {
		state.Credential = c.RemoteGatewayCredential(target)
		state.RemoteArmed = c.RemoteExecutionArmed
		state.RemoteIdentity, state.PhoneBindingID, state.PhoneHash = RemoteIdentity(c), c.PhoneBindingID, c.PhoneHash
		state.RemoteChoice, state.RemoteChoicePresent = c.RemoteAPI[family]
	} else {
		family = target
	}
	if !direct && (!remote || !state.RemoteChoicePresent || state.RemoteChoice.AccountID == "") {
		state.Binding, state.BindingPresent = c.ToolAPIApplied[ToolFamily(family)]
	}
	selected, authorized := c.GatewaySourceAccount(target)
	state.Authorized = authorized
	if !authorized {
		return state
	}
	state.AccountID, state.Fingerprint = selected.ID, AccountFingerprint(selected)
	vault, _ := c.LocalAccount(selected.ID)
	state.VaultFingerprint = AccountFingerprint(vault)
	if !remote && !direct && selected.PoolEnabled {
		state.Pool = make(map[string]gatewayPoolAuthorization)
		for _, account := range c.LocalAccounts {
			if !account.PoolEnabled || strings.TrimSpace(account.PoolGroup) == "" || selected.PoolGroup != "" && account.PoolGroup != selected.PoolGroup {
				continue
			}
			state.Pool[account.ID] = gatewayPoolAuthorization{
				Fingerprint: AccountFingerprint(c.APIForTool(account, family)), VaultFingerprint: AccountFingerprint(account),
				Priority: account.Priority, Weight: account.Weight, Concurrency: account.MaxConcurrency,
			}
		}
	}
	return state
}

func fenceGatewayAuthorization(old Config, next *Config) {
	// Credentials include the epoch. Compare both configurations under the
	// Store's old epochs so callback-supplied values cannot trigger rotation.
	// This view only reads the old map; the committed map is allocated below.
	candidate := *next
	candidate.GatewayAuthEpochs = old.GatewayAuthEpochs
	epochs := make(map[string]string, len(gatewayAuthorizationTargets))
	for _, target := range gatewayAuthorizationTargets {
		epoch := old.GatewayAuthEpochs[target]
		if epoch == "" || !reflect.DeepEqual(old.gatewayRouteAuthorization(target), candidate.gatewayRouteAuthorization(target)) {
			epoch = RandomToken(32)
		}
		epochs[target] = epoch
	}
	next.GatewayAuthEpochs = epochs
}
