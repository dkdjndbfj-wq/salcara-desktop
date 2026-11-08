// Package usageledger stores bounded local request metadata. Provider usage is
// nullable; this is neither a billing ledger nor an account quota data source.
package usageledger

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	MaxRecords    = 4096
	maxFileBytes  = 8 << 20
	queueCapacity = 128
)

var ErrSave = errors.New("本机请求记录保存失败")

type Tokens struct {
	Input              *int64 `json:"inputTokens"`
	Output             *int64 `json:"outputTokens"`
	Total              *int64 `json:"totalTokens"`
	CachedInput        *int64 `json:"cachedInputTokens"`
	CacheCreationInput *int64 `json:"cacheCreationInputTokens"`
	ReasoningOutput    *int64 `json:"reasoningOutputTokens"`
}

// Observation contains no request/response body, credential, URL or raw error.
// Account/client/route IDs are pseudonymized before entering the bounded queue.
type Observation struct {
	StartedAt                                time.Time
	DurationMS                               int64
	AccountID, ClientID, Route, Model, State string
	// AccountEpoch fences display aggregates after an official credential is replaced.
	AccountEpoch                   uint64
	HTTPStatus, UpstreamHTTPStatus int
	Tokens                         Tokens
	UsageFinal                     bool
	ServiceTier                    string
	ProviderCost                   *Money
	FirstByteMS                    *int64
	RoutingReason, FailureStage    string
	Attempts                       []Attempt
}

type Record struct {
	ID                 string    `json:"id"`
	Time               int64     `json:"time"`
	AccountID          string    `json:"accountId,omitempty"`
	AccountEpoch       uint64    `json:"accountEpoch,omitempty"`
	ClientID           string    `json:"clientId,omitempty"`
	RouteID            string    `json:"routeId"`
	Model              string    `json:"model,omitempty"`
	State              string    `json:"state"`
	HTTPStatus         int       `json:"httpStatus"`
	UpstreamHTTPStatus int       `json:"upstreamHttpStatus,omitempty"`
	DurationMS         int64     `json:"durationMs"`
	Tokens             Tokens    `json:"tokens"`
	UsageFinal         bool      `json:"usageFinal"`
	ServiceTier        string    `json:"serviceTier,omitempty"`
	ProviderCost       *Money    `json:"providerCost"`
	EstimatedCost      *Money    `json:"estimatedCost"`
	PricingVersion     uint64    `json:"pricingVersion"`
	EstimateReason     string    `json:"estimateReason,omitempty"`
	FirstByteMS        *int64    `json:"firstByteMs,omitempty"`
	RoutingReason      string    `json:"routingReason,omitempty"`
	FailureStage       string    `json:"failureStage,omitempty"`
	Attempts           []Attempt `json:"attempts,omitempty"`
}

type Retention struct {
	MaxRecords int `json:"maxRecords"`
	Days       int `json:"days"`
}

type Snapshot struct {
	Records   []Record  `json:"records"`
	Retention Retention `json:"retention"`
	Count     int       `json:"count"`
	Pending   int       `json:"pending"`
	Dropped   uint64    `json:"dropped"`
	Unsaved   bool      `json:"unsaved"`
	SaveError string    `json:"saveError,omitempty"`
	ReadError string    `json:"readError,omitempty"`
	SavedAt   int64     `json:"savedAt,omitempty"`
	Closed    bool      `json:"closed"`
}

type diskState struct {
	revision       uint64
	reset          bool
	Version        int                        `json:"version"`
	Salt           string                     `json:"salt"`
	Retention      Retention                  `json:"retention"`
	Dropped        uint64                     `json:"dropped"`
	Records        []Record                   `json:"records"`
	Prices         PriceBook                  `json:"prices"`
	Calibrations   map[string]CostCalibration `json:"calibrations,omitempty"`
	CostCoverage   uint64                     `json:"costCoverage,omitempty"`
	CostCoverageAt time.Time                  `json:"costCoverageAt,omitempty"`
	CleanShutdown  bool                       `json:"cleanShutdown,omitempty"`
}

type command struct {
	kind            string
	ctx             context.Context
	record          Record
	retention       Retention
	done            chan error
	prices          PriceBook
	expectedVersion uint64
	calibrationID   string
	calibration     CostCalibration
}

