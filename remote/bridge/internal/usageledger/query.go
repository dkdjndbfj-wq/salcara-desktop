package usageledger

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
)

var ErrQuery = errors.New("本机记录查询参数无效")
var ErrRead = errors.New("本机记录数据库读取失败，请查看保存状态")

type QueryFilter struct {
	ClientRequestsOnly bool   `json:"-"`
	From               int64  `json:"from"`
	To                 int64  `json:"to"`
	AccountID          string `json:"accountId"`
	ClientID           string `json:"clientId"`
	RouteID            string `json:"routeId"`
	Model              string `json:"model"`
	ModelContains      string `json:"modelContains"`
	State              string `json:"state"`
	HTTPStatus         *int   `json:"httpStatus"`
	ServiceTier        string `json:"serviceTier"`
	Sort               string `json:"sort"`
	Direction          string `json:"direction"`
	Limit              int    `json:"limit"`
	Offset             int    `json:"offset"`
	Revision           string `json:"revision"`
}

type NumberSummary struct {
	Sum            *int64 `json:"sum"`
	KnownRecords   int    `json:"knownRecords"`
	UnknownRecords int    `json:"unknownRecords"`
	Complete       bool   `json:"complete"`
}
type CurrencyTotal struct {
	Currency string   `json:"currency"`
	Amount   float64  `json:"amount"`
	Versions []uint64 `json:"versions"`
}
type CostSummary struct {
	Source         string          `json:"source"`
	Currencies     []CurrencyTotal `json:"currencies"`
	KnownRecords   int             `json:"knownRecords"`
	UnknownRecords int             `json:"unknownRecords"`
	Complete       bool            `json:"complete"`
}
type Summary struct {
	Records                  int            `json:"records"`
	States                   map[string]int `json:"states"`
	HTTPStatuses             map[string]int `json:"httpStatuses"`
	DurationMS               NumberSummary  `json:"durationMs"`
	InputTokens              NumberSummary  `json:"inputTokens"`
	OutputTokens             NumberSummary  `json:"outputTokens"`
	TotalTokens              NumberSummary  `json:"totalTokens"`
	CachedInputTokens        NumberSummary  `json:"cachedInputTokens"`
	CacheCreationInputTokens NumberSummary  `json:"cacheCreationInputTokens"`
	ReasoningOutputTokens    NumberSummary  `json:"reasoningOutputTokens"`
	ProviderCosts            CostSummary    `json:"providerCosts"`
	EstimatedCosts           CostSummary    `json:"estimatedCosts"`
}
type GroupSummary struct {
	Key     string  `json:"key"`
	Summary Summary `json:"summary"`
}
type TrendSummary struct {
	Time    int64   `json:"time"`
	Summary Summary `json:"summary"`
}
type QueryResult struct {
	Records     []Record       `json:"records"`
	Total       int            `json:"total"`
	Limit       int            `json:"limit"`
	Offset      int            `json:"offset"`
	Revision    string         `json:"revision"`
	Summary     Summary        `json:"summary"`
	ByModel     []GroupSummary `json:"byModel"`
	ByAccount   []GroupSummary `json:"byAccount"`
	ByClient    []GroupSummary `json:"byClient"`
	ByRoute     []GroupSummary `json:"byRoute"`
	Trend       []TrendSummary `json:"trend"`
	Status      Snapshot       `json:"status"`
	DurableOnly bool           `json:"durableOnly"`
}

func normalizeFilter(f QueryFilter) (QueryFilter, error) {
	if f.Limit == 0 {
		f.Limit = 100
	}
	if f.Sort == "" {
		f.Sort = "time"
	}
	if f.Direction == "" {
		f.Direction = "desc"
	}
	if f.From < 0 || f.To < 0 || f.From > 1e16 || f.To > 1e16 || f.To > 0 && f.From > f.To || f.Limit < 1 || f.Limit > 200 || f.Offset < 0 || f.Offset > MaxRecords || len(f.Revision) > 96 || f.Offset > 0 && f.Revision == "" || !validAnonymous(f.AccountID, "acct-") || !validAnonymous(f.ClientID, "client-") || !validAnonymous(f.RouteID, "route-") || f.Model != "" && !ValidateModel(f.Model) || f.ModelContains != "" && !ValidateModel(f.ModelContains) || f.State != "" && !validState(f.State) || !validTier(f.ServiceTier) || f.HTTPStatus != nil && (*f.HTTPStatus < 0 || *f.HTTPStatus > 599) {
		return f, ErrQuery
	}
	switch f.Sort {
	case "time", "duration", "model", "state":
	default:
		return f, ErrQuery
	}
	if f.Direction != "asc" && f.Direction != "desc" {
		return f, ErrQuery
	}
	return f, nil
}

