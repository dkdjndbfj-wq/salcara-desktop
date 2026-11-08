package gateway

import (
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/big"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"salcara/bridge/internal/config"
)

// AccountPool is the local, Cockpit-style account selector used by the
// protocol gateway. It never exposes a credential to the Hub. The caller
// supplies a complete fresh account snapshot for every selection so deleting or
// editing an account takes effect without restarting the desktop app.
//
// The pool is intentionally separate from authorization: a candidate may be
// healthy and schedulable here, but the Hub still decides whether its source
// is allowed for remote execution.
type AccountPool struct {
	mu sync.Mutex

	now          func() time.Time
	stickyTTL    time.Duration
	maxSticky    int
	nextSweep    time.Time
	sticky       map[string]stickyAccount
	state        map[string]*accountState
	budgets      map[string]*credentialBudget
	bindings     map[poolBindingKey]*credentialBinding
	limits       map[string]int
	retention    time.Duration
	maxStates    int
	maxBudgets   int
	maxBindings  int
	order        uint64
	routingProbe PoolRoutingProbe
	randomIndex  func(int) int
}

// PoolQuotaObservation contains only current, sanitized routing data. The
// probe must be memory-only: Select calls it while holding the pool mutex.
type PoolQuotaObservation struct {
	Fresh            bool
	Allowed          bool
	RemainingPercent float64
}

type PoolQuotaProbe func(credentialRef string, identityEpoch uint64, model string) PoolQuotaObservation

// PoolRoutingObservation contains only bounded data already cached for the
// current protected credential. Plan/quota freshness is provider-verified;
// subscription expiry remains an explicitly unverified ordering hint.
type PoolRoutingObservation struct {
	Current                 bool
	QuotaFresh              bool
	QuotaAllowed            bool
	RemainingPercent        float64
	PlanFresh               bool
	PlanFamily              string
	SubscriptionKnown       bool
	SubscriptionActiveUntil *time.Time
}

type PoolRoutingProbe func(credentialRef string, identityEpoch uint64, model string) PoolRoutingObservation

// PoolPolicy is supplied by the explicitly authorized/applied source account.
// SingleAccountID is never inferred from candidate order. Revision is a
// non-secret policy fingerprint used only to retire stale session affinity.
type PoolPolicy struct {
	Strategy        string
	SingleAccountID string
	Revision        string
}

type stickyAccount struct {
	accountID   string
	fingerprint string
	policy      string
	expires     time.Time
}

type poolCandidate struct {
	account     config.LocalAccount
	observation PoolRoutingObservation
}

type accountState struct {
	fingerprint string
	credential  string
	inflight    int
	lastUsed    time.Time
	lastSeen    time.Time
	order       uint64
}

// These keys are private, process-local budget identities, not source policy
// handles. No digest, vault reference, or credential is returned in diagnostics.
type credentialBudget struct {
	inflight     int
	memberLimit  int
	activeLimits map[int]int
	failures     int
	cooldownTil  time.Time
	lastSeen     time.Time
}

type poolBindingKey struct {
	accountID, fingerprint string
}

// Keep the exact selected budget until the request releases it. A later card
// edit must not redirect health updates or releases to the replacement Key.
type credentialBinding struct {
	budget   *credentialBudget
	inflight int
	lastSeen time.Time
	lastUsed time.Time // presentation evidence; set only by request admission
}

var (
	ErrCredentialCapacity    = errors.New("凭据并发已满")
	ErrCredentialCoolingDown = errors.New("凭据正在冷却")
	errNoPoolAccount         = errors.New("没有可用的账号池账号")
	errPoolCapacity          = errors.New("账号池并发已满")
	errPoolGroupRequired     = errors.New("存在多个账号池，请先指定账号池")
	errPoolQuotaReserve      = errors.New("账号池额度低于保留阈值或额度状态未知")
)

// AdmitFixed reserves the same credential budget for an already authorized
// fixed route. It never selects a substitute or establishes source authority.
// The caller must supply the complete current account snapshot and separately
// fence its source, identity and client leases immediately before forwarding.
func (p *AccountPool) AdmitFixed(account config.LocalAccount, allAccounts []config.LocalAccount) (func(), error) {
	return p.admitFixedModels(account, allAccounts, nil)
}

// AdmitFixedModel reserves an already authorized fixed account after checking
// every actual model used by the request. It does not select a replacement.
// Positive quota reserves require a current per-model memory observation;
// accounts that merely need a safe credential refresh remain eligible when no
// reserve was configured.
func (p *AccountPool) AdmitFixedModel(account config.LocalAccount, allAccounts []config.LocalAccount, models ...string) (func(), error) {
	return p.admitFixedModels(account, allAccounts, models)
}

