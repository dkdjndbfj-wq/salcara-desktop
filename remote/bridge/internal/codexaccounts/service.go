package codexaccounts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

type flight struct {
	done       chan struct{}
	cancel     context.CancelFunc
	epoch      uint64
	err        error
	cancelled  bool
	background *usageRefreshJob
}
type pendingRotation struct {
	credential credential
	epoch      uint64
}
type loginState struct {
	session      LoginSession
	cancel       context.CancelFunc
	request      LoginRequest
	epoch        uint64
	nameRevision uint64
	connector    LoginConnector
	done         chan struct{}
}

type Service struct {
	mu                   sync.Mutex
	path                 string
	protector            Protector
	refresher            Refresher
	login                LoginConnector
	deviceLogin          LoginConnector
	usageFetcher         UsageFetcher
	subscriptionFetcher  SubscriptionFetcher
	subscriptionTimeout  time.Duration
	subscriptionFreshFor time.Duration
	subscriptionFlights  map[string]*subscriptionFlight
	subscriptions        map[string]subscriptionEntry
	subscriptionWork     map[string]int
	resetConsumer        ResetCreditConsumer
	resetTimeout         time.Duration
	resetFlights         map[string]*resetCreditFlight
	now                  func() time.Time
	timeout              time.Duration
	usageTimeout         time.Duration
	usageFreshFor        time.Duration
	usageRefresh         *usageRefreshScheduler
	vault                vault
	flights              map[string]*flight
	usageFlights         map[string]*usageFlight
	usage                map[string]usageEntry
	importChecks         map[*importCheckFlight]struct{}
	importCheckSlots     chan struct{}
	pending              map[string]pendingRotation
	logins               map[string]*loginState
	closed               bool
	vaultLock            *vaultLock
	cleanupNonce         string
	cleanupObservations  map[string]uint64
	cleanupUsageWork     map[string]int
}

func New(opts Options) (*Service, error) {
	if strings.TrimSpace(opts.Path) == "" {
		return nil, ErrSaveFailed
	}
	if opts.Protector == nil {
		opts.Protector = platformProtector()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.RefreshTimeout <= 0 {
		opts.RefreshTimeout = 30 * time.Second
	}
	if opts.UsageTimeout <= 0 {
		opts.UsageTimeout = 5 * time.Second
	}
	if opts.UsageFreshFor <= 0 {
		opts.UsageFreshFor = 5 * time.Minute
	}
	if opts.UsageFetcher == nil {
		opts.UsageFetcher = newOfficialUsageFetcher(opts.UsageTimeout, opts.Now)
	}
	if opts.SubscriptionTimeout <= 0 {
		opts.SubscriptionTimeout = 8 * time.Second
	}
	if opts.SubscriptionFreshFor <= 0 {
		opts.SubscriptionFreshFor = 5 * time.Minute
	}
	if opts.SubscriptionFetcher == nil {
		opts.SubscriptionFetcher = newOfficialSubscriptionFetcher(opts.SubscriptionTimeout)
	}
	if opts.ResetCreditTimeout <= 0 {
		opts.ResetCreditTimeout = 30 * time.Second
	}
	if opts.ResetCreditConsumer == nil {
		opts.ResetCreditConsumer = newOfficialResetCreditConsumer(opts.ResetCreditTimeout)
	}
	var held *vaultLock
	var err error
	if opts.ExclusiveWriter {
		held, err = acquireVaultLock(opts.Path)
		if err != nil {
			return nil, err
		}
	}
	v, err := readVault(opts.Path, opts.Protector)
	if err != nil {
		if held != nil {
			_ = held.Close()
		}
		return nil, err
	}
	nonce, err := newID()
	if err != nil {
		if held != nil {
			_ = held.Close()
		}
		return nil, err
	}
	return &Service{cleanupNonce: nonce, path: opts.Path, protector: opts.Protector, refresher: opts.Refresher, login: opts.Login, deviceLogin: opts.DeviceLogin, usageFetcher: opts.UsageFetcher, subscriptionFetcher: opts.SubscriptionFetcher, subscriptionTimeout: opts.SubscriptionTimeout, subscriptionFreshFor: opts.SubscriptionFreshFor, subscriptionFlights: map[string]*subscriptionFlight{}, subscriptions: map[string]subscriptionEntry{}, subscriptionWork: map[string]int{}, resetConsumer: opts.ResetCreditConsumer, resetTimeout: opts.ResetCreditTimeout, resetFlights: map[string]*resetCreditFlight{}, now: opts.Now, timeout: opts.RefreshTimeout, usageTimeout: opts.UsageTimeout, usageFreshFor: opts.UsageFreshFor, vault: v, vaultLock: held, flights: map[string]*flight{}, usageFlights: map[string]*usageFlight{}, usage: map[string]usageEntry{}, importChecks: map[*importCheckFlight]struct{}{}, importCheckSlots: make(chan struct{}, importCheckConcurrency), pending: map[string]pendingRotation{}, logins: map[string]*loginState{}}, nil
}

func (s *Service) List() []Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := make([]Account, 0, len(s.vault.Accounts))
	for _, c := range s.vault.Accounts {
		rows = append(rows, s.accountLocked(c))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows
}
func (s *Service) Get(id string) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.vault.Accounts[id]
	if !ok {
		return Account{}, ErrNotFound
	}
	return s.accountLocked(c), nil
}