type Ledger struct {
	mu                           sync.RWMutex
	path                         string
	salt                         []byte
	retention                    Retention
	records                      []Record
	pending                      int
	dropped                      uint64
	unsaved, closed, blockedRead bool
	saveError, readError         string
	savedAt                      int64
	revision                     uint64
	queryEpoch                   string
	costCoverage                 uint64
	costCoverageAt               time.Time
	costActivity                 map[AccountCostScope]accountCostActivity
	costOverflow                 int
	calibrations                 map[string]CostCalibration
	prices                       PriceBook
	storage                      *sqliteStorage
	queue                        chan command
	finished                     chan struct{}
	submission                   chan struct{}
	write                        func(diskState) error
}

// Open never prevents inference when storage fails. Failure remains explicit in
// Snapshot; an unreadable existing file is not overwritten until Clear succeeds.
func Open(path string) *Ledger {
	dbPath, legacy := databasePath(path)
	l := &Ledger{path: dbPath, salt: randomBytes(32), retention: Retention{MaxRecords: 1000, Days: 30}, records: []Record{}, prices: PriceBook{Version: 1, Entries: []ModelPrice{}}, queryEpoch: hex.EncodeToString(randomBytes(16)), queue: make(chan command, queueCapacity), finished: make(chan struct{}), submission: make(chan struct{}, 1)}
	// Only an absent .json path becomes a compatibility marker. Existing JSON
	// is a user-owned migration source and must remain byte-for-byte intact.
	marker := ""
	if legacy != "" {
		if _, statErr := os.Stat(legacy); errors.Is(statErr, os.ErrNotExist) {
			marker = legacy
		}
	}
	l.storage = &sqliteStorage{path: dbPath, legacyPath: marker}
	l.write = l.storage.write
	state, err := l.storage.load()
	if errors.Is(err, os.ErrNotExist) && legacy != "" {
		var raw []byte
		raw, err = readBounded(legacy)
		if err == nil {
			err = decodeState(raw, &state)
		}
	}
	if err == nil {
		l.salt, _ = hex.DecodeString(state.Salt)
		l.retention, l.records, l.dropped = state.Retention, prune(state.Records, state.Retention, time.Now()), state.Dropped
		if state.Prices.Version > 0 {
			l.prices = cloneBook(state.Prices)
		}
		l.calibrations = cloneCalibrations(state.Calibrations)
		l.costCoverage, l.costCoverageAt = state.CostCoverage, state.CostCoverageAt
		if len(l.records) != len(state.Records) {
			l.invalidateCostCoverageLocked()
		}
		// A crash can lose a last in-flight response while the provider has
		// already charged quota. Never carry that unprovable calibration across
		// an unclean restart; ordinary retained usage still remains available.
		if !state.CleanShutdown {
			l.invalidateCostCoverageLocked()
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		l.blockedRead, l.readError = true, "本机记录文件无法读取；清空后可重新保存"
	}
	if !l.blockedRead {
		_ = l.save(l.state())
	}
	go l.run()
	return l
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil
	}
	return b
}
func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if len(b) > maxFileBytes {
		return nil, ErrSave
	}
	return b, err
}

func validRetention(v Retention) bool {
	return v.MaxRecords >= 1 && v.MaxRecords <= MaxRecords && v.Days >= 1 && v.Days <= 365
}
func validState(v string) bool {
	switch v {
	case "completed", "incomplete", "failed", "interrupted", "cancelled", "unknown", "rejected", "read_only", "local_estimate":
		return true
	}
	return false
}
func validHex(v string, n int) bool {
	if len(v) != n {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}
func validAnonymous(v, prefix string) bool {
	return v == "" || strings.HasPrefix(v, prefix) && validHex(strings.TrimPrefix(v, prefix), 24)
}

// ValidateModel accepts only bounded model identifiers, excluding URL/key-like
// values. The gateway additionally removes matches to known upstream secrets.
func ValidateModel(v string) bool {
	if v == "" || len(v) > 200 || strings.Contains(v, "://") || strings.HasPrefix(strings.ToLower(v), "sk-") || strings.HasPrefix(strings.ToLower(v), "bearer") {
		return false
	}
	for i, c := range v {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || i > 0 && strings.ContainsRune("._:/-", c)) {
			return false
		}
	}
	return true
}