// ValidateFixedModels rechecks health, policy and current quota for an account
// whose credential capacity is already owned by the caller (for example, an
// authenticated WebSocket lease). It never acquires another slot.
func (p *AccountPool) ValidateFixedModels(account config.LocalAccount, models ...string) error {
	now := p.nowTime()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked(now)
	if err := p.validateFixedModelsLocked(account, models); err != nil {
		return err
	}
	state := p.ensureAccountStateLocked(account, now)
	if state == nil {
		return ErrCredentialCapacity
	}
	if now.Before(p.budgets[state.credential].cooldownTil) {
		return ErrCredentialCoolingDown
	}
	return nil
}

func (p *AccountPool) validateFixedModelsLocked(account config.LocalAccount, models []string) error {
	if account.ID == "" {
		return errNoPoolAccount
	}
	if _, valid := poolCredentialIdentity(account); !valid {
		return errNoPoolAccount
	}
	seenModels := make(map[string]struct{}, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if !config.ValidCatalogModel(model) || !config.ModelAllowed(account, model) {
			return errNoPoolAccount
		}
		if _, duplicate := seenModels[model]; duplicate {
			continue
		}
		seenModels[model] = struct{}{}
		if !quotaEligibleObservation(account, p.routingObservationLocked(account, model, false)) {
			return errPoolQuotaReserve
		}
	}
	return nil
}

func (p *AccountPool) admitFixedModels(account config.LocalAccount, allAccounts []config.LocalAccount, models []string) (func(), error) {
	now := p.nowTime()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked(now)
	if err := p.validateFixedModelsLocked(account, models); err != nil {
		return nil, err
	}
	// Include a trusted virtual NamedAPI record even when it has no API-library
	// card. This adds a budget ceiling, not a new source authorization.
	snapshot := make([]config.LocalAccount, 0, len(allAccounts)+1)
	snapshot = append(snapshot, allAccounts...)
	snapshot = append(snapshot, account)
	p.syncBudgetLimitsLocked(snapshot)
	state := p.ensureAccountStateLocked(account, now)
	if state == nil {
		return nil, ErrCredentialCapacity
	}
	budget := p.budgets[state.credential]
	if now.Before(budget.cooldownTil) {
		return nil, ErrCredentialCoolingDown
	}
	if !p.canAcquireLocked(account, now) {
		return nil, ErrCredentialCapacity
	}
	return p.acquireLocked(account, "", now), nil
}

// NewAccountPool creates a bounded selector. The defaults are deliberately
// conservative: one-hour session affinity and at most 4096 sticky sessions.
func NewAccountPool() *AccountPool {
	return &AccountPool{
		now:         time.Now,
		stickyTTL:   time.Hour,
		maxSticky:   4096,
		sticky:      make(map[string]stickyAccount),
		state:       make(map[string]*accountState),
		budgets:     make(map[string]*credentialBudget),
		bindings:    make(map[poolBindingKey]*credentialBinding),
		limits:      make(map[string]int),
		retention:   time.Hour,
		maxStates:   4096,
		maxBudgets:  4096,
		maxBindings: 8192,
		randomIndex: secureRandomIndex,
	}
}

// SetQuotaProbe installs the memory-only usage observation source. A positive
// reserve fails closed until a fresh observation for the current identity is
// available.
func (p *AccountPool) SetQuotaProbe(probe PoolQuotaProbe) {
	if probe == nil {
		p.SetRoutingProbe(nil)
		return
	}
	p.SetRoutingProbe(func(credentialRef string, identityEpoch uint64, model string) PoolRoutingObservation {
		quota := probe(credentialRef, identityEpoch, model)
		return PoolRoutingObservation{Current: quota.Fresh, QuotaFresh: quota.Fresh, QuotaAllowed: quota.Allowed, RemainingPercent: quota.RemainingPercent}
	})
}

// SetRoutingProbe installs the memory-only current-identity observation source.
// Select never performs provider I/O while holding the routing mutex.
func (p *AccountPool) SetRoutingProbe(probe PoolRoutingProbe) {
	p.mu.Lock()
	p.routingProbe = probe
	p.mu.Unlock()
}

// Representative is a credential snapshot for a read-only model-catalog
// request. Unlike Select it neither changes affinity/health nor occupies a
// task slot. Cooling providers retain their cached catalogs.
func (p *AccountPool) Representative(target string, accounts []config.LocalAccount, group string) (config.LocalAccount, error) {
	return p.RepresentativeWithPolicy(target, accounts, group, PoolPolicy{})
}

func (p *AccountPool) RepresentativeWithPolicy(target string, accounts []config.LocalAccount, group string, policy PoolPolicy) (config.LocalAccount, error) {
	resolved, err := resolvePoolGroup(accounts, group)
	if err != nil {
		return config.LocalAccount{}, err
	}
	if !config.ValidPoolStrategy(strings.TrimSpace(policy.Strategy)) {
		return config.LocalAccount{}, errNoPoolAccount
	}
	for _, account := range accounts {
		if account.ID != "" && account.PoolEnabled && strings.TrimSpace(account.PoolGroup) != "" && (resolved == "" || account.PoolGroup == resolved) && poolTargetMatches(account, target) && (policy.Strategy != config.PoolStrategySingleAccount || account.ID == policy.SingleAccountID) {
			return account, nil
		}
	}
	return config.LocalAccount{}, errNoPoolAccount
}

