package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/localusage"
	"salcara/bridge/internal/usageledger"
)

func selectedUsageRequest(s *Server, h http.Handler, id string) *httptest.ResponseRecorder {
	return localRequest(s, h, "GET", "/api/usage/overview?days=7&source="+url.QueryEscape(id), nil)
}

func selectedUsageResult(t *testing.T, w *httptest.ResponseRecorder) usageOverviewResponse {
	t.Helper()
	var out usageOverviewResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err, w.Code, w.Body)
	}
	return out
}

func appendOverviewRecord(t *testing.T, s *Server, id string, epoch uint64, when time.Time, total *int64) {
	t.Helper()
	if !s.usageLedger.Append(usageledger.Observation{StartedAt: when, AccountID: id, AccountEpoch: epoch, Model: "synthetic-model", State: "completed", UsageFinal: true, HTTPStatus: 200, Tokens: usageledger.Tokens{Total: total}}) {
		t.Fatal("synthetic record was not accepted")
	}
}

func noNativeUsageForSelection(t *testing.T) {
	t.Helper()
	localUsageOnce.Do(func() {})
	before := localUsage
	// Calling the global scanner from a selected request would panic. No native
	// usage file is opened, even when the selected provider has no statistics.
	localUsage = nil
	t.Cleanup(func() { localUsage = before })
}

func TestUsageOverviewSelectedAPIKeepsDuplicateChoicesAndLocalProvenance(t *testing.T) {
	noNativeUsageForSelection(t)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer synthetic-shared" || r.URL.Query().Get("days") != "7" {
			t.Errorf("wrong selected provider request: %s %s", r.Header.Get("Authorization"), r.URL)
		}
		_, _ = w.Write([]byte(`{"remaining":42,"usage":{"today":{"total_tokens":3,"requests":1}}}`))
	}))
	defer provider.Close()
	s, h := newTestServer(t)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.RelayRoot, c.AccountKey = provider.URL, "synthetic-relay"
		c.LocalAccounts = []config.LocalAccount{
			{ID: "first", Name: "same name", BaseURL: provider.URL, Key: "synthetic-shared"},
			{ID: "duplicate", Name: "same name", BaseURL: provider.URL + "/v1", Key: "synthetic-shared"},
			{ID: "other", Name: "same name", BaseURL: "https://never-called.fixture.test", Key: "synthetic-other"},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	one, other := int64(11), int64(999)
	appendOverviewRecord(t, s, "duplicate", 0, time.Now(), &one)
	appendOverviewRecord(t, s, "first", 0, time.Now(), &other)
	appendOverviewRecord(t, s, "other", 0, time.Now(), &other)
	flushConsoleLedger(t, s)
	w := selectedUsageRequest(s, h, "duplicate")
	out := selectedUsageResult(t, w)
	if w.Code != 200 || out.SelectedSource != "duplicate" || out.RangeEnd != time.Now().Format("2006-01-02") || len(out.Sources) != 1 || len(out.Options) != 4 || calls.Load() != 1 {
		t.Fatal("selected source or complete menu lost", w.Code, w.Body)
	}
	src := out.Sources[0]
	if src.ID != "duplicate" || !src.OK || src.Usage["remaining"] != float64(42) || src.LocalUsage == nil || src.LocalUsage.Usage.Today.Tokens == nil || *src.LocalUsage.Usage.Today.Tokens != 11 || src.LocalUsage.Usage.Today.Requests != 1 {
		t.Fatal("provider and exact-account local history were mixed", w.Body)
	}
	if src.Usage["usage"].(map[string]any)["today"].(map[string]any)["total_tokens"] != float64(3) {
		t.Fatal("local Tokens overwrote upstream fields", w.Body)
	}
	for _, secret := range []string{"synthetic-shared", "synthetic-other", "synthetic-relay"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("provider credential leaked")
		}
	}
	appendOverviewRecord(t, s, "duplicate", 0, time.Now(), nil)
	flushConsoleLedger(t, s)
	w = selectedUsageRequest(s, h, "duplicate")
	out = selectedUsageResult(t, w)
	if w.Code != 200 || out.Sources[0].LocalUsage.Usage.Today.Tokens != nil || out.Sources[0].LocalUsage.Usage.Today.Requests != 2 || !out.Sources[0].LocalUsage.Partial {
		t.Fatal("missing Token count became zero or cached history hid a new request", w.Body)
	}
	localJSON, _ := json.Marshal(out.Sources[0].LocalUsage)
	if strings.Contains(string(localJSON), "cost") || strings.Contains(string(localJSON), "remaining") || strings.Contains(string(localJSON), "amount") {
		t.Fatal("local fallback exposed money or a provider balance", string(localJSON))
	}
}