// Query reads the durable database only. Unsaved/pending memory observations
// remain visible through status and the original Snapshot API. Revision binds
// offset pagination to both this process and the committed database contents.
func (l *Ledger) Query(ctx context.Context, f QueryFilter) (QueryResult, error) {
	var out QueryResult
	if l == nil {
		return out, ErrRead
	}
	f, err := normalizeFilter(f)
	if err != nil {
		return out, err
	}
	read, err := l.readQueryRecords(ctx, f)
	if err != nil {
		return out, err
	}
	records := read.records
	out = QueryResult{Records: []Record{}, Total: len(records), Limit: f.Limit, Offset: f.Offset, Revision: read.revision, Summary: summarizeTokens(records), ByModel: groupBy(records, func(r Record) string { return r.Model }), ByAccount: groupBy(records, func(r Record) string { return r.AccountID }), ByClient: groupBy(records, func(r Record) string { return r.ClientID }), ByRoute: groupBy(records, func(r Record) string { return r.RouteID }), Trend: []TrendSummary{}, Status: read.status, DurableOnly: true}
	days := map[int64][]Record{}
	for _, r := range records {
		day := r.Time / (24 * time.Hour.Milliseconds()) * (24 * time.Hour.Milliseconds())
		days[day] = append(days[day], r)
	}
	for stamp, rows := range days {
		out.Trend = append(out.Trend, TrendSummary{stamp, summarizeTokens(rows)})
	}
	sort.Slice(out.Trend, func(i, j int) bool { return out.Trend[i].Time < out.Trend[j].Time })
	if f.Offset < len(records) {
		end := min(f.Offset+f.Limit, len(records))
		out.Records = cloneRecords(records[f.Offset:end])
	}
	return out, nil
}

type queryRead struct {
	records  []Record
	status   Snapshot
	revision string
	cutoff   int64
}

// readQueryRecords is the shared durable-read fence. Callers must normalize the
// filter first. Storage limits every read to MaxRecords, independent of paging.
func (l *Ledger) readQueryRecords(ctx context.Context, f QueryFilter) (queryRead, error) {
	if l == nil {
		return queryRead{}, ErrRead
	}
	l.mu.RLock()
	before, epoch, retention, blocked, closed := l.revision, l.queryEpoch, l.retention, l.blockedRead, l.closed
	l.mu.RUnlock()
	if blocked || closed {
		return queryRead{}, ErrRead
	}
	cutoff := time.Now().Add(-time.Duration(retention.Days) * 24 * time.Hour).UnixMilli()
	records, durableRevision, err := l.storage.query(ctx, f, cutoff)
	if err != nil {
		if ctx.Err() != nil {
			return queryRead{}, ctx.Err()
		}
		return queryRead{}, ErrRead
	}
	l.mu.RLock()
	if l.revision != before || l.closed || l.blockedRead {
		l.mu.RUnlock()
		return queryRead{}, ErrRevision
	}
	status := Snapshot{Records: []Record{}, Retention: l.retention, Count: len(prune(l.records, l.retention, time.Now())), Pending: l.pending, Dropped: l.dropped, Unsaved: l.unsaved, SaveError: l.saveError, ReadError: l.readError, SavedAt: l.savedAt, Closed: l.closed}
	l.mu.RUnlock()
	revision := epoch + "-" + strconv.FormatUint(before, 10) + "-" + strconv.FormatUint(durableRevision, 10)
	if f.Revision != "" && f.Revision != revision {
		return queryRead{}, ErrRevision
	}
	return queryRead{records: records, status: status, revision: revision, cutoff: cutoff}, nil
}

func (s *sqliteStorage) query(ctx context.Context, f QueryFilter, cutoff int64) ([]Record, uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil, 0, ErrRead
	}
	clauses := []string{"time >= ?"}
	args := []any{cutoff}
	for _, v := range []struct {
		column string
		value  any
		use    bool
	}{{"time >=", f.From, f.From > 0}, {"time <=", f.To, f.To > 0}, {"account_id =", f.AccountID, f.AccountID != ""}, {"client_id =", f.ClientID, f.ClientID != ""}, {"route_id =", f.RouteID, f.RouteID != ""}, {"model =", f.Model, f.Model != ""}, {"state =", f.State, f.State != ""}} {
		if v.use {
			clauses = append(clauses, v.column+" ?")
			args = append(args, v.value)
		}
	}
	if f.HTTPStatus != nil {
		clauses = append(clauses, "http_status = ?")
		args = append(args, *f.HTTPStatus)
	}
	if f.ModelContains != "" {
		clauses = append(clauses, `model LIKE ? ESCAPE '\'`)
		args = append(args, "%"+strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(f.ModelContains)+"%")
	}
	column := map[string]string{"time": "time", "duration": "duration", "model": "model", "state": "state"}[f.Sort]
	direction := map[string]string{"asc": "ASC", "desc": "DESC"}[f.Direction]
	rows, err := s.db.QueryContext(ctx, "SELECT payload FROM request_records WHERE "+strings.Join(clauses, " AND ")+" ORDER BY "+column+" "+direction+",record_id "+direction+" LIMIT ?", append(args, MaxRecords+1)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	records := []Record{}
	for rows.Next() {
		var raw string
		var r Record
		if rows.Scan(&raw) != nil || len(raw) > 4096 || decodeStrict([]byte(raw), &r) != nil || !validDiagnostics(r) {
			return nil, 0, ErrRead
		}
		if (!f.ClientRequestsOnly || r.ClientID != "") && (f.ServiceTier == "" || r.ServiceTier == f.ServiceTier) {
			records = append(records, r)
		}
	}
	if rows.Err() != nil || len(records) > MaxRecords {
		return nil, 0, ErrRead
	}
	return records, s.revision, nil
}