// Select chooses one enabled account that can serve model. Accounts with a
// higher priority win first; among equal priorities, the least loaded account
// (normalized by weight) wins, with least-recent admission breaking load ties.
// Never-used accounts get the first turn without relying on wall-clock time. A non-empty
// session key pins later turns to the same account while it remains healthy.
// The returned release function must be called exactly once.
func (p *AccountPool) Select(target string, accounts []config.LocalAccount, group, model, sessionKey string) (config.LocalAccount, func(), error) {
	return p.SelectWithPolicy(target, accounts, group, model, sessionKey, PoolPolicy{})
}

// SelectWithPolicy applies an explicit applied-source routing policy after the
// normal membership, model, quota-reserve, cooldown and capacity gates.
func (p *AccountPool) SelectWithPolicy(target string, accounts []config.LocalAccount, group, model, sessionKey string, policy PoolPolicy) (config.LocalAccount, func(), error) {
	now := p.nowTime()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked(now)
	policy.Strategy = strings.TrimSpace(policy.Strategy)
	if !config.ValidPoolStrategy(policy.Strategy) {
		return config.LocalAccount{}, nil, errNoPoolAccount
	}
	if policy.Strategy == config.PoolStrategySingleAccount && !config.ValidAccountID(policy.SingleAccountID) {
		return config.LocalAccount{}, nil, errNoPoolAccount
	}
	policyRevision := policy.Revision
	if policyRevision == "" {
		policyRevision = policy.Strategy + "\x00" + policy.SingleAccountID
	}
	// The caller supplies the complete fresh card snapshot. Calculate shared
	// limits before filtering Agent, group, model, priority, or quota, so a
	// duplicate card in another namespace cannot multiply the same Key's limit.
	p.syncBudgetLimitsLocked(accounts)
	group, err := resolvePoolGroup(accounts, group)
	if err != nil {
		return config.LocalAccount{}, nil, err
	}
	// Provider conversation ids are not globally unique across Agent surfaces
	// and pools. Namespace affinity so a shared id never joins two namespaces.
	if sessionKey != "" {
		sessionKey = target + "\x00" + group + "\x00" + sessionKey
	}

	candidates := make([]poolCandidate, 0, len(accounts))
	byID := make(map[string]poolCandidate, len(accounts))
	quotaBlocked := 0
	stateBlocked := 0
	for _, account := range accounts {
		if !account.PoolEnabled || strings.TrimSpace(account.PoolGroup) == "" || account.ID == "" {
			continue
		}
		if group != "" && account.PoolGroup != group {
			continue
		}
		if !poolTargetMatches(account, target) || !poolModelMatches(account, model) {
			continue
		}
		if policy.Strategy == config.PoolStrategySingleAccount && account.ID != policy.SingleAccountID {
			continue
		}
		if _, valid := poolCredentialIdentity(account); !valid {
			continue
		}
		observation := p.routingObservationLocked(account, model, poolStrategyUsesMetrics(policy.Strategy))
		if !quotaEligibleObservation(account, observation) {
			quotaBlocked++
			continue
		}
		if _, duplicate := byID[account.ID]; duplicate {
			continue
		}
		if p.ensureAccountStateLocked(account, now) == nil {
			stateBlocked++
			continue
		}
		candidate := poolCandidate{account: account, observation: observation}
		byID[account.ID] = candidate
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		if stateBlocked > 0 {
			return config.LocalAccount{}, nil, errPoolCapacity
		}
		if quotaBlocked > 0 {
			return config.LocalAccount{}, nil, errPoolQuotaReserve
		}
		return config.LocalAccount{}, nil, errNoPoolAccount
	}

	// A sticky binding is only honored when its account is still present, still
	// serves the model, and is not cooling or at its concurrency ceiling.
	if sessionKey != "" {
		if sticky, ok := p.sticky[sessionKey]; ok && now.Before(sticky.expires) {
			candidate, present := byID[sticky.accountID]
			account := candidate.account
			if present && sticky.policy == policyRevision && sticky.fingerprint == poolStickyFingerprint(account) && (policy.Strategy != config.PoolStrategySingleAccount || account.ID == policy.SingleAccountID) && p.canAcquireLocked(account, now) {
				return account, p.acquireLocked(account, sessionKey, now), nil
			}
			delete(p.sticky, sessionKey)
		}
	}

	eligible := candidates[:0]
	for _, candidate := range candidates {
		if p.canAcquireLocked(candidate.account, now) {
			eligible = append(eligible, candidate)
		}
	}
	if len(eligible) == 0 {
		// Distinguish temporary capacity exhaustion from a provider cooldown. The
		// caller can return a retryable response without choosing a different key.
		for _, candidate := range candidates {
			if p.canAcquireIgnoringCapacityLocked(candidate.account, now) {
				return config.LocalAccount{}, nil, errPoolCapacity
			}
		}
		return config.LocalAccount{}, nil, errNoPoolAccount
	}

	if policy.Strategy == config.PoolStrategyCustom {
		primary := eligible[:0]
		for _, candidate := range eligible {
			if !candidate.account.PoolBackup {
				primary = append(primary, candidate)
			}
		}
		if len(primary) > 0 {
			eligible = primary
		}
	}
	selected := p.selectCandidateLocked(eligible, policy.Strategy)
	if sessionKey != "" {
		p.addStickyLocked(sessionKey, selected.account, policyRevision, now)
	}
	return selected.account, p.acquireLocked(selected.account, sessionKey, now), nil
}

