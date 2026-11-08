package agents

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"salcara/bridge/internal/protocol"
)

func hermesHistorySnapshot(t *testing.T, snapshot *hermesSnapshot, raw string) {
	t.Helper()
	if err := json.Unmarshal([]byte(raw), snapshot); err != nil {
		t.Fatal(err)
	}
}

func hermesCompleteFixture(s *hermesSession, admission string, generation, sequence int64, user, final int64, input string) hermesNativeEvent {
	payload, _ := json.Marshal(map[string]any{"text": "same reply", "outcome": "completed", "persisted_turn": hermesPersistedTurn{Rows: []int64{user, final}, User: user, Final: final, Submission: input, Complete: true}})
	return hermesNativeEvent{SessionID: s.NativeID, Type: "message.complete", AdmissionID: admission, Generation: generation, Replay: "replay-1", Sequence: sequence, Payload: payload}
}

func TestHermesPersistedRowsReconcileBufferedAndLateCompletionWithoutTextDedup(t *testing.T) {
	for _, buffered := range []bool{true, false} {
		t.Run(map[bool]string{true: "buffered", false: "after-snapshot"}[buffered], func(t *testing.T) {
			a, f, settings, snapshot := hermesFixture(t)
			id := hermesStart(t, a, snapshot)
			s, _ := a.session(id)
			s.Inputs["input-1"], s.Inputs["input-2"] = "client-1", "client-2"
			s.Admissions["admission-1"], s.Admissions["admission-2"] = "input-1", "input-2"
			hermesHistorySnapshot(t, snapshot, `{"execution_generation":1,"running":true,"last_sequence":10,"pending":[{"status":"started","admission_id":"admission-1"}],"messages":[{"row_id":1,"role":"user","content":"same prompt","display_metadata":{"gateway_input_owner":"owner-principal","submission_id":"input-1"}},{"row_id":2,"role":"assistant","content":"same reply"}]}`)
			base := f.call
			f.call = func(method string, p map[string]any) (any, error) {
				if method == "prompt.receipt" {
					return map[string]any{"admission_id": p["admission_id"], "status": "started", "ref": map[string]string{"session_id": s.NativeID}}, nil
				}
				if method == "session.resume" && buffered {
					a.event(f, id, s, hermesCompleteFixture(s, "admission-1", 1, 11, 1, 2, "input-1"))
				}
				return base(method, p)
			}
			_, rows, err := a.Open(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			phone := map[string]protocol.Event{}
			for _, row := range rows {
				if row.Type == "message" {
					phone[row.ID] = row
				}
			}
			a.BindEventSink(func(event protocol.Event) {
				if event.Type == "message" {
					phone[event.ID] = event
				}
			})
			if !buffered {
				a.event(f, id, s, hermesCompleteFixture(s, "admission-1", 1, 11, 1, 2, "input-1"))
			}
			phoneRows := []protocol.Event{}
			for _, row := range phone {
				phoneRows = append(phoneRows, row)
			}
			assertHermesRows(t, phoneRows, 1)
			assertHermesRows(t, rows, 1)
			assertHermesRows(t, s.Events, 1)
			// A second, identical prompt/reply is a different native row/turn.
			buffered = false
			hermesHistorySnapshot(t, snapshot, `{"execution_generation":2,"running":true,"last_sequence":20,"pending":[{"status":"started","admission_id":"admission-2"}],"messages":[{"row_id":1,"role":"user","content":"same prompt","display_metadata":{"gateway_input_owner":"owner-principal","submission_id":"input-1"}},{"row_id":2,"role":"assistant","content":"same reply"},{"row_id":3,"role":"user","content":"same prompt","display_metadata":{"gateway_input_owner":"owner-principal","submission_id":"input-2"}},{"row_id":4,"role":"assistant","content":"same reply"}]}`)
			if _, _, err = a.Open(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			a.event(f, id, s, hermesCompleteFixture(s, "admission-2", 2, 21, 3, 4, "input-2"))
			assertHermesRows(t, s.Events, 2)
			// Bindings and local echo provenance survive a viewer restart.
			restored := newHermesAgent(func() Settings { return *settings }, filepath.Dir(a.stateDir))
			defer restored.Close()
			r, _ := restored.session(id)
			snapshot.Pending = nil
			snapshot.Running = false
			snapshot.Sequence = 22
			restored.mu.Lock()
			restored.applySnapshotLocked(id, r, *snapshot)
			restored.mu.Unlock()
			assertHermesRows(t, r.Events, 2)
		})
	}
}

func TestHermesLateReceiptKeepsAlreadyPublishedStoredIdentity(t *testing.T) {
	a, f, _, snapshot := hermesFixture(t)
	id := hermesStart(t, a, snapshot)
	s, _ := a.session(id)
	hermesHistorySnapshot(t, snapshot, `{"execution_generation":1,"running":false,"last_sequence":10,"pending":[],"messages":[{"row_id":2,"role":"assistant","content":"same reply"}]}`)
	_, rows, err := a.Open(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	phone := map[string]protocol.Event{}
	for _, row := range rows {
		if row.Type == "message" {
			phone[row.ID] = row
		}
	}
	a.BindEventSink(func(event protocol.Event) {
		if event.Type == "message" {
			phone[event.ID] = event
		}
	})
	a.event(f, id, s, hermesCompleteFixture(s, "admission-1", 1, 11, 1, 2, "input-1"))
	if len(phone) != 1 || phone["history-2"].Text != "same reply" || !phone["history-2"].Final {
		t.Fatal("late receipt renamed a published bubble", phone)
	}
}

func assertHermesRows(t *testing.T, rows []protocol.Event, turns int) {
	t.Helper()
	users, assistants := map[string]bool{}, map[string]bool{}
	for _, e := range rows {
		if e.Type != "message" {
			continue
		}
		if e.Role == "assistant" && e.Text == "same reply" {
			if assistants[e.ID] {
				t.Fatal("duplicate assistant identity", e.ID)
			}
			assistants[e.ID] = true
		}
		if e.Role == "user" && e.Text == "same prompt" {
			if e.ClientID == "" || users[e.ClientID] {
				t.Fatal("lost or duplicate local input acknowledgement", e)
			}
			users[e.ClientID] = true
		}
	}
	if len(users) != turns || len(assistants) != turns {
		t.Fatalf("users=%d assistants=%d want=%d: %+v", len(users), len(assistants), turns, rows)
	}
}

func TestHermesReceiptReadRecoversDetachedCompletionAndKeepsTools(t *testing.T) {
	a, f, _, snapshot := hermesFixture(t)
	id := hermesStart(t, a, snapshot)
	s, _ := a.session(id)
	s.Inputs["input-1"] = "client-1"
	s.Admissions["admission-1"] = "input-1"
	a.event(f, id, s, hermesNativeEvent{SessionID: s.NativeID, Type: "tool.complete", AdmissionID: "admission-1", Generation: 1, Replay: "replay-1", Sequence: 2, Payload: json.RawMessage(`{"tool_call_id":"tool-1","tool_name":"terminal","result":"kept output"}`)})
	hermesHistorySnapshot(t, snapshot, `{"execution_generation":1,"running":false,"last_sequence":12,"pending":[],"messages":[{"row_id":1,"role":"user","content":"same prompt","display_metadata":{"gateway_input_owner":"owner-principal","submission_id":"input-1"}},{"row_id":2,"role":"assistant","content":"same reply"}]}`)
	base := f.call
	f.call = func(method string, p map[string]any) (any, error) {
		if method == "prompt.receipt" {
			if p["admission_id"] != "admission-1" {
				t.Fatal("used principal identity instead of accepted admission", p)
			}
			return map[string]any{"admission_id": "admission-1", "status": "terminal", "ref": map[string]string{"session_id": s.NativeID}, "result": map[string]any{"persisted_turn": hermesPersistedTurn{Rows: []int64{1, 2}, User: 1, Final: 2, Complete: true}}}, nil
		}
		return base(method, p)
	}
	_, rows, err := a.Open(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	assertHermesRows(t, rows, 1)
	if rows[1].ID != "hermes-reply-admission-1" {
		t.Fatal("detached completion did not recover canonical stream identity")
	}
	found := false
	for _, e := range rows {
		found = found || e.Type == "tool" && e.Output == "kept output"
	}
	if !found {
		t.Fatal("history read dropped tool output")
	}
}

func TestHermesReadFailureRetainsCauseWithoutPublishingSecrets(t *testing.T) {
	for _, cause := range []error{context.DeadlineExceeded, context.Canceled, &rpcError{Code: -32000, Message: "fixture-private-key C:/private/profile"}, errors.New("fixture-private-key C:/private/profile")} {
		err := newHermesReadFailure(cause)
		if !errors.Is(err, cause) || strings.Contains(err.Error(), "fixture-private-key") || strings.Contains(err.Error(), "C:/private") {
			t.Fatal("read diagnostic lost its cause or exposed private data", err)
		}
	}
	if !strings.Contains(newHermesReadFailure(context.DeadlineExceeded).Error(), "读取超时") {
		t.Fatal("timeout classification missing")
	}
	err := newHermesReadFailure(&rpcError{Code: -32000, Message: "admission_conflict"})
	var native *rpcError
	if !errors.As(err, &native) || !strings.Contains(err.Error(), "admission_conflict") {
		t.Fatal("safe native reason missing")
	}
}

func TestHermesActiveSnapshotKeepsStreamPrefixAndBoundsBindings(t *testing.T) {
	a, f, _, snapshot := hermesFixture(t)
	id := hermesStart(t, a, snapshot)
	s, _ := a.session(id)
	a.event(f, id, s, hermesNativeEvent{SessionID: s.NativeID, Type: "message.delta", AdmissionID: "admission-1", Generation: 1, Replay: "replay-1", Sequence: 2, Payload: json.RawMessage(`{"text":"hello"}`)})
	hermesHistorySnapshot(t, snapshot, `{"execution_generation":1,"running":true,"last_sequence":3,"pending":[{"status":"started","admission_id":"admission-1"}],"messages":[]}`)
	if _, _, err := a.Open(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	a.event(f, id, s, hermesNativeEvent{SessionID: s.NativeID, Type: "message.delta", AdmissionID: "admission-1", Generation: 1, Replay: "replay-1", Sequence: 4, Payload: json.RawMessage(`{"text":" world"}`)})
	if s.streams["admission-1"] != "hello world" {
		t.Fatal("snapshot discarded streaming prefix")
	}
	for i := 0; i < 1000; i++ {
		boundedHermesPut(s.Inputs, &s.InputOrder, strconv.Itoa(i), "client")
		boundedHermesPut(s.Admissions, &s.AdmissionOrder, strconv.Itoa(i), "input")
		boundedHermesPut(s.Receipts, &s.ReceiptOrder, strconv.Itoa(i), hermesPersistedTurn{})
		boundedHermesPut(s.ReplyIDs, &s.ReplyOrder, strconv.Itoa(i), "reply")
	}
	if len(s.Inputs) > 400 || len(s.Admissions) > 400 || len(s.Receipts) > 400 || len(s.ReplyIDs) > 400 {
		t.Fatal("unbounded native message identity cache")
	}
	if _, old := s.Inputs["599"]; old || s.InputOrder[0] != "600" || s.InputOrder[399] != "999" {
		t.Fatal("identity eviction did not preserve the most recent 400 inputs")
	}
}
