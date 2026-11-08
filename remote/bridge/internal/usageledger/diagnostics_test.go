package usageledger

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRequestDiagnosticsPersistBoundedAnonymousAttempts(t *testing.T) {
	l := testLedger(t)
	first := int64(12)
	o := observedFixture()
	o.FirstByteMS, o.RoutingReason = &first, "failover"
	o.Attempts = []Attempt{{AccountID: "private-source-a", AccountEpoch: 2, State: "rejected", HTTPStatus: 429, Reason: "rate_limited", RetryAfterMS: 45000}, {AccountID: o.AccountID, State: "completed", HTTPStatus: 200, Model: o.Model, FirstByteMS: &first}}
	if !l.Append(o) {
		t.Fatal("append failed")
	}
	first = 999
	o.Attempts[0].Reason = "mutated"
	flushLedger(t, l)
	out, err := l.Query(context.Background(), QueryFilter{})
	if err != nil || len(out.Records) != 1 {
		t.Fatal(err)
	}
	r := out.Records[0]
	if len(r.Attempts) != 2 || r.Attempts[0].AccountID != l.AnonymousAccountID("private-source-a") || r.Attempts[0].Reason != "rate_limited" || *r.FirstByteMS != 12 || *r.Attempts[1].FirstByteMS != 12 {
		t.Fatalf("lost diagnostic evidence: %+v", r)
	}
	raw, _ := json.Marshal(r)
	if strings.Contains(string(raw), "private-source-a") || strings.Contains(string(raw), o.AccountID) {
		t.Fatal("raw account IDs leaked")
	}
	if l.Close(context.Background()) != nil {
		t.Fatal("close failed")
	}
	reopened := Open(l.path)
	defer reopened.Close(context.Background())
	s := ledgerSnapshot(t, reopened)
	if s.ReadError != "" || len(s.Records) != 1 || len(s.Records[0].Attempts) != 2 {
		t.Fatalf("reopen lost diagnostics: %+v", s)
	}
	*s.Records[0].FirstByteMS = 90
	s.Records[0].Attempts[0].Reason = "mutation"
	*s.Records[0].Attempts[1].FirstByteMS = 90
	clone := ledgerSnapshot(t, reopened).Records[0]
	if *clone.FirstByteMS != 12 || clone.Attempts[0].Reason != "rate_limited" || *clone.Attempts[1].FirstByteMS != 12 {
		t.Fatal("snapshot aliases stored diagnostics")
	}
}

func TestRequestDiagnosticsRejectFreeTextAndKeepLegacyUnknown(t *testing.T) {
	l := testLedger(t)
	o := observedFixture()
	bad := int64(-1)
	o.RoutingReason, o.FailureStage, o.FirstByteMS = "sk-private", "https://private.invalid", &bad
	for i := 0; i < 8; i++ {
		o.Attempts = append(o.Attempts, Attempt{AccountID: "raw", Model: "sk-private", State: "raw error", HTTPStatus: 900, Reason: "Bearer private", FirstByteMS: &bad})
	}
	l.Append(o)
	l.Append(observedFixture())
	flushLedger(t, l)
	rows := ledgerSnapshot(t, l).Records
	var diagnostic, legacy Record
	for _, row := range rows {
		if len(row.Attempts) > 0 {
			diagnostic = row
		} else {
			legacy = row
		}
	}
	if len(diagnostic.Attempts) != MaxAttempts || diagnostic.FirstByteMS != nil || diagnostic.RoutingReason != "" || diagnostic.FailureStage != "" {
		t.Fatal("invalid metadata survived")
	}
	if legacy.FirstByteMS != nil || legacy.RoutingReason != "" || legacy.Attempts != nil {
		t.Fatal("invented diagnostics for legacy record")
	}
	for _, a := range diagnostic.Attempts {
		if a.Model != "" || a.State != "unknown" || a.HTTPStatus != 0 || a.Reason != "" || a.FirstByteMS != nil {
			t.Fatal("raw error survived")
		}
	}
}

func TestRequestDiagnosticsCannotBypassRetentionOrClientScope(t *testing.T) {
	l := testLedger(t)
	if err := l.SetRetention(context.Background(), Retention{MaxRecords: 2, Days: 1}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		o := observedFixture()
		o.Attempts = []Attempt{{AccountID: o.AccountID, State: "completed", HTTPStatus: 200}}
		l.Append(o)
	}
	o := observedFixture()
	o.ClientID = ""
	l.Append(o)
	flushLedger(t, l)
	out, err := l.Query(context.Background(), QueryFilter{ClientRequestsOnly: true})
	if err != nil || out.Total != 1 || len(out.Records) != 1 {
		t.Fatalf("scope/retention: %+v %v", out, err)
	}
	if len(out.Records[0].Attempts) != 1 {
		t.Fatal("retained record lost attempts")
	}
}

func TestRequestDiagnosticsShareOriginalDiskBudget(t *testing.T) {
	model := strings.Repeat("m", 200)
	row := Record{ID: strings.Repeat("a", 32), Time: time.Now().UnixMilli(), AccountID: "acct-" + strings.Repeat("b", 24), RouteID: "route-" + strings.Repeat("c", 24), Model: model, State: "completed", HTTPStatus: 200}
	for i := 0; i < MaxAttempts; i++ {
		row.Attempts = append(row.Attempts, Attempt{AccountID: row.AccountID, Model: model, State: "failed", HTTPStatus: 429, Reason: "rate_limited", DurationMS: 999999, RetryAfterMS: 999999})
	}
	// Large valid diagnostic records must still fit the pre-existing row bound.
	raw, _ := json.Marshal(row)
	if len(raw) > 4096 {
		t.Fatal("diagnostic record exceeds SQLite read bound")
	}
	rows := make([]Record, MaxRecords)
	for i := range rows {
		rows[i] = row
	}
	kept := prune(rows, Retention{MaxRecords: MaxRecords, Days: 30}, time.Now())
	encoded, _ := json.Marshal(kept)
	if len(encoded) > maxFileBytes-(2<<20)+2 {
		t.Fatal("diagnostics exceeded record storage budget")
	}
	if len(kept) == 0 {
		t.Fatal("budget discarded all records")
	}
}
