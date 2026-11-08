package accounthealth

import "testing"

func TestRecordForDoesNotRebindLateCredential(t *testing.T) {
	h := NewTracker()
	h.Bind("account", "new")
	h.RecordFor("account", "old", Result{Kind: RateLimit, Status: 429})
	if !h.Available("account") {
		t.Fatal("late old credential cooled current account")
	}
	h.RecordFor("account", "new", Result{Kind: RateLimit, Status: 429})
	if h.Available("account") {
		t.Fatal("current credential outcome not recorded")
	}
	h.Bind("account", "replacement")
	if !h.Available("account") {
		t.Fatal("replacement inherited previous cooldown")
	}
}