func sanitizeTokens(t Tokens) Tokens {
	copyValue := func(v *int64) *int64 {
		if v == nil || *v < 0 || *v > 1_000_000_000_000 {
			return nil
		}
		n := *v
		return &n
	}
	return Tokens{copyValue(t.Input), copyValue(t.Output), copyValue(t.Total), copyValue(t.CachedInput), copyValue(t.CacheCreationInput), copyValue(t.ReasoningOutput)}
}
func cloneRecord(r Record) Record {
	r.Tokens = sanitizeTokens(r.Tokens)
	r.ProviderCost, r.EstimatedCost = cloneMoney(r.ProviderCost), cloneMoney(r.EstimatedCost)
	r.FirstByteMS = diagnosticTime(r.FirstByteMS)
	r.Attempts = cloneAttempts(r.Attempts)
	return r
}
func cloneRecords(v []Record) []Record {
	out := make([]Record, len(v))
	for i, r := range v {
		out[i] = cloneRecord(r)
	}
	return out
}

func (l *Ledger) anonymous(prefix, raw string) string {
	if raw == "" || len(raw) > 1024 || len(l.salt) != 32 {
		return ""
	}
	h := hmac.New(sha256.New, l.salt)
	_, _ = h.Write([]byte(prefix + "\x00" + raw))
	return prefix + hex.EncodeToString(h.Sum(nil)[:12])
}

// Append is nonblocking with respect to disk IO. Queue overflow is observable
// and persisted as a gap count, never silently converted into zero token usage.
func (l *Ledger) Append(o Observation) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return false
	}
	if len(l.salt) != 32 {
		l.saveError = ErrSave.Error()
		l.unsaved = true
		l.dropped++
		l.revision++
		return false
	}
	id := randomBytes(16)
	if len(id) != 16 {
		l.saveError = ErrSave.Error()
		l.unsaved = true
		l.dropped++
		l.revision++
		return false
	}
	when := o.StartedAt
	if when.UnixMilli() <= 0 || when.After(time.Now().Add(5*time.Minute)) {
		when = time.Now()
	}
	state := o.State
	if !validState(state) {
		state = "unknown"
	}
	model := o.Model
	if !ValidateModel(model) {
		model = ""
	}
	duration := o.DurationMS
	if duration < 0 {
		duration = 0
	}
	if duration > 48*time.Hour.Milliseconds() {
		duration = 48 * time.Hour.Milliseconds()
	}
	status := o.HTTPStatus
	if status < 100 || status > 599 {
		status = 0
	}
	upstream := o.UpstreamHTTPStatus
	if upstream < 100 || upstream > 599 {
		upstream = 0
	}
	route := l.anonymous("route-", o.Route)
	if route == "" {
		route = l.anonymous("route-", "unknown")
	}
	r := Record{ID: hex.EncodeToString(id), Time: when.UnixMilli(), AccountID: l.anonymous("acct-", o.AccountID), AccountEpoch: o.AccountEpoch, ClientID: l.anonymous("client-", o.ClientID), RouteID: route, Model: model, State: state, HTTPStatus: status, UpstreamHTTPStatus: upstream, DurationMS: duration, Tokens: sanitizeTokens(o.Tokens), UsageFinal: o.UsageFinal}
	l.addDiagnostics(&r, o)
	if validTier(o.ServiceTier) {
		r.ServiceTier = o.ServiceTier
	}
	r.ProviderCost = providerMoney(o.ProviderCost)
	select {
	case l.queue <- command{kind: "append", record: r}:
		l.pending++
		l.unsaved = true
		return true
	default:
		l.dropped++
		l.revision++
		l.unsaved = true
		return false
	}
}

func (l *Ledger) state() diskState {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return diskState{revision: l.revision, Version: 2, Salt: hex.EncodeToString(l.salt), Retention: l.retention, Dropped: l.dropped, Records: cloneRecords(l.records), Prices: cloneBook(l.prices), Calibrations: cloneCalibrations(l.calibrations), CostCoverage: l.costCoverage, CostCoverageAt: l.costCoverageAt}
}
func (l *Ledger) save(state diskState) error {
	l.mu.RLock()
	blocked := l.blockedRead
	write := l.write
	l.mu.RUnlock()
	var err error
	if blocked || len(l.salt) != 32 || l.path == "" {
		err = ErrSave
	} else {
		err = write(state)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		l.saveError = ErrSave.Error()
		l.unsaved = true
		return ErrSave
	}
	l.saveError = ""
	l.unsaved = l.pending > 0 || l.revision != state.revision
	l.savedAt = time.Now().UnixMilli()
	return nil
}