// MatchesPreparedRefreshToken answers a local conflict check without exposing
// the vault credential.  The prepared value normally comes from a read-only
// inspection of a user-selected/native auth file.  This does not coordinate
// with that process; it only lets the caller warn before two clients race the
// provider's refresh-token family.
func (s *Service) MatchesPreparedRefreshToken(ctx context.Context, id string, p *PreparedImport) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	c, ok := s.vault.Accounts[id]
	if !ok {
		return false, ErrNotFound
	}
	if c.RefreshToken == "" || p == nil {
		return false, nil
	}
	return p.containsRefreshToken(c.RefreshToken), nil
}

// Import commits the entire bundle atomically. replaceID requires exactly one
// record and fences refresh/login work on that account after successful save.
func (s *Service) Import(ctx context.Context, p *PreparedImport, replaceID string) ([]Account, error) {
	if p == nil || len(p.records) == 0 || len(p.records) > MaxAccounts || replaceID != "" && len(p.records) != 1 {
		return nil, ErrInvalidImport
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if replaceID != "" {
		if _, ok := s.vault.Accounts[replaceID]; !ok {
			return nil, ErrNotFound
		}
	}
	if replaceID == "" && len(s.vault.Accounts)+len(p.records) > MaxAccounts {
		return nil, ErrInvalidImport
	}
	if s.credentialConflictLocked(p.records, replaceID) {
		return nil, ErrConflict
	}
	next := cloneVault(s.vault)
	rows := make([]Account, 0, len(p.records))
	for _, c := range p.records {
		id := replaceID
		if id == "" {
			var err error
			id, err = newID()
			if err != nil {
				return nil, ErrSaveFailed
			}
		}
		c.ID = id
		if replaceID != "" {
			preserveCustomAccountName(&c, s.vault.Accounts[id])
		}
		stampSubscriptionHint(&c, s.now())
		c.Epoch = next.Epochs[id] + 1
		c.IdentityEpoch = c.Epoch
		c.Status = StatusReady
		if c.AccessToken == "" {
			c.Status = StatusExpired
		}
		next.Epochs[id] = c.Epoch
		next.Accounts[id] = c
		delete(next.ResetRedemptions, id)
		rows = append(rows, s.accountLocked(c))
	}
	if err := writeVault(s.path, s.protector, next); err != nil {
		return nil, err
	}
	s.vault = next
	if replaceID != "" {
		s.fenceLocked(replaceID)
	}
	return rows, nil
}

func (s *Service) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := s.vault.Accounts[id]; !ok {
		return ErrNotFound
	}
	next := cloneVault(s.vault)
	delete(next.Accounts, id)
	delete(next.ResetRedemptions, id)
	next.Epochs[id]++
	if err := writeVault(s.path, s.protector, next); err != nil {
		return err
	}
	s.vault = next
	s.fenceLocked(id)
	return nil
}

