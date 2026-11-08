package usageledger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func testLedger(t *testing.T) *Ledger {
	t.Helper()
	l := Open(filepath.Join(t.TempDir(), "ledger.json"))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = l.Close(ctx)
	})
	return l
}
func flushLedger(t *testing.T, l *Ledger) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := l.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}
func ledgerSnapshot(t *testing.T, l *Ledger) Snapshot {
	t.Helper()
	out, err := l.Snapshot(200, "")
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func observedFixture() Observation {
	return Observation{StartedAt: time.Now(), AccountID: "raw-fixture-account", ClientID: "raw-fixture-client", Route: "codex/v1/responses", Model: "gpt-fixture", State: "completed", HTTPStatus: 200, UpstreamHTTPStatus: 200, DurationMS: 15}
}
func setLedgerWriter(l *Ledger, write func(diskState) error) {
	l.mu.Lock()
	l.write = write
	l.mu.Unlock()
}

func TestLedgerPersistsAnonymousRecordsWithoutManufacturingZeroUsage(t *testing.T) {
	l := testLedger(t)
	o := observedFixture()
	input := int64(3)
	o.Tokens.Input = &input
	if !l.Append(o) {
		t.Fatal("append rejected")
	}
	input = 999
	flushLedger(t, l)
	before := ledgerSnapshot(t, l)
	if before.Count != 1 || before.Unsaved || before.Records[0].Tokens.Input == nil || *before.Records[0].Tokens.Input != 3 || before.Records[0].Tokens.Output != nil || before.Records[0].Tokens.Total != nil {
		t.Fatalf("usage truth or save status lost: %+v", before)
	}
	row := before.Records[0]
	if row.AccountID == o.AccountID || row.ClientID == o.ClientID || row.RouteID == o.Route {
		t.Fatal("raw local identifiers were persisted")
	}
	raw, err := os.ReadFile(l.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{o.AccountID, o.ClientID, o.Route, "prompt", "body", "apiKey"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("private request data persisted: %q", secret)
		}
	}
	*before.Records[0].Tokens.Input = 1234
	if *ledgerSnapshot(t, l).Records[0].Tokens.Input != 3 {
		t.Fatal("snapshot usage pointer aliases live storage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if l.Close(ctx) != nil {
		t.Fatal("close failed")
	}
	reopened := Open(l.path)
	defer reopened.Close(ctx)
	reopened.Append(observedFixture())
	flushLedger(t, reopened)
	rows := ledgerSnapshot(t, reopened).Records
	if len(rows) != 2 || rows[0].AccountID != rows[1].AccountID || rows[0].ClientID != rows[1].ClientID || rows[0].RouteID != rows[1].RouteID || rows[0].ID == rows[1].ID {
		t.Fatal("anonymous IDs are unstable or requests share a record ID")
	}
}

func TestLedgerRetentionPagingAndModelSanitization(t *testing.T) {
	l := testLedger(t)
	ctx := context.Background()
	if err := l.SetRetention(ctx, Retention{MaxRecords: 2, Days: 1}); err != nil {
		t.Fatal(err)
	}
	old := observedFixture()
	old.StartedAt = time.Now().Add(-48 * time.Hour)
	l.Append(old)
	for _, model := range []string{"gpt-one", "https://secret.example/token", "sk-secret-fixture"} {
		o := observedFixture()
		o.Model = model
		l.Append(o)
	}
	flushLedger(t, l)
	out := ledgerSnapshot(t, l)
	if out.Count != 2 || out.Records[0].Model != "" || out.Records[1].Model != "" {
		t.Fatalf("retention/model privacy failed: %+v", out)
	}
	page, err := l.Snapshot(1, "")
	if err != nil || len(page.Records) != 1 {
		t.Fatal("first page failed")
	}
	older, err := l.Snapshot(1, page.Records[0].ID)
	if err != nil || len(older.Records) != 1 || older.Records[0].ID == page.Records[0].ID {
		t.Fatal("cursor repeated a request")
	}
	if _, err = l.Snapshot(1, "not-a-real-cursor"); err == nil {
		t.Fatal("invalid cursor accepted")
	}
	if l.SetRetention(ctx, Retention{MaxRecords: MaxRecords + 1, Days: 1}) == nil {
		t.Fatal("unbounded retention accepted")
	}
	if l.Clear(ctx) != nil || ledgerSnapshot(t, l).Count != 0 {
		t.Fatal("clear failed")
	}
	flushLedger(t, l)
}

func TestSaveFailuresKeepUnsavedRecordsAndFailedMutationsAreAtomic(t *testing.T) {
	l := testLedger(t)
	beforeBytes, _ := os.ReadFile(l.path)
	beforePolicy := ledgerSnapshot(t, l).Retention
	setLedgerWriter(l, func(diskState) error { return errors.New("private path / synthetic-key") })
	l.Append(observedFixture())
	if l.Flush(context.Background()) == nil {
		t.Fatal("save failure hidden")
	}
	out := ledgerSnapshot(t, l)
	if out.Count != 1 || !out.Unsaved || out.SaveError == "" || strings.Contains(out.SaveError, "synthetic-key") {
		t.Fatalf("save failure metadata unsafe: %+v", out)
	}
	if l.Clear(context.Background()) == nil || ledgerSnapshot(t, l).Count != 1 {
		t.Fatal("failed clear deleted live records")
	}
	if l.SetRetention(context.Background(), Retention{MaxRecords: 1, Days: 1}) == nil || ledgerSnapshot(t, l).Retention != beforePolicy {
		t.Fatal("failed policy save changed retention")
	}
	afterBytes, _ := os.ReadFile(l.path)
	if !reflect.DeepEqual(beforeBytes, afterBytes) {
		t.Fatal("failed writes replaced old durable data")
	}
	setLedgerWriter(l, l.storage.write)
	flushLedger(t, l)
	out = ledgerSnapshot(t, l)
	if out.SaveError != "" || out.Unsaved || out.Count != 1 {
		t.Fatal("retry did not save pending records")
	}
}

func TestUnreadableLedgerRequiresExplicitClearBeforeReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	broken := []byte(`{"version":1,"Version":1}`)
	if err := os.WriteFile(path, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	l := Open(path)
	defer l.Close(context.Background())
	if ledgerSnapshot(t, l).ReadError == "" {
		t.Fatal("ambiguous file accepted")
	}
	l.Append(observedFixture())
	if l.Flush(context.Background()) == nil || l.SetRetention(context.Background(), Retention{MaxRecords: 5, Days: 1}) == nil {
		t.Fatal("unreadable file overwritten without clear")
	}
	raw, _ := os.ReadFile(path)
	if !reflect.DeepEqual(raw, broken) {
		t.Fatal("unreadable file was replaced")
	}
	if l.Clear(context.Background()) != nil {
		t.Fatal("explicit recovery clear failed")
	}
	if out := ledgerSnapshot(t, l); out.Count != 0 || out.ReadError != "" || out.SaveError != "" {
		t.Fatal("clear did not reset recovery state")
	}
}

func TestBlockedDiskDoesNotBlockAppendAndOverflowIsObservable(t *testing.T) {
	l := testLedger(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	setLedgerWriter(l, func(state diskState) error {
		once.Do(func() { close(entered); <-release })
		return l.storage.write(state)
	})
	l.Append(observedFixture())
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("write did not begin")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < queueCapacity+5; i++ {
			l.Append(observedFixture())
		}
		_ = ledgerSnapshot(t, l)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("blocked disk blocked inference observation")
	}
	if out := ledgerSnapshot(t, l); out.Dropped == 0 || !out.Unsaved || out.Pending > queueCapacity {
		close(release)
		t.Fatalf("queue bound/gaps hidden: %+v", out)
	}
	close(release)
	flushLedger(t, l)
	if out := ledgerSnapshot(t, l); out.Dropped == 0 || out.Unsaved || out.Pending != 0 {
		t.Fatal("durable gap status lost")
	}
}

func TestConcurrentCloseClearAndFlushAlwaysFinish(t *testing.T) {
	for iteration := 0; iteration < 30; iteration++ {
		l := testLedger(t)
		setLedgerWriter(l, func(diskState) error { return nil })
		start := make(chan struct{})
		results := make(chan error, 22)
		for i := 0; i < 20; i++ {
			go func(index int) {
				<-start
				var err error
				switch index % 3 {
				case 0:
					err = l.Clear(context.Background())
				case 1:
					err = l.Flush(context.Background())
				default:
					err = l.SetRetention(context.Background(), Retention{MaxRecords: 20, Days: 2})
				}
				results <- err
			}(i)
		}
		for i := 0; i < 2; i++ {
			go func() { <-start; results <- l.Close(context.Background()) }()
		}
		close(start)
		for i := 0; i < 22; i++ {
			select {
			case err := <-results:
				if err != nil && !errors.Is(err, ErrSave) {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Close stranded an accepted control command")
			}
		}
		if l.Append(observedFixture()) {
			t.Fatal("closed ledger accepted a request")
		}
	}
}

func TestCancelledQueuedCommandsDoNotMutateRecordsOrRetention(t *testing.T) {
	l := testLedger(t)
	oldRetention := ledgerSnapshot(t, l).Retention
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	setLedgerWriter(l, func(state diskState) error {
		once.Do(func() { close(entered); <-release })
		return l.storage.write(state)
	})
	l.Append(observedFixture())
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	results := make(chan error, 2)
	go func() { results <- l.Clear(ctx) }()
	go func() { results <- l.SetRetention(ctx, Retention{MaxRecords: 1, Days: 1}) }()
	for i := 0; i < 2; i++ {
		if err := <-results; !errors.Is(err, context.DeadlineExceeded) {
			close(release)
			t.Fatalf("queued mutation should time out: %v", err)
		}
	}
	close(release)
	flushLedger(t, l)
	if out := ledgerSnapshot(t, l); out.Count != 1 || out.Retention != oldRetention {
		t.Fatalf("cancelled queued command executed: %+v", out)
	}
}

func TestCancellationDuringAtomicWriteHasUncertainOutcome(t *testing.T) {
	l := testLedger(t)
	l.Append(observedFixture())
	flushLedger(t, l)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	setLedgerWriter(l, func(state diskState) error {
		once.Do(func() { close(entered); <-release })
		return l.storage.write(state)
	})
	ctx, cancel := context.WithCancel(context.Background())
	results := make(chan error, 1)
	go func() { results <- l.Clear(ctx) }()
	<-entered
	cancel()
	if err := <-results; !errors.Is(err, context.Canceled) {
		close(release)
		t.Fatalf("mutation did not expose uncertain cancellation: %v", err)
	}
	if ledgerSnapshot(t, l).Count != 1 {
		close(release)
		t.Fatal("uncommitted candidate replaced records")
	}
	close(release)
	flushLedger(t, l)
	if ledgerSnapshot(t, l).Count != 0 {
		t.Fatal("successful atomic write was not applied")
	}
}
