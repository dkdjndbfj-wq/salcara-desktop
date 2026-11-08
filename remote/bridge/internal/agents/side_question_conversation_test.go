package agents

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func persistentSideFixture(t *testing.T) (*sideQuestionRegistry, SideQuestionSnapshot, sideQuestionBinding, string) {
	t.Helper()
	return &sideQuestionRegistry{persistentDir: t.TempDir()}, SideQuestionSnapshot{Revision: newUUID(), Messages: []SideQuestionMessage{{ID: "message-1", Role: "user", Text: "parent context"}}}, sideQuestionBinding{Source: "live-source-1", Persistent: "account-and-route", Runtime: new(int)}, newUUID()
}

func TestPersistentSideQuestionSourceSurvivesPreferenceButNotAccountReplacement(t *testing.T) {
	account := config.LocalAccount{ID: "same-slot", BaseURL: "https://fixture.invalid", Key: "fixture-a", SourceType: "api_key"}
	first := sideConversationSource(account, "remote-codex")
	account.Model, account.Name = "changed-model", "renamed"
	if sideConversationSource(account, "remote-codex") != first {
		t.Fatal("display preference orphaned history")
	}
	account.Key = "fixture-b"
	if sideConversationSource(account, "remote-codex") == first {
		t.Fatal("replacement account inherited old context")
	}
	account.CredentialRef, account.CredentialEpoch = "protected-identity", 1
	first = sideConversationSource(account, "remote-codex")
	account.Key = "refreshed-protected-token-marker"
	if sideConversationSource(account, "remote-codex") != first {
		t.Fatal("protected refresh orphaned history")
	}
	account.CredentialEpoch++
	if sideConversationSource(account, "remote-codex") == first {
		t.Fatal("protected identity replacement inherited old context")
	}
}

func TestPersistentSideQuestionSurvivesRestartTimeAndSixRounds(t *testing.T) {
	r, snapshot, binding, chain := persistentSideFixture(t)
	guard := func() error { return nil }
	view, err := r.conversation(context.Background(), "codex:parent", "phone-owner", snapshot, protocol.SideQuestionConversationRequest{Action: "open", ChainID: chain}, binding, guard, nil, nil)
	if err != nil || !view.Available {
		t.Fatalf("open: %+v %v", view, err)
	}
	requests := 0
	transport := func(_ context.Context, input sideQuestionInput) (sideQuestionAnswer, bool, error) {
		requests++
		if input.Snapshot.Messages[0].Text != "parent context" {
			t.Fatal("parent changed")
		}
		return sideQuestionAnswer{Text: "answer"}, true, nil
	}
	for i := 0; i < 9; i++ {
		view, err = r.conversation(context.Background(), "codex:parent", "phone-owner", SideQuestionSnapshot{}, protocol.SideQuestionConversationRequest{Action: "send", ChainID: chain, SideQuestionRequest: protocol.SideQuestionRequest{OperationID: newUUID(), ExpectedRevision: view.Revision, PreviousOperationID: view.LastOperationID, Question: "follow up"}}, binding, guard, transport, nil)
		if err != nil || len(view.Turns) != i+1 || !view.Available {
			t.Fatalf("turn %d: %+v %v", i, view, err)
		}
	}
	key := sideConversationStorageKey("phone-owner", "codex:parent", chain)
	r.mu.Lock()
	doc, err := r.loadConversationLocked(key)
	if err != nil {
		t.Fatal(err)
	}
	doc.Turns[0].CreatedAt = time.Now().Add(-365 * 24 * time.Hour).UnixMilli()
	if err := r.saveConversationLocked(key, doc); err != nil {
		t.Fatal(err)
	}
	r.mu.Unlock()
	restarted := &sideQuestionRegistry{persistentDir: r.persistentDir}
	binding.Source, binding.Runtime = "fresh-authorized-source", new(int)
	view, err = restarted.conversation(context.Background(), "codex:parent", "phone-owner", SideQuestionSnapshot{}, protocol.SideQuestionConversationRequest{Action: "read", ChainID: chain}, binding, guard, nil, nil)
	if err != nil || len(view.Turns) != 9 || !view.Available || requests != 9 || view.Turns[0].Answer != "answer" {
		t.Fatalf("restore: %+v %v", view, err)
	}
	if time.Since(time.UnixMilli(view.Turns[0].CreatedAt)) < 300*24*time.Hour {
		t.Fatal("old history was replaced")
	}
}