func (p *AccountPool) routingObservationLocked(account config.LocalAccount, model string, includeMetrics bool) PoolRoutingObservation {
	if p.routingProbe == nil || account.CredentialRef == "" || account.CredentialEpoch == 0 || account.QuotaReservePercent <= 0 && !includeMetrics {
		return PoolRoutingObservation{}
	}
	model = strings.TrimSpace(model)
	if strings.HasPrefix(model, "claude-salcara-v1-") {
		resolved, ok := config.ResolveClaudeDesktopModel(account, model)
		if !ok {
			return PoolRoutingObservation{}
		}
		model = resolved
	}
	return p.routingProbe(account.CredentialRef, account.CredentialEpoch, model)
}

func poolStrategyUsesMetrics(strategy string) bool {
	switch strategy {
	case config.PoolStrategyAuto, config.PoolStrategyQuotaHighFirst, config.PoolStrategyQuotaLowFirst,
		config.PoolStrategyPlanHighFirst, config.PoolStrategyPlanLowFirst, config.PoolStrategyExpirySoonFirst:
		return true
	default:
		return false
	}
}

func quotaEligibleObservation(account config.LocalAccount, observation PoolRoutingObservation) bool {
	if account.QuotaReservePercent <= 0 {
		return true
	}
	return observation.Current && observation.QuotaFresh && observation.QuotaAllowed && observation.RemainingPercent > float64(account.QuotaReservePercent)
}

func (p *AccountPool) selectCandidateLocked(candidates []poolCandidate, strategy string) poolCandidate {
	if strategy == config.PoolStrategyRandom {
		index := 0
		if p.randomIndex != nil {
			index = p.randomIndex(len(candidates))
		}
		if index < 0 || index >= len(candidates) {
			index = 0
		}
		return candidates[index]
	}
	selected := candidates[0]
	for _, candidate := range candidates[1:] {
		if p.candidateBetterLocked(candidate, selected, strategy) {
			selected = candidate
		}
	}
	return selected
}

func (p *AccountPool) candidateBetterLocked(left, right poolCandidate, strategy string) bool {
	compareKnownFloat := func(leftKnown bool, leftValue float64, rightKnown bool, rightValue float64, highFirst bool) int {
		if leftKnown != rightKnown {
			if leftKnown {
				return 1
			}
			return -1
		}
		if !leftKnown || leftValue == rightValue {
			return 0
		}
		if highFirst == (leftValue > rightValue) {
			return 1
		}
		return -1
	}
	comparePlan := func(highFirst bool) int {
		leftRank, leftKnown := poolPlanRank(left.observation)
		rightRank, rightKnown := poolPlanRank(right.observation)
		return compareKnownFloat(leftKnown, float64(leftRank), rightKnown, float64(rightRank), highFirst)
	}
	compareQuota := func(highFirst bool) int {
		leftKnown := left.observation.Current && left.observation.QuotaFresh
		rightKnown := right.observation.Current && right.observation.QuotaFresh
		return compareKnownFloat(leftKnown, left.observation.RemainingPercent, rightKnown, right.observation.RemainingPercent, highFirst)
	}
	compareExpiry := func() int {
		leftKnown := left.observation.Current && left.observation.SubscriptionKnown && left.observation.SubscriptionActiveUntil != nil
		rightKnown := right.observation.Current && right.observation.SubscriptionKnown && right.observation.SubscriptionActiveUntil != nil
		if leftKnown != rightKnown {
			if leftKnown {
				return 1
			}
			return -1
		}
		if !leftKnown || left.observation.SubscriptionActiveUntil.Equal(*right.observation.SubscriptionActiveUntil) {
			return 0
		}
		if left.observation.SubscriptionActiveUntil.Before(*right.observation.SubscriptionActiveUntil) {
			return 1
		}
		return -1
	}
	var comparisons []int
	switch strategy {
	case config.PoolStrategyAuto, config.PoolStrategyPlanHighFirst:
		comparisons = []int{comparePlan(true), compareQuota(true)}
	case config.PoolStrategyPlanLowFirst:
		comparisons = []int{comparePlan(false), compareQuota(true)}
	case config.PoolStrategyQuotaHighFirst:
		comparisons = []int{compareQuota(true), comparePlan(true)}
	case config.PoolStrategyQuotaLowFirst:
		comparisons = []int{compareQuota(false), comparePlan(true)}
	case config.PoolStrategyExpirySoonFirst:
		comparisons = []int{compareExpiry(), comparePlan(true), compareQuota(true)}
	case config.PoolStrategySingleAccount:
		return false
	case config.PoolStrategyCustom, "":
		if left.account.Priority != right.account.Priority {
			return left.account.Priority > right.account.Priority
		}
	}
	for _, comparison := range comparisons {
		if comparison != 0 {
			return comparison > 0
		}
	}
	return p.lessLoadedLocked(left.account, right.account)
}

