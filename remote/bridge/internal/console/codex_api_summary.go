package console

import (
	"fmt"
	"sort"
	"time"

	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/config"
)

// This projection is cache-only, not an inference/entitlement probe. A configured
// API key without a current provider observation must never become "available".
type apiServiceSummary struct {
	CheckedAt int64              `json:"checkedAt"`
	Members   []apiSummaryMember `json:"members"`
}

type apiServiceCostEstimate struct {
	LocalUsedUSD  *float64   `json:"localUsedUSD"`
	WindowUsedUSD *float64   `json:"windowUsedUSD"`
	TotalUSD      *float64   `json:"totalUSD"`
	RemainingUSD  *float64   `json:"remainingUSD"`
	Status        string     `json:"status"`
	KnownAccounts int        `json:"knownAccounts"`
	TotalAccounts int        `json:"totalAccounts"`
	FreshUntil    *time.Time `json:"freshUntil,omitempty"`
	Scope         string     `json:"scope"`
}

type apiSummaryMember struct {
	ID           string                       `json:"id"`
	Name         string                       `json:"name"`
	SourceType   string                       `json:"sourceType"`
	Availability string                       `json:"availability"`
	Reason       string                       `json:"reason"`
	Usage        *codexaccounts.UsageSnapshot `json:"usage,omitempty"`
	CostEstimate *officialCostEstimate        `json:"-"` // legacy calculator input only
}

