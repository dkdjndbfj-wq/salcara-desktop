package apiservice

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/gateway"
)

// namedTextPoolEnabled is deliberately separate from ImageAccountIDs. A named
// client never inherits the local account pool implicitly; only this explicit
// list can be selected for ordinary text requests.
func namedTextPoolEnabled(client config.NamedAPIClient) bool {
	return len(client.AccountIDs) > 0
}

func namedTextPoolGroup(client config.NamedAPIClient) string {
	hash := sha256.Sum256([]byte("named-text-pool\x00" + client.ID + "\x00" + client.AuthEpoch))
	return "named-text-" + hex.EncodeToString(hash[:8])
}

func namedTextPoolPolicy(client config.NamedAPIClient, candidates []config.LocalAccount) gateway.PoolPolicy {
	strategy := strings.TrimSpace(client.PoolStrategy)
	if strategy == "" {
		strategy = config.PoolStrategyAuto
	}
	// AuthEpoch already fences account membership and credentials. Include the
	// normalized candidate fingerprints as well so an embedded caller that did
	// not use Store-owned epochs still retires old sticky bindings.
	hash := sha256.New()
	hash.Write([]byte(client.ID))
	hash.Write([]byte{0})
	hash.Write([]byte(client.AuthEpoch))
	hash.Write([]byte{0})
	hash.Write([]byte(strategy))
	hash.Write([]byte{0})
	hash.Write([]byte(client.PoolAccountID))
	stable := append([]config.LocalAccount(nil), candidates...)
	sort.Slice(stable, func(i, j int) bool { return stable[i].ID < stable[j].ID })
	for _, account := range stable {
		hash.Write([]byte{0})
		hash.Write([]byte(account.ID))
		hash.Write([]byte{0})
		hash.Write([]byte(config.AccountFingerprint(account)))
	}
	return gateway.PoolPolicy{
		Strategy:        strategy,
		SingleAccountID: client.PoolAccountID,
		Revision:        hex.EncodeToString(hash.Sum(nil)),
	}
}

// namedTextPoolCandidates creates a private virtual pool namespace. The pool
// flags are routing metadata only; all credential, model, quota, priority and
// health fields are copied from the authorized vault cards.
func namedTextPoolCandidates(c config.Config, client config.NamedAPIClient) ([]config.LocalAccount, int) {
	if !namedTextPoolEnabled(client) {
		return nil, 0
	}
	group := namedTextPoolGroup(client)
	candidates := make([]config.LocalAccount, 0, len(client.AccountIDs))
	seen := make(map[string]struct{}, len(client.AccountIDs))
	for _, id := range client.AccountIDs {
		if !config.ValidAccountID(id) {
			return nil, 422 // invalid id format — not a conflict, a bad request
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, 422 // duplicate in list — client config error
		}
		seen[id] = struct{}{}
		account, ok := c.LocalAccount(id)
		if !ok {
			// A deleted/replaced explicit member must never silently redirect to
			// another member or the compatibility anchor.
			return nil, 409 // resource gone — genuine conflict with current config
		}
		account = codexDispatchAccount(account)
		account.PoolEnabled = true
		account.PoolGroup = group
		candidates = append(candidates, account)
	}
	if len(candidates) == 0 {
		return nil, 409
	}
	if client.PoolStrategy == config.PoolStrategySingleAccount {
		found := false
		for _, account := range candidates {
			found = found || account.ID == client.PoolAccountID
		}
		if !found {
			return nil, 422 // strategy requires a specific account that is not in the pool
		}
	}
	return candidates, 0
}

func namedTextModelAllowed(candidates []config.LocalAccount, model string) bool {
	for _, account := range candidates {
		if !config.ModelAllowed(account, model) {
			continue
		}
		if account.CredentialRef != "" && account.SourceType == "official_account" && config.ValidCatalogModel(model) {
			return true
		}
		if account.Model == model || len(account.Models) == 0 {
			return true
		}
		for _, candidate := range account.Models {
			if candidate == model {
				return true
			}
		}
	}
	return false
}

// namedTextCatalog is intentionally cache-only. It advertises the union of
// member cards already held by Salcara; a model directory read never performs
// provider I/O or turns an unverified official picker hint into entitlement.
func namedTextCatalog(c config.Config, client config.NamedAPIClient) ([]string, int) {
	candidates, status := namedTextPoolCandidates(c, client)
	if status != 0 {
		return nil, status
	}
	seen := make(map[string]struct{})
	models := make([]string, 0)
	for _, account := range candidates {
		for _, model := range append(append([]string{}, account.Models...), account.Model) {
			model = strings.TrimSpace(model)
			if model == "" || !config.ValidCatalogModel(model) || !config.ModelAllowed(account, model) {
				continue
			}
			if _, exists := seen[model]; exists {
				continue
			}
			seen[model] = struct{}{}
			models = append(models, model)
		}
	}
	sort.Strings(models)
	return models, 0
}