func (p *AccountPool) lessLoadedLocked(left, right config.LocalAccount) bool {
	leftState, rightState := p.state[left.ID], p.state[right.ID]
	leftLoad := int64(leftState.inflight) * int64(normalizedWeight(right))
	rightLoad := int64(rightState.inflight) * int64(normalizedWeight(left))
	return leftLoad < rightLoad || leftLoad == rightLoad && leftState.order < rightState.order
}

func poolPlanRank(observation PoolRoutingObservation) (int, bool) {
	if !observation.Current || !observation.PlanFresh {
		return 0, false
	}
	switch observation.PlanFamily {
	case "enterprise", "edu", "health", "gov", "teachers":
		return 700, true
	case "pro":
		return 500, true
	case "business", "team", "plus":
		return 300, true
	case "go":
		return 200, true
	case "free":
		return 100, true
	default:
		return 0, false
	}
}

func secureRandomIndex(length int) int {
	if length <= 1 {
		return 0
	}
	value, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(length)))
	if err != nil {
		return 0
	}
	return int(value.Int64())
}

func poolStickyFingerprint(account config.LocalAccount) string {
	raw := strings.Join([]string{
		config.AccountFingerprint(account), strconv.Itoa(account.Priority), strconv.Itoa(account.Weight),
		strconv.Itoa(account.MaxConcurrency), strconv.Itoa(account.QuotaReservePercent),
	}, "\x00")
	digest := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(digest[:])
}

// Current reports whether an account snapshot is still present and unchanged
// in the supplied candidates. It is used immediately before forwarding so a
// concurrent key edit cannot send using an old credential.
func (p *AccountPool) Current(target string, accounts []config.LocalAccount, selected config.LocalAccount) bool {
	for _, account := range accounts {
		if account.ID == selected.ID && poolTargetMatches(account, target) && config.AccountFingerprint(account) == config.AccountFingerprint(selected) {
			return true
		}
	}
	return false
}

// MarkSuccess clears an expired transient cooldown and failure count. It does not
// release a request slot; release belongs to the selection's lifetime.
func (p *AccountPool) MarkSuccess(accountID string) {
	p.MarkSuccessFor(accountID, "")
}

// MarkSuccessFor is the race-safe form used by a completed request. When a
// fingerprint is supplied, a late response from an old Key cannot clear or
// mutate the health state of a replacement that reused the same account ID.
func (p *AccountPool) MarkSuccessFor(accountID, fingerprint string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if budget := p.healthBudgetLocked(accountID, fingerprint); budget != nil {
		now := p.nowTime()
		budget.lastSeen = now
		// A successful stream admitted before another request's 429 is not
		// evidence that Retry-After has elapsed. Never clear an active cooldown.
		if !now.Before(budget.cooldownTil) {
			budget.failures = 0
			budget.cooldownTil = time.Time{}
		}
	}
}

// MarkFailure applies bounded health backoff. Retry-After supplied by an
// upstream is capped so a malformed provider cannot poison the pool forever.
func (p *AccountPool) MarkFailure(accountID string, retryAfter time.Duration) {
	p.MarkFailureFor(accountID, "", retryAfter)
}

// MarkFailureFor is the race-safe form used by a completed request. See
// MarkSuccessFor for why the fingerprint is part of the health update.
func (p *AccountPool) MarkFailureFor(accountID, fingerprint string, retryAfter time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	budget := p.healthBudgetLocked(accountID, fingerprint)
	if budget == nil {
		return
	}
	budget.failures++
	if retryAfter <= 0 {
		retryAfter = 2 * time.Second
		for i := 1; i < budget.failures && retryAfter < time.Minute; i++ {
			retryAfter *= 2
		}
	}
	if retryAfter > 24*time.Hour {
		retryAfter = 24 * time.Hour
	}
	now := p.nowTime()
	budget.lastSeen = now
	until := now.Add(retryAfter)
	if until.After(budget.cooldownTil) {
		budget.cooldownTil = until
	}
}

