package usageledger

import (
	"context"
	"testing"
)

func TestAccountCostsFenceIdentityAndReturnDetachedRecords(t *testing.T) {
	l := testLedger(t)
	for _, epoch := range []uint64{0, 1, 2} {
		o := observedFixture()
		o.AccountEpoch = epoch
		input := int64(42)
		o.Tokens.Input = &input
		if !l.Append(o) {
			t.Fatal("append")
		}
	}
	other := observedFixture()
	other.AccountID = "another-source"
	other.AccountEpoch = 2
	l.Append(other)
	flushLedger(t, l)
	scope := AccountCostScope{"raw-fixture-account", 2}
	got := l.AccountCosts([]AccountCostScope{scope})[scope]
	if len(got.Records) != 1 || got.Incomplete || got.Records[0].AccountEpoch != 2 {
		t.Fatalf("crossed identities %+v", got)
	}
	*got.Records[0].Tokens.Input = 999
	again := l.AccountCosts([]AccountCostScope{scope})[scope]
	if *again.Records[0].Tokens.Input != 42 {
		t.Fatal("shared pointer")
	}
	if len(l.AccountCosts([]AccountCostScope{{scope.ID, 0}})) != 0 {
		t.Fatal("legacy unknown identity included")
	}
}

func TestAccountCostsRetentionAndDroppedAreNotComplete(t *testing.T) {
	l := testLedger(t)
	o := observedFixture()
	o.AccountEpoch = 3
	l.Append(o)
	flushLedger(t, l)
	l.mu.Lock()
	l.dropped = 1
	l.mu.Unlock()
	scope := AccountCostScope{o.AccountID, 3}
	if !l.AccountCosts([]AccountCostScope{scope})[scope].Incomplete {
		t.Fatal("lost dropped flag")
	}
}

func TestAccountCostsActiveStreamsAndClearInvalidateCalibration(t *testing.T) {
	l := testLedger(t)
	scope := AccountCostScope{"fixture", 3}
	before := l.AccountCosts([]AccountCostScope{scope})[scope]
	finish := l.BeginAccountUsage(scope.ID, scope.Epoch)
	active := l.AccountCosts([]AccountCostScope{scope})[scope]
	if !active.Incomplete || active.LatestActivity.IsZero() {
		t.Fatal("in-flight stream not fenced")
	}
	finish()
	finish() // idempotent release
	after := l.AccountCosts([]AccountCostScope{scope})[scope]
	if after.Incomplete || after.LatestActivity.Before(active.LatestActivity) {
		t.Fatal("release")
	}
	if err := l.Clear(context.Background()); err != nil {
		t.Fatal(err)
	}
	cleared := l.AccountCosts([]AccountCostScope{scope})[scope]
	if cleared.Coverage == before.Coverage {
		t.Fatal("clear reused sample coverage")
	}
}