func namedTextSessionKey(r *http.Request, body []byte, socketKey string) string {
	if socketKey = strings.TrimSpace(socketKey); socketKey != "" {
		return socketKey
	}
	if r != nil {
		if value := strings.TrimSpace(r.Header.Get("X-Salcara-Session")); value != "" && len(value) <= 256 {
			return value
		}
	}
	var input struct {
		ConversationID string `json:"conversation_id"`
		SessionID      string `json:"session_id"`
		Previous       string `json:"previous_response_id"`
		User           string `json:"user"`
		Metadata       struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if json.Unmarshal(body, &input) != nil {
		return ""
	}
	for _, value := range []string{input.ConversationID, input.SessionID, input.Metadata.UserID, input.User, input.Previous} {
		value = strings.TrimSpace(value)
		if value != "" && len(value) <= 256 {
			return value
		}
	}
	return ""
}

// selectNamedTextAccount performs one bounded, synchronous choice. It does no
// provider I/O and returns a current check tied to the exact member fingerprint
// selected by the pool.
func (s *Service) selectNamedTextAccount(c config.Config, client config.NamedAPIClient, model, sessionKey string, clientCurrent func() bool) (config.LocalAccount, func() bool, func(), int) {
	return s.selectNamedTextAccountExcept(c, client, model, sessionKey, clientCurrent, nil)
}

func (s *Service) selectNamedTextAccountExcept(c config.Config, client config.NamedAPIClient, model, sessionKey string, clientCurrent func() bool, excluded []string) (config.LocalAccount, func() bool, func(), int) {
	if !clientCurrent() {
		return config.LocalAccount{}, nil, nil, 409
	}
	if s.Pool == nil {
		return config.LocalAccount{}, nil, nil, 503
	}
	candidates, status := namedTextPoolCandidates(c, client)
	if status != 0 {
		return config.LocalAccount{}, nil, nil, status
	}
	// Configuration identity must not depend on transient health/ranking.
	policy := namedTextPoolPolicy(client, candidates)
	remaining := candidates[:0]
	for _, account := range candidates {
		identity, valid := gateway.CredentialBudgetIdentity(account)
		skip := !valid
		for _, previous := range excluded {
			skip = skip || previous == identity
		}
		if !skip {
			remaining = append(remaining, account)
		}
	}
	candidates = remaining
	if len(candidates) == 0 {
		return config.LocalAccount{}, nil, nil, 503
	}
	// Health is an eligibility observation, separate from the stable policy.
	candidates = s.HealthyAccounts(candidates)
	candidates = s.ScoreAccounts(candidates)
	if len(candidates) == 0 {
		return config.LocalAccount{}, nil, nil, 503
	}
	allowed := 0
	for _, account := range candidates {
		if config.ModelAllowed(account, model) {
			allowed++
		}
	}
	if allowed == 0 {
		return config.LocalAccount{}, nil, nil, 403
	}
	selected, release, err := s.Pool.SelectWithPolicy("codex", candidates, namedTextPoolGroup(client), model, sessionKey, policy)
	if err != nil {
		// Selection failures intentionally expose only a stable status. The pool
		// error can contain quota/cooldown details unsuitable for a client body.
		// Use errors.Is for structured gateway errors; avoid fragile Chinese string matching.
		if errors.Is(err, gateway.ErrCredentialCapacity) {
			return config.LocalAccount{}, nil, nil, 429
		}
		errStr := err.Error()
		if strings.Contains(errStr, "concurrency") || strings.Contains(errStr, "quota") || strings.Contains(errStr, "cooldown") ||
			strings.Contains(errStr, "并发") || strings.Contains(errStr, "额度") || strings.Contains(errStr, "冷却") {
			return config.LocalAccount{}, nil, nil, 429
		}
		return config.LocalAccount{}, nil, nil, 503
	}
	selectedFingerprint := config.AccountFingerprint(selected)
	selectedID := selected.ID
	current := func() bool {
		if !clientCurrent() || s.Config == nil {
			return false
		}
		latest := s.Config()
		latestClient, ok := latest.APIClient(client.ID)
		if !ok || !namedTextPoolEnabled(latestClient) {
			return false
		}
		member := false
		for _, id := range latestClient.AccountIDs {
			if id == selectedID {
				member = true
				break
			}
		}
		if !member {
			return false
		}
		account, ok := latest.LocalAccount(selectedID)
		if !ok {
			return false
		}
		account = codexDispatchAccount(account)
		account.PoolEnabled = true
		account.PoolGroup = namedTextPoolGroup(latestClient)
		return config.AccountFingerprint(account) == selectedFingerprint
	}
	if !current() {
		release()
		return config.LocalAccount{}, nil, nil, 409
	}
	return selected, current, release, 0
}