func (s *Service) fenceLocked(id string) {
	if f := s.flights[id]; f != nil {
		f.cancel()
		delete(s.flights, id)
	}
	delete(s.pending, id)
	s.fenceUsageLocked(id)
	s.fenceSubscriptionLocked(id)
	s.fenceResetCreditLocked(id)
	for _, l := range s.logins {
		if l.session.Status == "pending" && l.request.ReplaceID == id {
			l.cancel()
			l.session.Status = "cancelled"
			l.session.LoginProgress = LoginProgress{}
		}
	}
}

// Refresh coalesces concurrent callers. Cancelling a waiter does not cancel other
// waiters; CancelRefresh, Delete, replacement, and Close cancel the shared work.
func (s *Service) Refresh(ctx context.Context, id string) (Account, error) {
	return s.refreshCredential(ctx, id, nil)
}

func (s *Service) refreshCredential(ctx context.Context, id string, expected *usageAttempt) (Account, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return Account{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return Account{}, err
	}
	c, ok := s.vault.Accounts[id]
	if !ok {
		s.mu.Unlock()
		return Account{}, ErrNotFound
	}
	if c.TokenSourceMode == "chatgpt_web_session" {
		a := s.accountLocked(c)
		s.mu.Unlock()
		return a, ErrConflict
	}
	if !s.backgroundUsageCurrentLocked(ctx, c) {
		s.mu.Unlock()
		return Account{}, ErrConflict
	}
	if expected != nil && (c.Epoch != expected.epoch || c.IdentityEpoch != expected.identityEpoch) {
		a := s.accountLocked(c)
		s.mu.Unlock()
		return a, ErrConflict
	}
	for _, l := range s.logins {
		if l.session.Status == "pending" && l.request.ReplaceID == id {
			a := s.accountLocked(c)
			s.mu.Unlock()
			return a, ErrConflict
		}
	}
	if s.pending[id].epoch != 0 {
		a := s.accountLocked(c)
		s.mu.Unlock()
		return a, ErrSaveFailed
	}
	if c.RefreshToken == "" {
		s.bumpCleanupObservationLocked(id)
		c.Status = StatusNeedsLogin
		s.vault.Accounts[id] = c
		a := s.accountLocked(c)
		s.mu.Unlock()
		return a, ErrNeedsLogin
	}
	if s.refresher == nil {
		s.mu.Unlock()
		return Account{}, ErrRefreshFailed
	}
	f := s.flights[id]
	if f != nil && f.cancelled {
		a := s.accountLocked(c)
		s.mu.Unlock()
		return a, ErrConflict
	}
	if f == nil {
		s.bumpCleanupObservationLocked(id)
		refreshCtx, cancel := context.WithTimeout(context.Background(), s.timeout)
		f = &flight{done: make(chan struct{}), cancel: cancel, epoch: c.Epoch, background: backgroundUsageJob(ctx)}
		if f.background != nil {
			f.background.work = append(f.background.work, f.done)
		}
		s.flights[id] = f
		go s.runRefresh(refreshCtx, id, c, f)
	} else if backgroundUsageJob(ctx) == nil {
		f.background = nil
	}
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return Account{}, ctx.Err()
	case <-f.done:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok = s.vault.Accounts[id]
	if !ok {
		return Account{}, ErrNotFound
	}
	if job := backgroundUsageJob(ctx); job != nil && c.IdentityEpoch == job.target.IdentityEpoch {
		job.epoch = c.Epoch
	}
	return s.accountLocked(c), f.err
}