// ResetHealth clears the local failure/cooldown state for the current
// credential behind an account card. It never releases in-flight leases or
// changes membership; a caller must explicitly refresh the account snapshot
// before the next request. This is a user-directed recovery action, not an
// upstream quota reset.
func (p *AccountPool) ResetHealth(accountID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.state[accountID]
	if state == nil || state.credential == "" {
		return false
	}
	budget := p.budgets[state.credential]
	if budget == nil {
		return false
	}
	budget.failures = 0
	budget.cooldownTil = time.Time{}
	budget.lastSeen = p.nowTime()
	return true
}

func (p *AccountPool) healthBudgetLocked(accountID, fingerprint string) *credentialBudget {
	if fingerprint != "" {
		if binding := p.bindings[poolBindingKey{accountID, fingerprint}]; binding != nil {
			binding.lastSeen = p.nowTime()
			return binding.budget
		}
		return nil
	}
	if state := p.state[accountID]; state != nil {
		return p.budgets[state.credential]
	}
	return nil
}

// Snapshot returns non-sensitive runtime state for diagnostics. It never
// includes the account key.
type PoolAccountState struct {
	AccountID     string    `json:"accountId"`
	Inflight      int       `json:"inflight"`
	Failures      int       `json:"failures"`
	CoolingDown   bool      `json:"coolingDown"`
	CooldownUntil time.Time `json:"cooldownUntil,omitempty"`
	LastUsedAt    time.Time `json:"lastUsedAt,omitempty"`
}

func (p *AccountPool) Snapshot(accounts []config.LocalAccount) []PoolAccountState {
	now := p.nowTime()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked(now)
	out := make([]PoolAccountState, 0, len(accounts))
	for _, account := range accounts {
		identity, valid := poolCredentialIdentity(account)
		if !valid {
			continue
		}
		budget := p.budgets[identity]
		binding := p.bindings[poolBindingKey{account.ID, config.AccountFingerprint(account)}]
		// Card-wide state preserves concurrency limits while old streams drain.
		// Display activity must instead belong to the exact current identity;
		// a probe or a key replacement cannot inherit an old request timestamp.
		if binding != nil && binding.budget != budget {
			binding = nil
		}
		if binding == nil && budget == nil {
			continue
		}
		row := PoolAccountState{AccountID: account.ID}
		if binding != nil {
			row.Inflight, row.LastUsedAt = binding.inflight, binding.lastUsed
		}
		if budget != nil {
			row.Failures, row.CoolingDown, row.CooldownUntil = budget.failures, now.Before(budget.cooldownTil), budget.cooldownTil
		}
		out = append(out, row)
	}
	return out
}

// Catalog returns the union of the cached model catalogs for eligible pool
// members. It is read-only: unlike Select it does not consume a concurrency
// slot or discard catalogs just because a provider is temporarily cooling
// down. This keeps a model picker useful during a transient 429/network
// backoff and lets a later request select the member that actually supports
// the chosen model.
func (p *AccountPool) Catalog(target string, accounts []config.LocalAccount, group string) []string {
	return p.CatalogWithPolicy(target, accounts, group, PoolPolicy{})
}

func (p *AccountPool) CatalogWithPolicy(target string, accounts []config.LocalAccount, group string, policy PoolPolicy) []string {
	policy.Strategy = strings.TrimSpace(policy.Strategy)
	if !config.ValidPoolStrategy(policy.Strategy) {
		return nil
	}
	if resolved, err := resolvePoolGroup(accounts, group); err != nil {
		return nil
	} else {
		group = resolved
	}
	seen := make(map[string]struct{})
	models := make([]string, 0)
	for _, account := range accounts {
		if !account.PoolEnabled || strings.TrimSpace(account.PoolGroup) == "" || account.ID == "" {
			continue
		}
		if group != "" && account.PoolGroup != group {
			continue
		}
		if policy.Strategy == config.PoolStrategySingleAccount && account.ID != policy.SingleAccountID {
			continue
		}
		if !poolTargetMatches(account, target) {
			continue
		}
		for _, model := range append(append([]string{}, account.Models...), account.Model) {
			model = strings.TrimSpace(model)
			if model == "" || !config.ValidCatalogModel(model) {
				continue
			}
			if !config.ModelAllowed(account, model) {
				continue
			}
			if _, ok := seen[model]; ok {
				continue
			}
			seen[model] = struct{}{}
			models = append(models, model)
		}
	}
	sort.Strings(models)
	return models
}