func prune(records []Record, retention Retention, now time.Time) []Record {
	cutoff := now.Add(-time.Duration(retention.Days) * 24 * time.Hour).UnixMilli()
	out := make([]Record, 0, len(records))
	for _, r := range records {
		if r.Time >= cutoff {
			out = append(out, r)
		}
	}
	if len(out) > retention.MaxRecords {
		out = out[len(out)-retention.MaxRecords:]
	}
	// Attempt traces share the original disk budget. Keep newest records rather
	// than letting diagnostic growth make every future database save fail.
	bytesLeft := maxFileBytes - (2 << 20)
	for i := len(out) - 1; i >= 0; i-- {
		encoded, err := json.Marshal(out[i])
		if err != nil || len(encoded)+1 > bytesLeft {
			return out[i+1:]
		}
		bytesLeft -= len(encoded) + 1
	}
	return out
}

func (l *Ledger) run() {
	defer close(l.finished)
	defer func() {
		if l.storage.close() != nil {
			l.mu.Lock()
			l.saveError, l.unsaved = ErrSave.Error(), true
			l.mu.Unlock()
		}
	}()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		var cmd command
		select {
		case cmd = <-l.queue:
		case <-ticker.C:
			l.mu.Lock()
			kept := prune(l.records, l.retention, time.Now())
			changed := len(kept) != len(l.records)
			if changed {
				l.invalidateCostCoverageLocked()
				l.records = kept
				l.revision++
				l.unsaved = true
			}
			l.mu.Unlock()
			if changed {
				_ = l.save(l.state())
			}
			continue
		}
		// A queued control command may have timed out while disk IO was busy.
		// Do not begin a cancelled mutation. Cancellation during an atomic write
		// still has an uncertain outcome, which callers must report as such.
		if cmd.ctx != nil && cmd.ctx.Err() != nil {
			if cmd.done != nil {
				cmd.done <- cmd.ctx.Err()
				close(cmd.done)
			}
			continue
		}
		var err error
		switch cmd.kind {
		case "calibration":
			l.mu.Lock()
			if cmd.calibration.Coverage == l.costCoverage && cmd.calibration.PriceKey == priceKey(l.prices) && !l.blockedRead {
				if l.calibrations == nil {
					l.calibrations = map[string]CostCalibration{}
				}
				old := l.calibrations[cmd.calibrationID]
				if old.Epoch <= cmd.calibration.Epoch && (old.Epoch != cmd.calibration.Epoch || !cmd.calibration.At.Before(old.At)) {
					for id, v := range l.calibrations {
						if !v.Reset.After(time.Now()) {
							delete(l.calibrations, id)
						}
					}
					if _, exists := l.calibrations[cmd.calibrationID]; exists || len(l.calibrations) < MaxRecords {
						l.calibrations[cmd.calibrationID] = cmd.calibration
						l.revision++
					}
				}
			}
			l.mu.Unlock()
			err = l.save(l.state())
		case "append":
			l.mu.Lock()
			l.pending--
			l.revision++
			// Keep observed Token counters. Historical monetary fields remain
			// readable, but new requests no longer run automatic cost estimation.
			previousLen := len(l.records)
			l.records = prune(append(l.records, cmd.record), l.retention, time.Now())
			if len(l.records) != previousLen+1 {
				l.invalidateCostCoverageLocked()
			}
			l.mu.Unlock()
			err = l.save(l.state())
		case "flush", "close":
			state := l.state()
			state.CleanShutdown = cmd.kind == "close"
			err = l.save(state)
		case "clear", "retention", "pricing", "reprice":
			candidate := l.state()
			l.mu.RLock()
			blocked, write := l.blockedRead, l.write
			l.mu.RUnlock()
			previousDrops := candidate.Dropped
			if cmd.kind == "clear" {
				candidate.Records = []Record{}
				candidate.Dropped = 0
				candidate.reset = blocked
			} else if cmd.kind == "retention" {
				candidate.Retention = cmd.retention
				candidate.Records = prune(candidate.Records, cmd.retention, time.Now())
			} else if candidate.Prices.Version != cmd.expectedVersion {
				err = ErrRevision
			} else if cmd.kind == "pricing" {
				candidate.Prices = cloneBook(cmd.prices)
				candidate.Prices.Version = cmd.expectedVersion + 1
				candidate.Prices.UpdatedAt = time.Now().UnixMilli()
			} else {
				for i := range candidate.Records {
					priceRecord(&candidate.Records[i], candidate.Prices)
				}
			}
			if err == nil {
				candidate.CostCoverage++
				candidate.CostCoverageAt = time.Now()
				candidate.Calibrations = nil
			}
			if err != nil {
				// A stale pricing editor must not change either disk or save status.
			} else if len(l.salt) != 32 || l.path == "" || blocked && cmd.kind != "clear" {
				err = ErrSave
			} else {
				err = write(candidate)
			}
			l.mu.Lock()
			if err == nil {
				l.costCoverage, l.costCoverageAt = candidate.CostCoverage, candidate.CostCoverageAt
				l.calibrations = nil // clear, retention and price edits invalidate learned samples
				l.records, l.retention = candidate.Records, candidate.Retention
				l.prices = cloneBook(candidate.Prices)
				if cmd.kind == "clear" {
					l.dropped -= previousDrops
				}
				l.blockedRead = false
				l.readError = ""
				l.saveError = ""
				l.savedAt = time.Now().UnixMilli()
				l.unsaved = l.pending > 0 || l.revision != candidate.revision
				l.revision++
			} else if !errors.Is(err, ErrRevision) {
				l.saveError = ErrSave.Error()
				l.unsaved = true
				err = ErrSave
			}
			l.mu.Unlock()
		}
		if cmd.done != nil {
			cmd.done <- err
			close(cmd.done)
		}
		if cmd.kind == "close" {
			return
		}
	}
}