func (s *Service) runRefresh(ctx context.Context, id string, old credential, f *flight) {
	defer f.cancel()
	record, err := s.refresher.Refresh(ctx, old.sdk())
	var updated credential
	if err == nil {
		updated, err = credentialFromSDK(record)
		if err == nil && updated.AccessToken == "" {
			err = ErrRefreshFailed
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer close(f.done)
	current, exists := s.vault.Accounts[id]
	if s.closed || !exists || current.Epoch != f.epoch || s.flights[id] != f || f.cancelled {
		f.err = ErrConflict
		if s.flights[id] == f {
			delete(s.flights, id)
		}
		return
	}
	delete(s.flights, id)
	s.bumpCleanupObservationLocked(id)
	if ctx.Err() != nil {
		current.Status = StatusRefreshFailed
		s.vault.Accounts[id] = current
		f.err = ErrRefreshFailed
		return
	}
	if err != nil {
		current.Status = StatusRefreshFailed
		f.err = ErrRefreshFailed
		if needsLogin(err) {
			s.invalidateRefreshLocked(id, current, f)
			return
		}
		s.vault.Accounts[id] = current
		return
	}
	if updated.AccountID != "" && updated.AccountID != old.AccountID && !(old.AccessToken == "" && old.AccountID == "") {
		// Token rotation must not change the provider account behind a selected
		// card. Mixed imports or a connector returning another account require
		// explicit login/import; never publish that account's tokens as a refresh.
		s.invalidateRefreshLocked(id, current, f)
		return
	}
	// Providers may omit an unchanged refresh token in the rotation response.
	if updated.RefreshToken == "" {
		updated.RefreshToken = old.RefreshToken
	}
	if updated.IDToken == "" {
		updated.IDToken = old.IDToken
	}
	if updated.AccountID == "" {
		updated.AccountID = old.AccountID
	}
	updated.ID = id
	mergeRotatedAccountName(&updated, current)
	updated.Epoch = current.Epoch + 1
	updated.IdentityEpoch = old.IdentityEpoch
	updated.Status = StatusReady
	if updated.Plan == "" {
		updated.Plan = old.Plan
	}
	if updated.SubscriptionActiveUntil.IsZero() {
		updated.SubscriptionActiveUntil = old.SubscriptionActiveUntil
		updated.SubscriptionObservedAt = old.SubscriptionObservedAt
		updated.SubscriptionSource = old.SubscriptionSource
	} else {
		stampSubscriptionHint(&updated, s.now())
	}
	next := cloneVault(s.vault)
	next.Accounts[id] = updated
	next.Epochs[id] = updated.Epoch
	if err = writeVault(s.path, s.protector, next); err != nil {
		current.Status = StatusSaveFailed
		s.vault.Accounts[id] = current
		s.pending[id] = pendingRotation{credential: updated, epoch: current.Epoch}
		f.err = ErrSaveFailed
		return
	}
	s.vault = next
	s.fenceUsageLocked(id)
	s.fenceSubscriptionLocked(id)
}

// invalidateRefreshLocked durably retires a refresh credential that the
// provider has declared invalid or that resolved to another provider account.
// The old access token remains available until its recorded expiry, but the
// rejected refresh token must not be retried after a normal Bridge restart.
//
// If storage is unavailable, keep the sanitized mutation private in pending so
// RetrySave can finish it. The in-memory snapshot also drops the refresh token
// immediately and reports save_failed; this process therefore fails closed
// even though no implementation can make a failed disk write survive a crash.
func (s *Service) invalidateRefreshLocked(id string, current credential, f *flight) {
	unauthorized, hadUnauthorized := s.usage[id]
	hadUnauthorized = hadUnauthorized && unauthorized.epoch == current.Epoch && unauthorized.identityEpoch == current.IdentityEpoch && unauthorized.snapshot.ErrorCode == "needs_login"
	invalid := current
	invalid.RefreshToken = ""
	invalid.Status = StatusNeedsLogin
	invalid.Epoch = current.Epoch + 1
	next := cloneVault(s.vault)
	next.Accounts[id] = invalid
	next.Epochs[id] = invalid.Epoch
	if err := writeVault(s.path, s.protector, next); err != nil {
		current.RefreshToken = ""
		current.Status = StatusSaveFailed
		s.vault.Accounts[id] = current
		s.pending[id] = pendingRotation{credential: invalid, epoch: current.Epoch}
		f.err = ErrSaveFailed
		return
	}
	s.vault = next
	s.fenceUsageLocked(id)
	s.fenceSubscriptionLocked(id)
	if hadUnauthorized {
		// This transition only retires the rejected refresh token; it keeps
		// exactly the same access token. Preserve its already-observed 401 so
		// cleanup can distinguish it from an untested, still-usable access-only
		// account. Successful rotation and identity replacement never do this.
		unauthorized.epoch = invalid.Epoch
		unauthorized.finalUnauthorized = true
		s.usage[id] = unauthorized
	}
	f.err = ErrNeedsLogin
}

// RetrySave saves an in-memory rotated refresh token after a persistence error.
// Until this succeeds the rotated token is never exposed through SDKStore.
func (s *Service) RetrySave(ctx context.Context, id string) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Account{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return Account{}, err
	}
	current, ok := s.vault.Accounts[id]
	if !ok {
		return Account{}, ErrNotFound
	}
	p, ok := s.pending[id]
	if !ok {
		return s.accountLocked(current), nil
	}
	if p.epoch != current.Epoch {
		return Account{}, ErrConflict
	}
	mergeRotatedAccountName(&p.credential, current)
	next := cloneVault(s.vault)
	next.Accounts[id] = p.credential
	next.Epochs[id] = p.credential.Epoch
	if err := writeVault(s.path, s.protector, next); err != nil {
		return s.accountLocked(current), err
	}
	s.vault = next
	delete(s.pending, id)
	s.fenceUsageLocked(id)
	s.fenceSubscriptionLocked(id)
	return s.accountLocked(p.credential), nil
}

func (s *Service) CancelRefresh(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f := s.flights[id]; f != nil {
		f.cancelled = true
		f.cancel()
	}
}
func (s *Service) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	waiters := make([]<-chan struct{}, 0, len(s.flights)+len(s.usageFlights)+len(s.resetFlights))
	if r := s.usageRefresh; r != nil {
		r.cancel()
		waiters = append(waiters, r.done)
		for job := range r.jobs {
			s.cancelUsageJobLocked(job)
			waiters = append(waiters, job.done)
		}
	}
	for _, f := range s.flights {
		waiters = append(waiters, f.done)
		f.cancel()
	}
	for _, l := range s.logins {
		if l.done != nil {
			waiters = append(waiters, l.done)
		}
		if l.session.Status == "pending" {
			l.cancel()
			l.session.Status = "cancelled"
			l.session.LoginProgress = LoginProgress{}
		}
	}
	for _, f := range s.usageFlights {
		waiters = append(waiters, f.done)
		f.cancel()
	}
	for _, f := range s.subscriptionFlights {
		waiters = append(waiters, f.done)
		f.cancel()
	}
	for _, f := range s.resetFlights {
		waiters = append(waiters, f.done)
		f.cancel()
	}
	for f := range s.importChecks {
		waiters = append(waiters, f.done)
		f.cancel()
	}
	s.usageFlights = map[string]*usageFlight{}
	s.usage = map[string]usageEntry{}
	s.subscriptionFlights = map[string]*subscriptionFlight{}
	s.subscriptions = map[string]subscriptionEntry{}
	s.pending = map[string]pendingRotation{}
	held := s.vaultLock
	s.vaultLock = nil
	s.mu.Unlock()
	// Keep the inter-process lease while cancellation is being observed. This
	// closes the small window in which a second Bridge could start a refresh
	// while the old process is still unwinding an HTTP request. A misbehaving
	// connector is bounded so shutdown cannot hang forever.
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for _, done := range waiters {
		select {
		case <-done:
		case <-deadline.C:
			waiters = nil
		}
		if waiters == nil {
			break
		}
	}
	if held != nil {
		_ = held.Close()
	}
}
func newID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}
func needsLogin(err error) bool {
	// Only a connector's normalized authentication verdict may retire tokens.
	// Network/proxy/HTML diagnostic text is never credential-invalid evidence.
	return errors.Is(err, ErrNeedsLogin)
}