func sumNumbers(records []Record, value func(Record) *int64) NumberSummary {
	out := NumberSummary{}
	for _, r := range records {
		if v := value(r); v != nil {
			if out.Sum == nil {
				out.Sum = new(int64)
			}
			*out.Sum += *v
			out.KnownRecords++
		} else {
			out.UnknownRecords++
		}
	}
	out.Complete = out.UnknownRecords == 0
	return out
}
func sumCosts(records []Record, source string, value func(Record) *Money) CostSummary {
	out := CostSummary{Source: source, Currencies: []CurrencyTotal{}}
	currencies := map[string]*CurrencyTotal{}
	sources := map[string]bool{}
	for _, r := range records {
		v := value(r)
		if v == nil {
			out.UnknownRecords++
			continue
		}
		out.KnownRecords++
		sources[v.Source] = true
		c := currencies[v.Currency]
		if c == nil {
			c = &CurrencyTotal{Currency: v.Currency, Versions: []uint64{}}
			currencies[v.Currency] = c
		}
		c.Amount += v.Amount
		found := false
		for _, version := range c.Versions {
			if version == v.Version {
				found = true
			}
		}
		if !found {
			c.Versions = append(c.Versions, v.Version)
		}
	}
	for _, c := range currencies {
		sort.Slice(c.Versions, func(i, j int) bool { return c.Versions[i] < c.Versions[j] })
		out.Currencies = append(out.Currencies, *c)
	}
	sort.Slice(out.Currencies, func(i, j int) bool { return out.Currencies[i].Currency < out.Currencies[j].Currency })
	out.Complete = out.UnknownRecords == 0
	if source != "provider" {
		for key := range sources {
			out.Source = key
		}
		if len(sources) > 1 {
			out.Source = "mixed"
		}
	}
	return out
}
func summarizeTokens(records []Record) Summary {
	out := Summary{Records: len(records), States: map[string]int{}, HTTPStatuses: map[string]int{}}
	for _, r := range records {
		out.States[r.State]++
		out.HTTPStatuses[strconv.Itoa(r.HTTPStatus)]++
	}
	out.DurationMS = sumNumbers(records, func(r Record) *int64 { return &r.DurationMS })
	out.InputTokens = sumNumbers(records, func(r Record) *int64 { return r.Tokens.Input })
	out.OutputTokens = sumNumbers(records, func(r Record) *int64 { return r.Tokens.Output })
	out.TotalTokens = sumNumbers(records, func(r Record) *int64 { return r.Tokens.Total })
	out.CachedInputTokens = sumNumbers(records, func(r Record) *int64 { return r.Tokens.CachedInput })
	out.CacheCreationInputTokens = sumNumbers(records, func(r Record) *int64 { return r.Tokens.CacheCreationInput })
	out.ReasoningOutputTokens = sumNumbers(records, func(r Record) *int64 { return r.Tokens.ReasoningOutput })
	return out
}

// Retained for explicit legacy monetary tools; ordinary queries use Token
// summaries and do not total historical or newly observed charges.
func summarize(records []Record) Summary {
	out := summarizeTokens(records)
	out.ProviderCosts = sumCosts(records, "provider", func(r Record) *Money { return r.ProviderCost })
	out.EstimatedCosts = sumCosts(records, "user_pricebook", func(r Record) *Money { return r.EstimatedCost })
	return out
}
func groupBy(records []Record, value func(Record) string) []GroupSummary {
	groups := map[string][]Record{}
	for _, r := range records {
		key := value(r)
		groups[key] = append(groups[key], r)
	}
	out := []GroupSummary{}
	for key, rows := range groups {
		out = append(out, GroupSummary{key, summarizeTokens(rows)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