func (s *Server) readAPIServiceSummary(c config.Config) apiServiceSummary {
	out := apiServiceSummary{CheckedAt: time.Now().UnixMilli(), Members: []apiSummaryMember{}}
	// A member can be valid through one client even if another client has a
	// broken pool. Empty image pools use the anchor only when images are enabled.
	members := map[string]bool{}
	textMembers := map[string]bool{}
	add := func(ids []string) {
		valid := true
		for _, id := range ids {
			a, ok := c.LocalAccount(id)
			if !ok || config.ValidateLocalAccount(&a) != nil {
				valid = false
			}
		}
		for _, id := range ids {
			members[id] = members[id] || valid
		}
	}
	for _, client := range c.CodexAPI.Clients {
		if client.Disabled || config.ValidateAPIClient(client) != nil {
			continue
		}
		if c.CodexAPI.ImagesMode != "images_only" {
			ids := client.AccountIDs
			if len(ids) == 0 {
				ids = []string{client.AccountID}
			}
			if client.PoolStrategy == config.PoolStrategySingleAccount {
				// Validate the full configured pool, but do not count non-selected
				// members as executable capacity in fixed-account mode.
				valid := true
				for _, id := range ids {
					a, ok := c.LocalAccount(id)
					valid = valid && ok && config.ValidateLocalAccount(&a) == nil
				}
				members[client.PoolAccountID] = members[client.PoolAccountID] || valid
				textMembers[client.PoolAccountID] = true
			} else {
				add(ids)
				for _, id := range ids {
					textMembers[id] = true
				}
			}
		}
		if config.CodexAPIImagesEnabled(c.CodexAPI.ImagesMode) {
			ids := client.ImageAccountIDs
			if len(ids) == 0 {
				ids = []string{client.AccountID}
			}
			add(ids)
		}
	}
	ids := make([]string, 0, len(members))
	for id := range members {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	seen := map[string]int{}
	for _, id := range ids {
		a, exists := c.LocalAccount(id)
		row := apiSummaryMember{ID: id, Name: a.Name, SourceType: a.SourceType, Availability: "unavailable", Reason: "account_missing"}
		if !exists {
			out.Members = append(out.Members, row)
			continue
		}
		if row.SourceType == "" {
			row.SourceType = "api_key"
		}
		identity := "account:" + id
		if a.SourceType == "official_account" && a.CredentialRef != "" {
			identity = fmt.Sprintf("official:%s:%d", a.CredentialRef, a.CredentialEpoch)
		}
		if !members[id] {
			row.Reason = "pool_configuration_invalid"
		} else {
			probe := config.NamedAPIClient{ID: "summary-probe", Name: "summary", AccountID: id, KeyDigest: config.APIClientDigest("unused-summary-probe")}
			ready := s.apiClientReadiness(c, probe)
			row.Reason = ready.State
			if ready.State == "configured" {
				row.Availability, row.Reason = "unknown", "upstream_not_verified"
			}
			if a.SourceType == "official_account" && s.d.CodexAccounts != nil {
				account, err := s.d.CodexAccounts.Get(a.CredentialRef)
				if err == nil && account.IdentityEpoch == a.CredentialEpoch {
					usage := account.Usage
					row.Usage = &usage
					quota := s.d.CodexAccounts.PoolRouting(a.CredentialRef, a.CredentialEpoch, "")
					latest, latestErr := s.d.CodexAccounts.Get(a.CredentialRef)
					if latestErr != nil || latest.Epoch != account.Epoch || latest.IdentityEpoch != account.IdentityEpoch {
						row.Usage = nil
						row.Availability, row.Reason = "unknown", "credential_changed"
					} else if ready.State == "configured" && quota.Current && quota.QuotaFresh && textMembers[id] {
						if quota.QuotaAllowed && quota.RemainingPercent > 0 {
							row.Availability, row.Reason = "ready", "verified_quota"
						} else {
							row.Availability, row.Reason = "unavailable", "quota_exhausted"
						}
					}
				}
			}
			if s.apiService != nil && s.apiService.Pool != nil {
				for _, health := range s.apiService.Pool.Snapshot([]config.LocalAccount{a}) {
					if health.CoolingDown {
						row.Availability, row.Reason = "unavailable", "cooling_down"
					}
				}
			}
		}
		if index, duplicate := seen[identity]; duplicate {
			// Repeated handles for one protected credential are one account.
			// Prefer its usable route over a broken duplicate membership.
			if out.Members[index].Reason == "pool_configuration_invalid" && row.Reason != "pool_configuration_invalid" || row.Reason == "verified_quota" || row.Reason == "quota_exhausted" {
				out.Members[index] = row
			}
			continue
		}
		seen[identity] = len(out.Members)
		out.Members = append(out.Members, row)
	}
	return out
}

// Members have already been deduplicated by protected identity, even when
// several local handles/clients point at the same credential. Unknown capacity
// never becomes zero. Different quota windows are explicitly not one period.
func sumAPIServiceCosts(members []apiSummaryMember, now time.Time) apiServiceCostEstimate {
	out := apiServiceCostEstimate{Status: "unknown", TotalAccounts: len(members), Scope: "account_windows"}
	local, total, used, remaining := float64(0), float64(0), float64(0), float64(0)
	localKnown := 0
	partial := false
	for _, row := range members {
		c := row.CostEstimate
		if c == nil {
			continue
		}
		if c.LocalUsedUSD != nil && finiteCost(*c.LocalUsedUSD) {
			local += *c.LocalUsedUSD
			localKnown++
		}
		if c.TotalUSD == nil || c.WindowUsedUSD == nil || c.RemainingUSD == nil || c.FreshUntil == nil || !c.FreshUntil.After(now) || !finiteCost(*c.TotalUSD) || !finiteCost(*c.WindowUsedUSD) || !finiteCost(*c.RemainingUSD) {
			continue
		}
		out.KnownAccounts++
		total += *c.TotalUSD
		used += *c.WindowUsedUSD
		remaining += *c.RemainingUSD
		partial = partial || c.Status != "estimated"
		if out.FreshUntil == nil || c.FreshUntil.Before(*out.FreshUntil) {
			copy := *c.FreshUntil
			out.FreshUntil = &copy
		}
	}
	if localKnown > 0 && finiteCost(local) {
		out.LocalUsedUSD = &local
	}
	if out.KnownAccounts > 0 {
		out.Status = "partial"
	}
	if out.TotalAccounts > 0 && out.KnownAccounts == out.TotalAccounts && finiteCost(total) && finiteCost(used) && finiteCost(remaining) {
		out.TotalUSD, out.WindowUsedUSD, out.RemainingUSD = &total, &used, &remaining
		if !partial {
			out.Status = "estimated"
		}
	}
	return out
}