// resolvePoolGroup prevents a missing tool selection from silently combining
// unrelated providers. A single configured group remains convenient and is
// selected implicitly; multiple groups require the tool/remote selector to
// name one explicitly.
func resolvePoolGroup(accounts []config.LocalAccount, group string) (string, error) {
	group = strings.TrimSpace(group)
	if group != "" {
		return group, nil
	}
	var only string
	for _, account := range accounts {
		if !account.PoolEnabled || strings.TrimSpace(account.PoolGroup) == "" {
			continue
		}
		candidate := strings.TrimSpace(account.PoolGroup)
		if only == "" {
			only = candidate
			continue
		}
		if only != candidate {
			return "", errPoolGroupRequired
		}
	}
	return only, nil
}

func (p *AccountPool) nowTime() time.Time {
	if p.now == nil {
		return time.Now()
	}
	return p.now()
}

func poolCredentialIdentity(account config.LocalAccount) (string, bool) {
	var raw string
	if account.SourceType == "official_account" || account.SourceType == "oauth" {
		// The validated card supplies a vault reference and identity epoch. Never
		// use its placeholder Key or a changing access token as the budget identity.
		if account.CredentialRef == "" || account.CredentialEpoch == 0 || strings.ContainsAny(account.CredentialRef, "\r\n\x00") {
			return "", false
		}
		raw = "protected-v1\x00" + account.CredentialRef + "\x00" + strconv.FormatUint(account.CredentialEpoch, 10)
	} else {
		if account.CredentialRef != "" || account.Key == "" || strings.ContainsAny(account.Key, "\r\n\x00") {
			return "", false
		}
		base, _, err := config.APIBase(account.BaseURL)
		if err != nil {
			return "", false
		}
		u, err := url.Parse(base)
		if err != nil {
			return "", false
		}
		host, port := strings.ToLower(u.Hostname()), u.Port()
		if u.Scheme == "https" && port == "443" || u.Scheme == "http" && port == "80" {
			port = ""
		}
		if port != "" {
			host = net.JoinHostPort(host, port)
		} else if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		u.Host = host
		// Distinct schemes and API mount paths remain distinct; we cannot infer
		// that two server aliases or tenants share an upstream quota.
		raw = "api-v1\x00" + u.String() + "\x00" + account.Key
	}
	digest := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(digest[:]), true
}

func (p *AccountPool) syncBudgetLimitsLocked(accounts []config.LocalAccount) {
	limits := make(map[string]int, len(accounts))
	for _, account := range accounts {
		if account.ID == "" {
			continue
		}
		identity, valid := poolCredentialIdentity(account)
		if !valid {
			continue
		}
		limit := limits[identity]
		if account.MaxConcurrency > 0 && (limit == 0 || account.MaxConcurrency < limit) {
			limit = account.MaxConcurrency
		}
		limits[identity] = limit
	}
	p.limits = limits
	for identity, budget := range p.budgets {
		budget.memberLimit = limits[identity]
	}
}

func (p *AccountPool) ensureAccountStateLocked(account config.LocalAccount, now time.Time) *accountState {
	identity, valid := poolCredentialIdentity(account)
	if !valid {
		return nil
	}
	fingerprint := config.AccountFingerprint(account)
	key := poolBindingKey{account.ID, fingerprint}
	state, budget, binding := p.state[account.ID], p.budgets[identity], p.bindings[key]
	// Never evict an active lease or an unexpired provider cooldown to admit a
	// newly copied card. Bounded history exhaustion is retryable and fail-closed.
	if state == nil && len(p.state) >= p.maxStates || budget == nil && len(p.budgets) >= p.maxBudgets || binding == nil && len(p.bindings) >= p.maxBindings {
		return nil
	}
	if budget == nil {
		budget = &credentialBudget{memberLimit: p.limits[identity], activeLimits: make(map[int]int)}
		p.budgets[identity] = budget
	}
	if binding == nil {
		binding = &credentialBinding{budget: budget}
		p.bindings[key] = binding
	}
	if state == nil {
		state = &accountState{}
		p.state[account.ID] = state
	}
	state.fingerprint, state.credential, state.lastSeen = fingerprint, identity, now
	budget.lastSeen, binding.lastSeen = now, now
	return state
}

func (b *credentialBudget) concurrencyLimit() int {
	limit := b.memberLimit
	for active, count := range b.activeLimits {
		if count > 0 && active > 0 && (limit == 0 || active < limit) {
			limit = active
		}
	}
	return limit
}

func (p *AccountPool) canAcquireLocked(account config.LocalAccount, now time.Time) bool {
	state := p.ensureAccountStateLocked(account, now)
	if state == nil {
		return false
	}
	budget := p.budgets[state.credential]
	limit := budget.concurrencyLimit()
	return !now.Before(budget.cooldownTil) && (account.MaxConcurrency <= 0 || state.inflight < account.MaxConcurrency) && (limit <= 0 || budget.inflight < limit)
}

func (p *AccountPool) canAcquireIgnoringCapacityLocked(account config.LocalAccount, now time.Time) bool {
	state := p.ensureAccountStateLocked(account, now)
	return state != nil && !now.Before(p.budgets[state.credential].cooldownTil)
}

