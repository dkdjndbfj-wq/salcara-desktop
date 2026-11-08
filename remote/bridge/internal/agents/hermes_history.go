package agents

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"

	"salcara/bridge/internal/protocol"
)

// The native owner explicitly addresses the committed rows. Equal text is
// never identity: two turns may legitimately produce exactly the same reply.
type hermesPersistedTurn struct {
	Rows       []int64 `json:"row_ids"`
	User       int64   `json:"user_row_id"`
	Final      int64   `json:"final_assistant_row_id"`
	Submission string  `json:"submission_id"`
	Complete   bool    `json:"complete"`
}

func normalizeHermesOrder[V any](values map[string]V, order *[]string) {
	seen := map[string]bool{}
	clean := []string{}
	for _, key := range *order {
		if _, ok := values[key]; ok && !seen[key] {
			clean = append(clean, key)
			seen[key] = true
		}
	}
	missing := []string{}
	for key := range values {
		if !seen[key] {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	clean = append(clean, missing...)
	for len(clean) > 400 {
		delete(values, clean[0])
		clean = clean[1:]
	}
	*order = clean
}

func boundedHermesPut[V any](values map[string]V, order *[]string, key string, value V) {
	if len(values) != len(*order) {
		normalizeHermesOrder(values, order)
	}
	if _, exists := values[key]; !exists {
		if len(*order) >= 400 {
			delete(values, (*order)[0])
			*order = (*order)[1:]
		}
		*order = append(*order, key)
	}
	values[key] = value
}

func hermesRowID(raw json.RawMessage) int64 {
	var id int64
	if json.Unmarshal(raw, &id) != nil || id <= 0 {
		return 0
	}
	return id
}

func (p hermesPersistedTurn) hasRow(id int64) bool {
	if id <= 0 || len(p.Rows) > 400 {
		return false
	}
	for _, row := range p.Rows {
		if row == id {
			return true
		}
	}
	return false
}

func hermesBindReceipt(s *hermesSession, admission string, receipt hermesPersistedTurn) {
	if !ValidTurnID(admission) || !receipt.hasRow(receipt.Final) {
		return
	}
	boundedHermesPut(s.Receipts, &s.ReceiptOrder, admission, receipt)
	if input, known := s.Admissions[admission]; !known || input == "" {
		if _, submitted := s.Inputs[receipt.Submission]; submitted {
			boundedHermesPut(s.Admissions, &s.AdmissionOrder, admission, receipt.Submission)
		}
	}
	// A transcript read may race terminal publication after SQLite commits.
	// Rebind only the explicitly addressed final row, leaving commentary/tools.
	stored := "history-" + strconv.FormatInt(receipt.Final, 10)
	canonical := hermesReplyID(s, admission)
	if _, fixed := s.ReplyIDs[admission]; !fixed {
		storedSeen, streamed := false, false
		for _, event := range s.Events {
			storedSeen = storedSeen || event.Type == "message" && event.Role == "assistant" && event.ID == stored
			streamed = streamed || event.Type == "message" && event.Role == "assistant" && event.ID == canonical
		}
		// A published stored row with no prior stream keeps its public ID.
		// Renaming only the cache would leave the old bubble on the phone.
		if storedSeen && !streamed {
			canonical = stored
		}
		boundedHermesPut(s.ReplyIDs, &s.ReplyOrder, admission, canonical)
	}
	for i := range s.Events {
		if s.Events[i].Type == "message" && s.Events[i].Role == "assistant" && s.Events[i].ID == stored {
			s.Events[i].ID = canonical
		}
	}
	seen := map[string]bool{}
	rows := s.Events[:0]
	for _, event := range s.Events {
		if event.Type == "message" && event.ID == canonical {
			if seen[canonical] {
				continue
			}
			seen[canonical] = true
		}
		rows = append(rows, event)
	}
	s.Events = rows
}

func hermesReplyID(s *hermesSession, admission string) string {
	if id := s.ReplyIDs[admission]; id != "" {
		return id
	}
	return "hermes-reply-" + admission
}

func hermesReceiptFromEvent(s *hermesSession, event hermesNativeEvent) {
	if event.SessionID != s.NativeID || event.Type != "message.complete" {
		return
	}
	var payload struct {
		Admission string              `json:"admission_id"`
		Receipt   hermesPersistedTurn `json:"persisted_turn"`
	}
	if json.Unmarshal(event.Payload, &payload) != nil {
		return
	}
	admission := event.AdmissionID
	if admission == "" {
		admission = payload.Admission
	}
	hermesBindReceipt(s, admission, payload.Receipt)
}

// gateway_input_owner is a principal identity, not an admission. The native
// submission ID must join this viewer's accepted admission receipt.
func hermesAdmissionForInput(s *hermesSession, submission string) string {
	if !ValidTurnID(submission) {
		return ""
	}
	match := ""
	for admission, input := range s.Admissions {
		if input == submission && ValidTurnID(admission) {
			if match != "" && match != admission {
				return ""
			}
			match = admission
		}
	}
	return match
}

// Recover row bindings for this Bridge's known admissions when completion
// happened while its viewer was detached. This is a read, never a resubmission.
func (a *hermesAgent) hydrateHermesReceipts(ctx context.Context, s *hermesSession, conn hermesGateway, snapshot hermesSnapshot) {
	seen := map[string]bool{}
	start := len(snapshot.Messages) - 400
	if start < 0 {
		start = 0
	}
	for i := len(snapshot.Messages) - 1; i >= start; i-- {
		row := snapshot.Messages[i]
		if row.Role != "user" {
			continue
		}
		a.mu.Lock()
		admission := hermesAdmissionForInput(s, row.Metadata.Submission)
		_, bound := s.Receipts[admission]
		a.mu.Unlock()
		if !ValidTurnID(admission) || seen[admission] {
			continue
		}
		seen[admission] = true
		if bound {
			continue
		}
		var response struct {
			Admission string `json:"admission_id"`
			Status    string `json:"status"`
			Ref       struct {
				Session string `json:"session_id"`
			} `json:"ref"`
			Result struct {
				Receipt hermesPersistedTurn `json:"persisted_turn"`
			} `json:"result"`
		}
		if conn.Call(ctx, "prompt.receipt", map[string]any{"session_id": s.NativeID, "admission_id": admission, "include_result": true}, &response) != nil {
			continue
		}
		if response.Admission != admission || response.Ref.Session != s.NativeID || response.Status != "terminal" {
			continue
		}
		response.Result.Receipt.Submission = row.Metadata.Submission
		a.mu.Lock()
		if a.conn == conn {
			hermesBindReceipt(s, admission, response.Result.Receipt)
		}
		a.mu.Unlock()
	}
}

func hermesSnapshotBindings(s *hermesSession) map[int64]string {
	bindings := map[int64]string{}
	for admission, receipt := range s.Receipts {
		if receipt.hasRow(receipt.Final) {
			bindings[receipt.Final] = hermesReplyID(s, admission)
		}
	}
	return bindings
}

func hermesPreserveLiveRows(s *hermesSession, old []protocol.Event, active map[string]bool) {
	seen := map[string]bool{}
	for _, e := range s.Events {
		seen[e.Type+":"+e.ID] = true
	}
	for _, e := range old {
		keep := e.Type == "tool" || e.Type == "reasoning" || e.Type == "notice" || e.Type == "turn"
		if e.Type == "message" && e.Role == "assistant" {
			for admission := range active {
				keep = keep || e.ID == hermesReplyID(s, admission)
			}
		}
		if keep && !seen[e.Type+":"+e.ID] {
			s.Events = append(s.Events, e)
			seen[e.Type+":"+e.ID] = true
		}
	}
	if len(s.Events) > 400 {
		s.Events = append([]protocol.Event(nil), s.Events[len(s.Events)-400:]...)
	}
}