func TestPersistentSideQuestionUnknownDoesNotReplayAndOwnerCannotCross(t *testing.T) {
	r, snapshot, binding, chain := persistentSideFixture(t)
	guard := func() error { return nil }
	view, err := r.conversation(context.Background(), "codex:parent", "phone-a", snapshot, protocol.SideQuestionConversationRequest{Action: "open", ChainID: chain}, binding, guard, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := protocol.SideQuestionConversationRequest{Action: "send", ChainID: chain, SideQuestionRequest: protocol.SideQuestionRequest{OperationID: newUUID(), ExpectedRevision: view.Revision, Question: "one billed request"}}
	calls := 0
	transport := func(context.Context, sideQuestionInput) (sideQuestionAnswer, bool, error) {
		calls++
		return sideQuestionAnswer{}, true, errors.New("unknown private provider detail")
	}
	view, err = r.conversation(context.Background(), "codex:parent", "phone-a", snapshot, request, binding, guard, transport, nil)
	if err != nil || view.Available || view.Turns[0].State != "unconfirmed" || strings.Contains(view.Turns[0].Error, "private") {
		t.Fatalf("unknown: %+v %v", view, err)
	}
	restarted := &sideQuestionRegistry{persistentDir: r.persistentDir}
	view, err = restarted.conversation(context.Background(), "codex:parent", "phone-a", snapshot, request, binding, guard, transport, nil)
	if err != nil || calls != 1 || view.Turns[0].State != "unconfirmed" {
		t.Fatalf("replay: %d %+v %v", calls, view, err)
	}
	if _, err := restarted.conversation(context.Background(), "codex:parent", "phone-b", snapshot, protocol.SideQuestionConversationRequest{Action: "read", ChainID: chain}, binding, guard, nil, nil); err == nil {
		t.Fatal("different phone read history")
	}
	binding.Persistent = "another-account"
	if _, err := restarted.conversation(context.Background(), "codex:parent", "phone-a", snapshot, protocol.SideQuestionConversationRequest{Action: "read", ChainID: chain}, binding, guard, nil, nil); err == nil {
		t.Fatal("different account read history")
	}
	deleted, err := restarted.conversation(context.Background(), "codex:parent", "phone-a", SideQuestionSnapshot{}, protocol.SideQuestionConversationRequest{Action: "delete", ChainID: chain, SideQuestionRequest: protocol.SideQuestionRequest{OperationID: newUUID()}}, binding, guard, nil, nil)
	if err != nil || !deleted.Deleted || len(deleted.Turns) != 0 || deleted.ContextMessages != 0 {
		t.Fatalf("owner could not clear old account history: %+v %v", deleted, err)
	}
}

func TestPersistentSideQuestionStorageFailureAfterDispatchNeverClaimsUnsent(t *testing.T) {
	r, snapshot, binding, chain := persistentSideFixture(t)
	guard := func() error { return nil }
	view, err := r.conversation(context.Background(), "codex:parent", "phone", snapshot, protocol.SideQuestionConversationRequest{Action: "open", ChainID: chain}, binding, guard, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(r.persistentDir, sideConversationStorageKey("phone", "codex:parent", chain)+".json")
	held := path + ".held"
	requests := 0
	request := protocol.SideQuestionConversationRequest{Action: "send", ChainID: chain, SideQuestionRequest: protocol.SideQuestionRequest{OperationID: newUUID(), ExpectedRevision: view.Revision, Question: "bill once"}}
	transport := func(context.Context, sideQuestionInput) (sideQuestionAnswer, bool, error) {
		requests++
		if err := os.Rename(path, held); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		return sideQuestionAnswer{Text: "dispatched answer"}, true, nil
	}
	_, err = r.conversation(context.Background(), "codex:parent", "phone", snapshot, request, binding, guard, transport, nil)
	if err == nil || strings.Contains(err.Error(), "未发送") || requests != 1 {
		t.Fatalf("misleading dispatched storage error: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(held, path); err != nil {
		t.Fatal(err)
	}
	restarted := &sideQuestionRegistry{persistentDir: r.persistentDir}
	view, err = restarted.conversation(context.Background(), "codex:parent", "phone", snapshot, request, binding, guard, transport, nil)
	if err != nil || requests != 1 || len(view.Turns) != 1 || view.Turns[0].State != "unconfirmed" {
		t.Fatalf("failed write replayed dispatch: %+v %v", view, err)
	}
}

func TestPersistentSideQuestionDeleteWhilePendingNeverResurrects(t *testing.T) {
	r, snapshot, binding, chain := persistentSideFixture(t)
	guard := func() error { return nil }
	view, err := r.conversation(context.Background(), "codex:parent", "phone", snapshot, protocol.SideQuestionConversationRequest{Action: "open", ChainID: chain}, binding, guard, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	entered, release, result := make(chan struct{}), make(chan struct{}), make(chan protocol.SideQuestionConversation, 1)
	go func() {
		answer, _ := r.conversation(context.Background(), "codex:parent", "phone", snapshot, protocol.SideQuestionConversationRequest{Action: "send", ChainID: chain, SideQuestionRequest: protocol.SideQuestionRequest{OperationID: newUUID(), ExpectedRevision: view.Revision, Question: "pending"}}, binding, guard, func(context.Context, sideQuestionInput) (sideQuestionAnswer, bool, error) {
			close(entered)
			<-release
			return sideQuestionAnswer{Text: "late answer"}, true, nil
		}, nil)
		result <- answer
	}()
	<-entered
	if !r.activeTurns() {
		t.Fatal("API switch not blocked during send")
	}
	deleted, err := r.conversation(context.Background(), "codex:parent", "phone", snapshot, protocol.SideQuestionConversationRequest{Action: "delete", ChainID: chain, SideQuestionRequest: protocol.SideQuestionRequest{OperationID: newUUID()}}, binding, guard, nil, nil)
	if err != nil || !deleted.Deleted {
		t.Fatalf("delete: %+v %v", deleted, err)
	}
	close(release)
	if late := <-result; !late.Deleted || len(late.Turns) != 0 {
		t.Fatalf("resurrected %+v", late)
	}
	restored, err := r.conversation(context.Background(), "codex:parent", "phone", snapshot, protocol.SideQuestionConversationRequest{Action: "open", ChainID: chain}, binding, guard, nil, nil)
	if err != nil || !restored.Deleted || len(restored.Turns) != 0 || r.activeTurns() {
		t.Fatalf("tombstone lost: %+v %v", restored, err)
	}
}

func TestPersistentSideQuestionPendingRecoveryAndDiskFailure(t *testing.T) {
	r, snapshot, binding, chain := persistentSideFixture(t)
	key := sideConversationStorageKey("phone", "codex:parent", chain)
	doc := &sideConversationDocument{Version: 1, Owner: "phone", Key: "codex:parent", ChainID: chain, Source: binding.Persistent, Snapshot: snapshot, Turns: []sideConversationTurn{{SideQuestionExchange: protocol.SideQuestionExchange{OperationID: newUUID(), Question: "interrupted", State: "pending", CreatedAt: time.Now().UnixMilli()}, Hash: "hash"}}}
	if err := r.saveConversationLocked(key, doc); err != nil {
		t.Fatal(err)
	}
	view, err := r.conversation(context.Background(), doc.Key, doc.Owner, snapshot, protocol.SideQuestionConversationRequest{Action: "read", ChainID: chain}, binding, func() error { return nil }, nil, nil)
	if err != nil || view.Turns[0].State != "unconfirmed" || view.Available {
		t.Fatalf("pending recovery %+v %v", view, err)
	}
	broken := filepath.Join(t.TempDir(), "file-not-directory")
	if err := os.WriteFile(broken, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	r = &sideQuestionRegistry{persistentDir: broken}
	if _, err := r.conversation(context.Background(), doc.Key, doc.Owner, snapshot, protocol.SideQuestionConversationRequest{Action: "open", ChainID: newUUID()}, binding, func() error { return nil }, nil, nil); err == nil {
		t.Fatal("failed persistence reported success")
	}
}

func TestPersistentSideQuestionWindowDoesNotDeleteHistory(t *testing.T) {
	doc := &sideConversationDocument{Snapshot: SideQuestionSnapshot{Revision: newUUID(), Messages: []SideQuestionMessage{{ID: "parent", Role: "user", Text: strings.Repeat("p", 32<<10)}}}}
	for i := 0; i < 60; i++ {
		doc.Turns = append(doc.Turns, sideConversationTurn{SideQuestionExchange: protocol.SideQuestionExchange{OperationID: newUUID(), State: "completed", Question: "q", Answer: strings.Repeat("a", 16<<10)}})
	}
	input, err := persistentSideInput(doc, protocol.SideQuestionConversationRequest{SideQuestionRequest: protocol.SideQuestionRequest{Question: "latest"}})
	if err != nil || !input.Snapshot.Truncated || len(input.Previous) >= 60 || len(doc.Turns) != 60 {
		t.Fatalf("window %d history %d: %v", len(input.Previous), len(doc.Turns), err)
	}
	view, err := sideConversationView(doc, sideQuestionBinding{}, "")
	if err != nil || len(view.Turns) != 50 || view.NextCursor != "10" {
		t.Fatalf("page %+v %v", view, err)
	}
	older, err := sideConversationView(doc, sideQuestionBinding{}, view.NextCursor)
	if err != nil || len(older.Turns) != 10 || older.NextCursor != "" {
		t.Fatalf("older %+v %v", older, err)
	}
}

func TestPersistentSideQuestionEscapedPagesStayBoundedAndComplete(t *testing.T) {
	doc := &sideConversationDocument{ChainID: newUUID()}
	for i := 0; i < 70; i++ {
		doc.Turns = append(doc.Turns, sideConversationTurn{SideQuestionExchange: protocol.SideQuestionExchange{OperationID: newUUID(), State: "completed", Question: "q", Answer: strings.Repeat("<", 16<<10)}})
	}
	cursor, count := "", 0
	seen := map[string]bool{}
	for {
		page, err := sideConversationView(doc, sideQuestionBinding{}, cursor)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := json.Marshal(page)
		if err != nil || len(wire) > maxSideConversationPageBytes || len(page.Turns) == 0 {
			t.Fatal("oversized or empty page")
		}
		for _, turn := range page.Turns {
			if seen[turn.OperationID] {
				t.Fatal("duplicate across pages")
			}
			seen[turn.OperationID] = true
			count++
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if count != 70 || len(doc.Turns) != 70 {
		t.Fatal("history disappeared while paging")
	}
}

func TestPersistentSideQuestionTruncatedContextNoticeSurvivesRestart(t *testing.T) {
	r, snapshot, binding, chain := persistentSideFixture(t)
	doc := &sideConversationDocument{Version: 1, Owner: "phone", Key: "codex:parent", ChainID: chain, Source: binding.Persistent, Snapshot: snapshot}
	for i := 0; i < 12; i++ {
		doc.Turns = append(doc.Turns, sideConversationTurn{SideQuestionExchange: protocol.SideQuestionExchange{OperationID: newUUID(), Question: "q", Answer: strings.Repeat("a", 8<<10), State: "completed", CreatedAt: time.Now().UnixMilli()}})
	}
	if err := r.saveConversationLocked(sideConversationStorageKey(doc.Owner, doc.Key, doc.ChainID), doc); err != nil {
		t.Fatal(err)
	}
	guard := func() error { return nil }
	view, err := r.conversation(context.Background(), doc.Key, doc.Owner, snapshot, protocol.SideQuestionConversationRequest{Action: "send", ChainID: chain, SideQuestionRequest: protocol.SideQuestionRequest{OperationID: newUUID(), ExpectedRevision: sideConversationRevision(doc, binding), PreviousOperationID: sideConversationHead(doc), Question: "continue"}}, binding, guard, func(_ context.Context, input sideQuestionInput) (sideQuestionAnswer, bool, error) {
		if !input.Snapshot.Truncated || len(input.Previous) >= 12 {
			t.Fatal("unbounded request")
		}
		return sideQuestionAnswer{Text: "answer"}, true, nil
	}, nil)
	if err != nil || !view.ContextTruncated {
		t.Fatalf("missing truncation notice: %+v %v", view, err)
	}
	restarted := &sideQuestionRegistry{persistentDir: r.persistentDir}
	view, err = restarted.conversation(context.Background(), doc.Key, doc.Owner, SideQuestionSnapshot{}, protocol.SideQuestionConversationRequest{Action: "read", ChainID: chain}, binding, guard, nil, nil)
	if err != nil || !view.ContextTruncated || len(view.Turns) != 13 {
		t.Fatalf("restart lost notice or history: %+v %v", view, err)
	}
}