func (l *Ledger) request(ctx context.Context, cmd command) error {
	if l == nil {
		return ErrSave
	}
	cmd.done = make(chan error, 1)
	cmd.ctx = ctx
	// Serialize acceptance with Close without holding the metadata mutex
	// while waiting for queue capacity. Append remains nonblocking.
	select {
	case l.submission <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-l.finished:
		return ErrSave
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		<-l.submission
		return ErrSave
	}
	l.mu.Unlock()
	select {
	case l.queue <- cmd:
	case <-ctx.Done():
		<-l.submission
		return ctx.Err()
	}
	<-l.submission
	select {
	case err := <-cmd.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-l.finished:
		// The worker sends command completion before exiting. Prefer that
		// exact result when Close and this waiter wake at the same time.
		select {
		case err := <-cmd.done:
			return err
		default:
			return ErrSave
		}
	}
}
func (l *Ledger) Flush(ctx context.Context) error { return l.request(ctx, command{kind: "flush"}) }
func (l *Ledger) Clear(ctx context.Context) error { return l.request(ctx, command{kind: "clear"}) }
func (l *Ledger) SetRetention(ctx context.Context, v Retention) error {
	if !validRetention(v) {
		return errors.New("保留条数为 1–4096，保留天数为 1–365")
	}
	return l.request(ctx, command{kind: "retention", retention: v})
}
func (l *Ledger) Close(ctx context.Context) error {
	if l == nil {
		return nil
	}
	select {
	case l.submission <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-l.finished:
		return l.closeResult()
	}
	l.mu.Lock()
	already := l.closed
	l.closed = true
	l.mu.Unlock()
	<-l.submission
	if !already {
		go func() { l.queue <- command{kind: "close"} }()
	}
	select {
	case <-l.finished:
		return l.closeResult()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *Ledger) closeResult() error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.saveError != "" {
		return ErrSave
	}
	return nil
}