func TestUsageOverviewSelectedUnavailableProviderAndUnreadableLedgerStayDistinct(t *testing.T) {
	noNativeUsageForSelection(t)
	provider := httptest.NewServer(http.NotFoundHandler())
	defer provider.Close()
	s, h := newTestServer(t)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.LocalAccounts = []config.LocalAccount{{ID: "empty", BaseURL: provider.URL, Key: "synthetic-key"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := selectedUsageRequest(s, h, "empty")
	out := selectedUsageResult(t, w)
	if w.Code != 200 || out.Sources[0].OK || out.Sources[0].Error == "" || out.Sources[0].LocalUsage == nil || out.Sources[0].LocalUsage.Usage.Today.Tokens == nil || *out.Sources[0].LocalUsage.Usage.Today.Tokens != 0 {
		t.Fatal("readable empty local history must remain a known zero despite provider failure", w.Code, w.Body)
	}
	s.CloseUsageLedger()
	w = selectedUsageRequest(s, h, "empty")
	out = selectedUsageResult(t, w)
	if w.Code != 200 || out.Sources[0].LocalUsage != nil || out.Sources[0].LocalUsageError == "" {
		t.Fatal("unreadable ledger became an empty successful history", w.Code, w.Body)
	}
}

func TestUsageOverviewSelectedMissingDeletedAndAmbiguousSourceNeverFallsBack(t *testing.T) {
	noNativeUsageForSelection(t)
	s, h := newTestServer(t)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.LocalAccounts = []config.LocalAccount{{ID: "kept", Name: "Kept", SourceType: "oauth"}, {ID: "deleted", Name: "Deleted", SourceType: "oauth"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.d.Store.Update(func(c *config.Config) error { c.LocalAccounts = c.LocalAccounts[:1]; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"missing", "deleted"} {
		w := selectedUsageRequest(s, h, id)
		out := selectedUsageResult(t, w)
		if w.Code != 404 || out.SelectedSource != id || out.Error == "" || len(out.Sources) != 0 || len(out.Options) != 1 || out.Options[0].ID != "kept" {
			t.Fatal("invalid source silently became overview or lost recovery choices", w.Code, w.Body)
		}
	}
	for _, query := range []string{"source=kept&source=deleted", "source=kept%20", "source=%ZZ"} {
		w := localRequest(s, h, "GET", "/api/usage/overview?"+query, nil)
		out := selectedUsageResult(t, w)
		if w.Code != 400 || len(out.Options) != 1 || len(out.Sources) != 0 {
			t.Fatal("ambiguous source query accepted", query, w.Code, w.Body)
		}
	}
}

func TestUsageOverviewSelectedCalendarPagesAndEpochAreExact(t *testing.T) {
	s, _ := newTestServer(t)
	loc := time.FixedZone("fixture-plus-eight", 8*60*60)
	now := time.Now().In(loc).AddDate(0, 0, -2)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	start, end := day.AddDate(0, 0, -2), day.AddDate(0, 0, 1)
	add := func(id string, epoch uint64, at time.Time, tokens int64) {
		appendOverviewRecord(t, s, id, epoch, at, &tokens)
	}
	add("chosen", 9, start.Add(-time.Millisecond), 1000)
	add("chosen", 9, start, 3)
	add("chosen", 9, day.Add(-time.Millisecond), 5)
	add("chosen", 9, day, 7)
	add("chosen", 9, end.Add(-time.Millisecond), 11)
	add("chosen", 9, end, 1000)
	add("other", 9, day, 1000)
	add("chosen", 8, day, 1000)
	add("chosen", 0, day, 1000)
	appendOverviewRecord(t, s, "chosen", 9, day.Add(-time.Hour), nil)
	for i := range 201 {
		add("chosen", 9, day.Add(time.Hour), 1)
		if i%32 == 0 {
			flushConsoleLedger(t, s)
		}
	}
	flushConsoleLedger(t, s)
	out, err := s.usageOverviewAccountTokens(context.Background(), "chosen", 9, 3, now)
	if err != nil || len(out.Daily) != 3 {
		t.Fatal(err, out)
	}
	if *out.Daily[0].Tokens != 3 || out.Daily[1].Tokens != nil || out.Daily[1].Requests != 2 || *out.Daily[2].Tokens != 219 || out.Daily[2].Requests != 203 || *out.Usage.Today.Tokens != 219 {
		t.Fatalf("local midnight/To boundary, page, identity, or unknown totals mixed: %+v", out)
	}
	if out.Daily[0].Date != start.Format("2006-01-02") || out.Daily[2].Date != day.Format("2006-01-02") || len(out.Models) != 1 || out.Models[0].Tokens != nil || out.Models[0].Requests != 206 {
		t.Fatalf("model/calendar aggregate lost selected records: %+v", out)
	}
}

func TestUsageOverviewSelectedRetentionAndUnfinishedRecordsStayPartial(t *testing.T) {
	t.Run("rolling-day-retention", func(t *testing.T) {
		s, _ := newTestServer(t)
		ctx := context.Background()
		if err := s.usageLedger.SetRetention(ctx, usageledger.Retention{MaxRecords: 1000, Days: 1}); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		old, kept := int64(999), int64(9)
		appendOverviewRecord(t, s, "chosen", 0, now.AddDate(0, 0, -2), &old)
		appendOverviewRecord(t, s, "chosen", 0, now, &kept)
		flushConsoleLedger(t, s)
		out, err := s.usageOverviewAccountTokens(ctx, "chosen", 0, 7, now)
		if err != nil || !out.Partial || len(out.Daily) != 7 || out.Usage.Today.Tokens == nil || *out.Usage.Today.Tokens != 9 {
			t.Fatal("retention-limited history claimed a complete range", out, err)
		}
		for _, day := range out.Daily[:6] {
			if day.Tokens != nil || day.Requests != 0 {
				t.Fatal("expired or cutoff-straddling day became a known zero", day)
			}
		}
	})
	t.Run("record-limit-without-dropped-counter", func(t *testing.T) {
		s, _ := newTestServer(t)
		ctx := context.Background()
		if err := s.usageLedger.SetRetention(ctx, usageledger.Retention{MaxRecords: 1, Days: 30}); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		old, kept := int64(999), int64(5)
		appendOverviewRecord(t, s, "chosen", 0, now.AddDate(0, 0, -1), &old)
		appendOverviewRecord(t, s, "chosen", 0, now, &kept)
		flushConsoleLedger(t, s)
		out, err := s.usageOverviewAccountTokens(ctx, "chosen", 0, 7, now)
		if err != nil || !out.Partial || out.Usage.Today.Tokens == nil || *out.Usage.Today.Tokens != 5 {
			t.Fatal("record-capped history claimed completeness", out, err)
		}
		history, err := s.usageLedger.Query(ctx, usageledger.QueryFilter{})
		if err != nil || history.Total != 1 || history.Status.Dropped != 0 {
			t.Fatal("fixture did not exercise silent retention pruning", history, err)
		}
	})
	t.Run("nonterminal-token-observation", func(t *testing.T) {
		s, _ := newTestServer(t)
		ctx := context.Background()
		now, total := time.Now(), int64(4)
		appendOverviewRecord(t, s, "chosen", 0, now, &total)
		flushConsoleLedger(t, s)
		out, err := s.usageOverviewAccountTokens(ctx, "chosen", 0, 7, now)
		if err != nil || out.Partial {
			t.Fatal("complete retained history became partial", out, err)
		}
		if !s.usageLedger.Append(usageledger.Observation{StartedAt: now, AccountID: "chosen", Model: "synthetic-model", State: "incomplete", Tokens: usageledger.Tokens{Total: &total}}) {
			t.Fatal("synthetic nonterminal record was not accepted")
		}
		flushConsoleLedger(t, s)
		out, err = s.usageOverviewAccountTokens(ctx, "chosen", 0, 7, now)
		if err != nil || !out.Partial || out.Usage.Today.Tokens == nil || *out.Usage.Today.Tokens != 8 || out.Usage.Today.Requests != 2 {
			t.Fatal("nonterminal observation became complete or lost observed Tokens", out, err)
		}
	})
}

func TestUsageOverviewSelectedOfficialVaultOnlyAndReplacementAreLocalOnly(t *testing.T) {
	noNativeUsageForSelection(t)
	var probes atomic.Int32
	s, h := officialConsoleWithUsage(t, codexaccounts.UsageFetchFunc(func(context.Context, *coreauth.Auth) (codexaccounts.UsageFetchResult, error) {
		probes.Add(1)
		return codexaccounts.UsageFetchResult{}, nil
	}))
	prepared, err := codexaccounts.ParseImport([]byte(officialFixture))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.d.CodexAccounts.Import(context.Background(), prepared, "")
	if err != nil {
		t.Fatal(err)
	}
	row, id := rows[0], "official-"+rows[0].ID
	known, old := int64(10), int64(1000)
	appendOverviewRecord(t, s, id, row.IdentityEpoch, time.Now(), &known)
	appendOverviewRecord(t, s, id, 0, time.Now(), &old)
	appendOverviewRecord(t, s, row.ID, row.IdentityEpoch, time.Now(), &old)
	flushConsoleLedger(t, s)
	w := selectedUsageRequest(s, h, id)
	out := selectedUsageResult(t, w)
	if w.Code != 200 || len(out.Options) != 1 || len(out.Sources) != 1 || out.Sources[0].Kind != "official" || out.Sources[0].LocalUsage == nil || *out.Sources[0].LocalUsage.Usage.Today.Tokens != 10 || len(s.d.Store.Get().LocalAccounts) != 0 || probes.Load() != 0 {
		t.Fatal("vault-only Token query attached/probed or borrowed another identity", w.Code, w.Body)
	}
	assertNoOfficialSecrets(t, w.Body.String())
	if err := s.attachOfficialRows(rows); err != nil {
		t.Fatal(err)
	}
	captured := s.d.Store.Get()
	target := s.usageOverviewSelections(captured)[0]
	if !s.usageOverviewSelectionCurrent(captured, captured, target) {
		t.Fatal("current official fixture rejected")
	}
	replacement, err := codexaccounts.ParseImport([]byte(strings.ReplaceAll(officialFixture, "synthetic-access-private", "synthetic-replacement-private")))
	if err != nil {
		t.Fatal(err)
	}
	rows, err = s.d.CodexAccounts.Import(context.Background(), replacement, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s.usageOverviewSelectionCurrent(captured, s.d.Store.Get(), target) {
		t.Fatal("a vault identity replacement survived the final fence before config synchronization")
	}
	w = selectedUsageRequest(s, h, id)
	out = selectedUsageResult(t, w)
	if w.Code != http.StatusConflict || len(out.Options) != 1 || out.Options[0].ID != id || !strings.Contains(out.Error, "身份已变更") {
		t.Fatal("stale attached identity was silently reassigned or duplicated in the menu", w.Code, w.Body)
	}
	if err := s.attachOfficialRows(rows); err != nil {
		t.Fatal(err)
	}
	w = selectedUsageRequest(s, h, id)
	out = selectedUsageResult(t, w)
	if w.Code != 200 || len(out.Options) != 1 || *out.Sources[0].LocalUsage.Usage.Today.Tokens != 0 || out.Sources[0].LocalUsage.Usage.Today.Requests != 0 || probes.Load() != 0 {
		t.Fatal("new identity inherited old Token history", w.Code, w.Body)
	}
	history, err := s.usageLedger.Query(context.Background(), usageledger.QueryFilter{AccountID: s.usageLedger.AnonymousAccountID(id)})
	if err != nil || history.Total != 2 {
		t.Fatal("identity filtering modified historical records", err, history.Total)
	}
}

func TestUsageOverviewSelectedAndOverviewQueriesDoNotRetireEachOther(t *testing.T) {
	localUsageOnce.Do(func() {})
	localUsage = &localusage.Scanner{ClaudeHome: t.TempDir(), CodexHome: t.TempDir()}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		_, _ = w.Write([]byte(`{"remaining":12}`))
	}))
	t.Cleanup(provider.Close)
	s, h := newTestServer(t)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.LocalAccounts = []config.LocalAccount{{ID: "first", BaseURL: provider.URL, Key: "synthetic-key"}, {ID: "duplicate", BaseURL: provider.URL, Key: "synthetic-key"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- selectedUsageRequest(s, h, "duplicate") }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("selected provider read did not start")
	}
	if w := selectedUsageRequest(s, h, "first"); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	if w := localRequest(s, h, "GET", "/api/usage/overview?days=7", nil); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	once.Do(func() { close(release) })
	select {
	case w := <-done:
		if w.Code != 200 || selectedUsageResult(t, w).SelectedSource != "duplicate" {
			t.Fatal("independent source or ball query retired selected request", w.Code, w.Body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("selected request did not finish")
	}
}

func TestUsageOverviewSelectedDuplicateDeletionABACannotPublishRetiredResponse(t *testing.T) {
	noNativeUsageForSelection(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_, _ = w.Write([]byte(`{"remaining":12}`))
	}))
	t.Cleanup(provider.Close)
	s, h := newTestServer(t)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.LocalAccounts = []config.LocalAccount{{ID: "first", BaseURL: provider.URL, Key: "synthetic-key"}, {ID: "duplicate", BaseURL: provider.URL, Key: "synthetic-key"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	initial := s.d.Store.Get()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- selectedUsageRequest(s, h, "duplicate") }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("selected provider read did not start")
	}
	if err := s.d.Store.Update(func(c *config.Config) error { c.LocalAccounts = c.LocalAccounts[:1]; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.d.Store.Update(func(c *config.Config) error { c.LocalAccounts = initial.LocalAccounts; return nil }); err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	select {
	case w := <-done:
		out := selectedUsageResult(t, w)
		if w.Code != http.StatusConflict || len(out.Sources) != 0 || len(out.Options) != 2 || out.SelectedSource != "duplicate" {
			t.Fatal("removed/recreated duplicate reused retired provider result", w.Code, w.Body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("selected request did not finish")
	}
}
