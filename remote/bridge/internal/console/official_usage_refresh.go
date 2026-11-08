package console

import (
	"sort"
	"strings"
	"sync"

	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/config"
)

type officialUsageMaintenance struct {
	mu     sync.Mutex
	closed bool
}

func (s *Server) initOfficialUsageMaintenance() {
	if s.d.CodexAccounts == nil || s.d.Store == nil {
		return
	}
	s.officialUsageMaintenance = &officialUsageMaintenance{}
	s.d.Store.OnChange(func(_, _ config.Config) { s.syncOfficialUsageMaintenance() })
	s.syncOfficialUsageMaintenance()
}

func (s *Server) syncOfficialUsageMaintenance() {
	m := s.officialUsageMaintenance
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		// Store callbacks may arrive out of order after concurrent commits.
		// Serialize reconciliation and read the current committed config here.
		s.d.CodexAccounts.SetUsageRefreshTargets(officialUsageRefreshTargets(s.d.Store.Get()))
	}
}

func (s *Server) closeOfficialUsageMaintenance() {
	m := s.officialUsageMaintenance
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	s.d.CodexAccounts.SetUsageRefreshTargets(nil)
}

// Only active routes that consume quota/plan observations opt into provider
// reads. Library membership, a pending selection and persisted Enabled alone
// are not authorization to maintain every imported account in the background.
func officialUsageRefreshTargets(c config.Config) []codexaccounts.UsageRefreshTarget {
	selected := make(map[codexaccounts.UsageRefreshTarget]struct{})
	add := func(account config.LocalAccount, metrics bool) {
		if account.SourceType == "official_account" && account.CredentialRef != "" && account.CredentialEpoch != 0 && (metrics || account.QuotaReservePercent > 0) {
			selected[codexaccounts.UsageRefreshTarget{ID: account.CredentialRef, IdentityEpoch: account.CredentialEpoch}] = struct{}{}
		}
	}
	if c.CodexAPIActive() {
		for _, client := range c.CodexAPI.Clients {
			if client.Disabled || config.ValidateAPIClient(client) != nil {
				continue
			}
			if c.CodexAPI.ImagesMode != "images_only" {
				if len(client.AccountIDs) == 0 {
					if account, ok := c.LocalAccount(client.AccountID); ok {
						add(account, false)
					}
				} else {
					members := make([]config.LocalAccount, 0, len(client.AccountIDs))
					valid := true
					for _, id := range client.AccountIDs {
						account, ok := c.LocalAccount(id)
						valid = valid && ok
						members = append(members, account)
					}
					strategy := client.PoolStrategy
					if strategy == "" {
						strategy = config.PoolStrategyAuto
					}
					if valid {
						for _, account := range members {
							if strategy != config.PoolStrategySingleAccount || account.ID == client.PoolAccountID {
								add(account, quotaRoutingStrategy(strategy))
							}
						}
					}
				}
			}
			if config.CodexAPIImagesEnabled(c.CodexAPI.ImagesMode) && config.ValidateCodexAPIImages(c.CodexAPI.ImagesMode, c.CodexAPI.MaxConcurrentImageRequests) == nil {
				ids := client.ImageAccountIDs
				if len(ids) == 0 {
					ids = []string{client.AccountID}
				}
				for _, id := range ids {
					if account, ok := c.LocalAccount(id); ok {
						add(account, false)
					}
				}
			}
		}
	}
	for target := range c.ToolAPIApplied {
		anchor, authorized := c.AuthorizedAppliedToolAccount(target)
		if !authorized || c.LocalGatewayCredential(target) == "" {
			continue
		}
		if !anchor.PoolEnabled {
			add(anchor, false)
			continue
		}
		if !config.ValidPoolStrategy(anchor.PoolStrategy) {
			continue
		}
		for _, stored := range c.LocalAccounts {
			account := c.APIForTool(stored, target)
			if !account.PoolEnabled || strings.TrimSpace(account.PoolGroup) == "" || anchor.PoolGroup != "" && account.PoolGroup != anchor.PoolGroup {
				continue
			}
			if anchor.PoolStrategy == config.PoolStrategySingleAccount && account.ID != anchor.ID {
				continue
			}
			add(account, quotaRoutingStrategy(anchor.PoolStrategy))
		}
	}
	if c.RemoteExecutionArmed && c.GatewayRuntimeKey != "" {
		families := make(map[string]struct{})
		for family := range c.RemoteAPI {
			families[family] = struct{}{}
		}
		for family := range c.ToolAPIApplied {
			families[family] = struct{}{}
		}
		for family := range families {
			target := config.RemoteGatewayTarget(family)
			if account, ok := c.GatewaySourceAccount(target); ok && c.RemoteGatewayCredential(target) != "" {
				add(account, false) // Remote source proofs never authorize a pool.
			}
		}
	}
	result := make([]codexaccounts.UsageRefreshTarget, 0, len(selected))
	for target := range selected {
		result = append(result, target)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ID == result[j].ID {
			return result[i].IdentityEpoch < result[j].IdentityEpoch
		}
		return result[i].ID < result[j].ID
	})
	return result
}

func quotaRoutingStrategy(strategy string) bool {
	switch strategy {
	case config.PoolStrategyAuto, config.PoolStrategyQuotaHighFirst, config.PoolStrategyQuotaLowFirst,
		config.PoolStrategyPlanHighFirst, config.PoolStrategyPlanLowFirst, config.PoolStrategyExpirySoonFirst:
		return true
	default:
		return false
	}
}
