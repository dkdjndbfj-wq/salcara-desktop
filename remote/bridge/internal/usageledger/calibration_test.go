package usageledger

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func calibrationFixture(l *Ledger) CostCalibration {
	return CostCalibration{Epoch: 2, Plan: "pro", Reset: time.Now().Add(time.Hour).UTC(), Seconds: 7200, At: time.Now().Add(-time.Minute).UTC(), Used: 10, Coverage: l.costCoverage, ScopeHash: strings.Repeat("a", 64), PriceKey: priceKey(l.prices), Total: 100, LearnedAt: time.Now().UTC()}
}

func TestCostCalibrationPersistsPseudonymizedAndFencesRevisions(t *testing.T) {
	l := testLedger(t)
	v := calibrationFixture(l)
	if !l.QueueCostCalibration("private-vault-id", v) {
		t.Fatal("queue")
	}
	flushLedger(t, l)
	if got, ok := l.CostCalibration("private-vault-id"); !ok || got != v {
		t.Fatal(got, ok)
	}
	raw, _ := json.Marshal(l.state())
	if strings.Contains(string(raw), "private-vault-id") {
		t.Fatal("raw identity persisted")
	}
	if err := l.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	l = Open(l.path)
	defer l.Close(context.Background())
	if got, ok := l.CostCalibration("private-vault-id"); !ok || got != v {
		t.Fatal("restart lost learned sample", got, ok)
	}
	if err := l.ReplacePricing(context.Background(), 1, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := l.CostCalibration("private-vault-id"); ok {
		t.Fatal("price edit retained sample")
	}
	if l.QueueCostCalibration("private-vault-id", v) {
		t.Fatal("stale update accepted")
	}
}

func TestCostCalibrationLateIdentityAndClearCannotRevive(t *testing.T) {
	l := testLedger(t)
	v := calibrationFixture(l)
	l.QueueCostCalibration("one", v)
	flushLedger(t, l)
	older := v
	older.Epoch--
	older.At = older.At.Add(time.Second)
	l.QueueCostCalibration("one", older)
	flushLedger(t, l)
	got, ok := l.CostCalibration("one")
	if !ok || got != v {
		t.Fatal("old identity replaced new", got)
	}
	if err := l.Clear(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := l.CostCalibration("one"); ok || l.QueueCostCalibration("one", v) {
		t.Fatal("clear resurrected calibration")
	}
}

func TestCostCalibrationUncleanRestartDiscardsOnlyLearning(t *testing.T) {
	l := testLedger(t)
	v := calibrationFixture(l)
	l.QueueCostCalibration("one", v)
	l.Append(observedFixture())
	flushLedger(t, l)
	// Inspect the flushed metadata as if the process had crashed before Close.
	state := l.state()
	if state.CleanShutdown {
		t.Fatal("flush claimed clean shutdown")
	}
	if err := l.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	storage := &sqliteStorage{path: l.path}
	if _, err := storage.load(); err != nil {
		t.Fatal(err)
	}
	if err := storage.write(state); err != nil {
		t.Fatal(err)
	}
	storage.close()
	resumed := Open(l.path)
	defer resumed.Close(context.Background())
	if _, ok := resumed.CostCalibration("one"); ok {
		t.Fatal("unclean restart reused estimated budget")
	}
	snapshot, err := resumed.Snapshot(10, "")
	if err != nil || len(snapshot.Records) != 1 {
		t.Fatal("ordinary usage lost", snapshot, err)
	}
}