func (p *AccountPool) acquireLocked(account config.LocalAccount, sessionKey string, now time.Time) func() {
	state := p.ensureAccountStateLocked(account, now)
	budget := p.budgets[state.credential]
	binding := p.bindings[poolBindingKey{account.ID, state.fingerprint}]
	// Retain the effective shared cap under which this lease was admitted, not
	// just the chosen card's cap: a lower-limit alias may not be the chosen card.
	activeLimit := budget.concurrencyLimit()
	p.order++
	state.order = p.order
	state.inflight++
	state.lastUsed = now
	budget.inflight++
	budget.activeLimits[activeLimit]++
	binding.inflight++
	binding.lastUsed = now
	released := false
	return func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if released {
			return
		}
		released = true
		now := p.nowTime()
		// Capture pointers, not the mutable card ID, so editing/deleting/reusing
		// a card while its stream drains cannot free a replacement's budget.
		state.inflight--
		state.lastSeen = now
		budget.inflight--
		budget.lastSeen = now
		if budget.activeLimits[activeLimit] <= 1 {
			delete(budget.activeLimits, activeLimit)
		} else {
			budget.activeLimits[activeLimit]--
		}
		binding.inflight--
		binding.lastSeen = now
	}
}

func (p *AccountPool) addStickyLocked(sessionKey string, account config.LocalAccount, policy string, now time.Time) {
	if sessionKey == "" {
		return
	}
	if len(p.sticky) >= p.maxSticky {
		// Evict the oldest expiry rather than growing without a bound.
		oldestKey := ""
		var oldest time.Time
		for key, binding := range p.sticky {
			if oldestKey == "" || binding.expires.Before(oldest) {
				oldestKey, oldest = key, binding.expires
			}
		}
		if oldestKey != "" {
			delete(p.sticky, oldestKey)
		}
	}
	p.sticky[sessionKey] = stickyAccount{accountID: account.ID, fingerprint: poolStickyFingerprint(account), policy: policy, expires: now.Add(p.stickyTTL)}
}

func (p *AccountPool) sweepLocked(now time.Time) {
	if now.Before(p.nextSweep) {
		return
	}
	p.nextSweep = now.Add(time.Minute)
	for key, binding := range p.sticky {
		if !now.Before(binding.expires) {
			delete(p.sticky, key)
		}
	}
	for key, binding := range p.bindings {
		if binding.inflight == 0 && !now.Before(binding.lastSeen.Add(p.retention)) && !now.Before(binding.budget.cooldownTil) {
			delete(p.bindings, key)
		}
	}
	for key, state := range p.state {
		if state.inflight == 0 && !now.Before(state.lastSeen.Add(p.retention)) {
			delete(p.state, key)
		}
	}
	for key, budget := range p.budgets {
		if budget.inflight == 0 && !now.Before(budget.lastSeen.Add(p.retention)) && !now.Before(budget.cooldownTil) {
			delete(p.budgets, key)
		}
	}
}

func normalizedWeight(account config.LocalAccount) int {
	if account.Weight <= 0 {
		return 1
	}
	return account.Weight
}

func poolTargetMatches(account config.LocalAccount, target string) bool {
	originalTarget := target
	target = strings.TrimPrefix(target, "remote-")
	target = config.ToolFamily(target)
	if account.Target != "" && account.Target != originalTarget && account.Target != target && account.Target != config.ToolFamily(target) {
		return false
	}
	if account.Kind == "api" {
		return true
	}
	return account.Kind == config.ToolFamily(target)
}

func poolModelMatches(account config.LocalAccount, model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return true
	}
	// Claude Desktop exposes opaque compatibility route IDs for non-Claude
	// upstream models. Resolve that route against each member before applying
	// its allow/deny policy; comparing the hash itself would make every pooled
	// GPT/DeepSeek account look ineligible even though its catalog advertised it.
	if strings.HasPrefix(model, "claude-salcara-v1-") {
		resolved, ok := config.ResolveClaudeDesktopModel(account, model)
		if !ok {
			return false
		}
		model = resolved
	}
	if !config.ModelAllowed(account, model) {
		return false
	}
	// The native official picker is a bounded discovery hint, not an
	// entitlement list. The gateway performs the final provider-side model
	// authorization, so a valid explicit model remains eligible even when the
	// cached picker list is incomplete.
	if account.CredentialRef != "" && account.SourceType == "official_account" && config.ValidCatalogModel(model) {
		return true
	}
	if account.Model == model {
		return true
	}
	if len(account.Models) == 0 {
		// An unpopulated catalog means the provider has not been probed yet, not
		// that the account supports every model. The account is eligible for a
		// new request; the gateway's existing model validation remains authoritative.
		return true
	}
	for _, candidate := range account.Models {
		if candidate == model {
			return true
		}
	}
	return false
}