// Anonymous IDs expose only the persisted pseudonymization, for transient
// authenticated UI labels. Raw IDs and labels never enter the disk records.
func (l *Ledger) AnonymousAccountID(id string) string {
	if l == nil {
		return ""
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.anonymous("acct-", id)
}
func (l *Ledger) AnonymousClientID(id string) string {
	if l == nil {
		return ""
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.anonymous("client-", id)
}

// Snapshot returns newest-first detached records and prunes expired rows even
// during idle periods; no raw local identifiers or persistence paths are exposed.
func (l *Ledger) Snapshot(limit int, before string) (Snapshot, error) {
	if l == nil {
		return Snapshot{}, ErrSave
	}
	if limit < 1 || limit > 200 {
		return Snapshot{}, errors.New("读取条数为 1–200")
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	records := prune(l.records, l.retention, time.Now())
	out := Snapshot{Retention: l.retention, Count: len(records), Pending: l.pending, Dropped: l.dropped, Unsaved: l.unsaved, SaveError: l.saveError, ReadError: l.readError, SavedAt: l.savedAt, Closed: l.closed, Records: []Record{}}
	end := len(records)
	if before != "" {
		found := false
		for i, r := range records {
			if r.ID == before {
				end = i
				found = true
				break
			}
		}
		if !found {
			return Snapshot{}, errors.New("记录游标已过期，请刷新")
		}
	}
	for i := end - 1; i >= 0 && len(out.Records) < limit; i-- {
		out.Records = append(out.Records, cloneRecord(records[i]))
	}
	return out, nil
}

func decodeState(raw []byte, state *diskState) error {
	if decodeStrict(raw, state) != nil || state.Version != 1 && state.Version != 2 || !validHex(state.Salt, 64) || !validRetention(state.Retention) || state.Records == nil || len(state.Records) > MaxRecords || validatePrices(state.Prices.Entries) != nil || state.Version == 2 && state.Prices.Version == 0 || state.Prices.Version > maxPriceVersion || state.Prices.UpdatedAt < 0 || state.Prices.UpdatedAt > 1e16 {
		return ErrSave
	}
	if len(state.Calibrations) > MaxRecords {
		return ErrSave
	}
	for id, v := range state.Calibrations {
		if !validAnonymous(id, "acct-") || id == "" || !validCostCalibration(v) {
			return ErrSave
		}
	}
	seen := map[string]bool{}
	for _, r := range state.Records {
		if !validDiagnostics(r) {
			return ErrSave
		}
		if !validHex(r.ID, 32) || seen[r.ID] || r.Time <= 0 || !validAnonymous(r.AccountID, "acct-") || !validAnonymous(r.ClientID, "client-") || !validAnonymous(r.RouteID, "route-") || r.RouteID == "" || r.Model != "" && !ValidateModel(r.Model) || !validState(r.State) || r.DurationMS < 0 || r.DurationMS > 48*time.Hour.Milliseconds() || r.HTTPStatus < 0 || r.HTTPStatus > 599 || r.UpstreamHTTPStatus < 0 || r.UpstreamHTTPStatus > 599 {
			return ErrSave
		}
		seen[r.ID] = true
		if !tokensValid(r.Tokens) {
			return ErrSave
		}
		if !validTier(r.ServiceTier) || !validMoney(r.ProviderCost, "provider") || !validEstimateMoney(r.EstimatedCost) || !validEstimateReason(r.EstimateReason) {
			return ErrSave
		}
		if r.PricingVersion > maxPriceVersion || r.EstimatedCost != nil && r.EstimatedCost.Version != r.PricingVersion {
			return ErrSave
		}
	}
	return nil
}

func decodeStrict(raw []byte, value any) error {
	if !uniqueJSON(raw) {
		return ErrSave
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if d.Decode(value) != nil {
		return ErrSave
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return ErrSave
	}
	return nil
}

func tokensValid(t Tokens) bool {
	for _, v := range []*int64{t.Input, t.Output, t.Total, t.CachedInput, t.CacheCreationInput, t.ReasoningOutput} {
		if v != nil && (*v < 0 || *v > 1_000_000_000_000) {
			return false
		}
	}
	return true
}

// Reject duplicate/case-confusable fields throughout the small private file.
func uniqueJSON(raw []byte) bool {
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	var walk func(int) bool
	walk = func(depth int) bool {
		if depth > 20 {
			return false
		}
		token, err := d.Token()
		if err != nil {
			return false
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return true
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				s, ok := key.(string)
				if err != nil || !ok || seen[strings.ToLower(s)] {
					return false
				}
				seen[strings.ToLower(s)] = true
				if !walk(depth + 1) {
					return false
				}
			}
			last, err := d.Token()
			return err == nil && last == json.Delim('}')
		case '[':
			for d.More() {
				if !walk(depth + 1) {
					return false
				}
			}
			last, err := d.Token()
			return err == nil && last == json.Delim(']')
		}
		return false
	}
	if !walk(0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}
