package agents

import (
	"context"
	"encoding/json"
	"errors"
	"salcara/bridge/internal/protocol"
	"strconv"
	"time"
)

type hermesPrompt struct {
	ID          string   `json:"prompt_id"`
	Kind        string   `json:"kind"`
	Generation  int64    `json:"execution_generation"`
	Command     string   `json:"command"`
	Description string   `json:"description"`
	Choices     []string `json:"choices"`
}
type hermesSnapshot struct {
	NativeID   string         `json:"stored_session_id"`
	SessionID  string         `json:"session_id"`
	Epoch      int64          `json:"authority_epoch"`
	Generation int64          `json:"execution_generation"`
	Running    bool           `json:"running"`
	Replay     string         `json:"replay_epoch"`
	Sequence   int64          `json:"last_sequence"`
	Prompts    []hermesPrompt `json:"prompts"`
	Pending    []struct {
		Status    string `json:"status"`
		Admission string `json:"admission_id,omitempty"`
	} `json:"pending"`
	Messages []struct {
		ID        json.RawMessage `json:"row_id"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		ToolCalls json.RawMessage `json:"tool_calls"`
		Metadata  struct {
			Submission string `json:"submission_id"`
		} `json:"display_metadata"`
	} `json:"messages"`
	Info struct {
		Cwd   string `json:"cwd"`
		Model string `json:"model"`
		Title string `json:"title"`
	} `json:"info"`
}
type hermesNativeEvent struct {
	SessionID   string          `json:"session_id"`
	Type        string          `json:"type"`
	AdmissionID string          `json:"admission_id"`
	Generation  int64           `json:"execution_generation"`
	Epoch       int64           `json:"authority_epoch"`
	Replay      string          `json:"replay_epoch"`
	Sequence    int64           `json:"seq"`
	Payload     json.RawMessage `json:"payload"`
}

func (a *hermesAgent) resume(ctx context.Context, id string, s *hermesSession, conn hermesGateway, key string) error {
	a.mu.Lock()
	s.recovering = true
	s.buffered = nil
	s.overflow = false
	a.mu.Unlock()
	params := map[string]any{"session_id": s.NativeID}
	if key != "" {
		params["api_key"] = key
	}
	var snapshot hermesSnapshot
	err := conn.Call(ctx, "session.resume", params, &snapshot)
	if err != nil {
		err = newHermesReadFailure(err)
	}
	if err == nil && snapshot.NativeID == s.NativeID && snapshot.SessionID == s.NativeID {
		a.hydrateHermesReceipts(ctx, s, conn, snapshot)
	}
	a.mu.Lock()
	buffered, overflow := s.buffered, s.overflow
	s.recovering = false
	s.buffered = nil
	if err == nil && (snapshot.NativeID != s.NativeID || snapshot.SessionID != s.NativeID || snapshot.Info.Cwd != s.Info.Cwd || snapshot.Info.Model != "" && snapshot.Info.Model != s.Info.Model) {
		err = errors.New("Hermes 原会话目录或模型已变化，请在电脑核对")
	}
	if err == nil && !overflow {
		for _, event := range buffered {
			if event.Replay == snapshot.Replay && event.Generation >= snapshot.Generation {
				hermesReceiptFromEvent(s, event)
			}
		}
		a.applySnapshotLocked(id, s, snapshot)
	} else {
		s.uncertain = true
		s.Info.Status = "unknown"
		if err == nil {
			err = errors.New("Hermes 事件超过同步上限，请刷新")
		}
	}
	a.mu.Unlock()
	for _, event := range buffered {
		a.event(conn, id, s, event)
	}
	return err
}
func (a *hermesAgent) applySnapshotLocked(id string, s *hermesSession, snapshot hermesSnapshot) {
	oldEvents := s.Events
	active := map[string]bool{}
	s.Epoch, s.Generation, s.replay, s.sequence = snapshot.Epoch, snapshot.Generation, snapshot.Replay, snapshot.Sequence
	s.busy, s.uncertain = snapshot.Running, false
	s.prompts = map[string]hermesPrompt{}
	s.streams = map[string]string{}
	s.completed = map[string]bool{}
	for _, p := range snapshot.Pending {
		if p.Status == "unknown" {
			s.uncertain = true
		}
		if p.Status == "queued" || p.Status == "started" {
			s.busy = true
			if ValidTurnID(p.Admission) {
				active[p.Admission] = true
			}
		}
	}
	s.Info.Status = "idle"
	s.Info.ActiveTurnID = ""
	if s.busy {
		s.Info.Status = "running"
		if snapshot.Running {
			s.Info.ActiveTurnID = hermesTurnID(s.Epoch, s.Generation)
		}
	}
	if s.uncertain {
		s.Info.Status = "unknown"
	}
	s.Events = nil
	messages := snapshot.Messages
	if len(messages) > 400 {
		messages = messages[len(messages)-400:]
	}
	bindings := hermesSnapshotBindings(s)
	currentAdmission := ""
	for index, row := range messages {
		if row.Role == "user" {
			currentAdmission = hermesAdmissionForInput(s, row.Metadata.Submission)
		}
		if row.Role != "user" && row.Role != "assistant" {
			continue
		}
		var text string
		if json.Unmarshal(row.Content, &text) != nil {
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(row.Content, &blocks) != nil {
				continue
			}
			for _, block := range blocks {
				if block.Type == "text" {
					text += block.Text
				}
			}
			if text == "" {
				continue
			}
		}
		messageID := "history-" + strconv.Itoa(index)
		if len(row.ID) > 0 {
			messageID = "history-" + string(row.ID)
		}
		client, final := "", true
		if canonical := bindings[hermesRowID(row.ID)]; canonical != "" && row.Role == "assistant" {
			messageID = canonical
		} else if row.Role == "assistant" && active[currentAdmission] && index == len(messages)-1 && (len(row.ToolCalls) == 0 || string(row.ToolCalls) == "[]" || string(row.ToolCalls) == "null") {
			// SQLite may contain the tail answer before its terminal receipt is
			// published. Keep it in the active admission's existing stream slot
			// until the native receipt binds the authoritative stored row.
			messageID, final = hermesReplyID(s, currentAdmission), false
		}
		if row.Role == "user" {
			if ownClient, known := s.Inputs[row.Metadata.Submission]; known {
				messageID, client = "hermes-input-"+row.Metadata.Submission, ownClient
			}
		}
		s.Events = append(s.Events, protocol.Event{Tool: hermesID, SessionKey: s.Info.SessionKey, Type: "message", ID: messageID, Role: row.Role, Text: truncHead(text, 100000), Final: final, ClientID: client})
	}
	hermesPreserveLiveRows(s, oldEvents, active)
	for _, e := range s.Events {
		for admission := range active {
			if e.Type == "message" && e.ID == hermesReplyID(s, admission) {
				s.streams[admission] = e.Text
			}
		}
	}
	for _, p := range snapshot.Prompts {
		if p.Kind == "approval" && ValidTurnID(p.ID) && p.Generation == s.Generation {
			s.prompts[p.ID] = p
			s.Info.Status = "waiting_approval"
			s.Info.ActiveTurnID = hermesTurnID(s.Epoch, s.Generation)
			s.Events = append(s.Events, hermesApprovalEvent(id, p))
		} else {
			s.uncertain = true
			s.Info.Status = "waiting_approval"
		}
	}
	if !s.busy && s.Generation > 0 {
		s.Info.LastTerminalTurnID = hermesTurnID(s.Epoch, s.Generation)
	}
	s.Info.UpdatedAt = time.Now().UnixMilli()
	_ = a.persistLocked(id, s)
}
func hermesApprovalEvent(id string, p hermesPrompt) protocol.Event {
	decisions := []string{}
	for _, m := range [][2]string{{"once", "allow"}, {"session", "allow_session"}, {"deny", "deny"}} {
		if containsHermes(p.Choices, m[0]) {
			decisions = append(decisions, m[1])
		}
	}
	return protocol.Event{Type: "approval.request", Tool: hermesID, SessionKey: hermesID + ":" + id, ApprovalID: hermesID + ":" + id + ":" + p.ID, Kind: "tool", Title: truncHead(p.Description, 200), Detail: truncHead(p.Command, 4000), ApprovalOptions: &protocol.ApprovalOptions{Version: 1, Decisions: decisions}}
}
func (a *hermesAgent) notify(conn hermesGateway, method string, raw json.RawMessage) {
	a.mu.Lock()
	if a.closed || a.conn != conn {
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()
	if method == "gateway.disconnected" {
		a.mu.Lock()
		if a.conn == conn {
			a.conn = nil
		}
		rows := map[string]*hermesSession{}
		for id, s := range a.sessions {
			rows[id] = s
		}
		a.mu.Unlock()
		for id, s := range rows {
			a.markUnknown(id, s)
		}
		go conn.detach()
		return
	}
	if method != "event" {
		return
	}
	var event hermesNativeEvent
	if json.Unmarshal(raw, &event) != nil {
		return
	}
	a.mu.Lock()
	var id string
	var s *hermesSession
	for key, row := range a.sessions {
		if row.NativeID == event.SessionID {
			id, s = key, row
			break
		}
	}
	a.mu.Unlock()
	if s != nil {
		a.event(conn, id, s, event)
	}
}
func (a *hermesAgent) event(conn hermesGateway, id string, s *hermesSession, event hermesNativeEvent) {
	a.mu.Lock()
	if a.closed || a.conn != conn || event.SessionID != s.NativeID {
		a.mu.Unlock()
		return
	}
	if s.recovering {
		if len(s.buffered) < 1000 {
			s.buffered = append(s.buffered, event)
		} else {
			s.overflow = true
		}
		a.mu.Unlock()
		return
	}
	if event.Type == "session.replay_gap" {
		s.uncertain = true
		s.Info.Status = "unknown"
		a.mu.Unlock()
		a.emit(id, protocol.Event{Type: "notice", Level: "warning", Text: "Hermes 事件记录需要重新同步，请刷新会话；不会重发任务"})
		return
	}
	if event.Replay == s.replay && event.Sequence > 0 && event.Sequence <= s.sequence {
		a.mu.Unlock()
		return
	}
	if event.Replay != "" && s.replay != "" && event.Replay != s.replay {
		s.uncertain = true
		a.mu.Unlock()
		return
	}
	if event.Generation > 0 && event.Generation < s.Generation {
		a.mu.Unlock()
		return
	}
	if event.Generation > s.Generation {
		s.Generation = event.Generation
	}
	if event.Epoch > 0 {
		s.Epoch = event.Epoch
	}
	if event.Sequence > 0 {
		s.sequence = event.Sequence
	}
	if event.Replay != "" {
		s.replay = event.Replay
	}
	turn := hermesTurnID(s.Epoch, s.Generation)
	var p struct {
		Text           string          `json:"text"`
		Delta          string          `json:"delta"`
		Content        string          `json:"content"`
		Outcome        string          `json:"outcome"`
		Status         string          `json:"status"`
		Name           string          `json:"tool_name"`
		FallbackName   string          `json:"name"`
		ToolID         string          `json:"tool_call_id"`
		FallbackToolID string          `json:"tool_id"`
		AdmissionID    string          `json:"admission_id"`
		Args           json.RawMessage `json:"args"`
		Result         json.RawMessage `json:"result"`
		Error          bool            `json:"is_error"`
		PromptID       string          `json:"prompt_id"`
	}
	if json.Unmarshal(event.Payload, &p) != nil {
		s.uncertain = true
		s.Info.Status = "unknown"
		a.mu.Unlock()
		a.emit(id, protocol.Event{Type: "notice", Level: "warning", Text: "Hermes 返回了无法核对的事件，请刷新原会话"})
		return
	}
	if p.Name == "" {
		p.Name = p.FallbackName
	}
	if p.ToolID == "" {
		p.ToolID = p.FallbackToolID
	}
	admission := event.AdmissionID
	if admission == "" {
		admission = p.AdmissionID
	}
	if admission == "" {
		admission = turn
	}
	out := []protocol.Event{}
	switch event.Type {
	case "message.delta":
		if ValidTurnID(admission) {
			if _, exists := s.Admissions[admission]; !exists {
				boundedHermesPut(s.Admissions, &s.AdmissionOrder, admission, "")
			}
		}
		text := p.Text
		if text == "" {
			text = p.Delta
		}
		if text == "" {
			text = p.Content
		}
		s.streams[admission] = truncTail(s.streams[admission]+text, 100000)
		if !s.busy || s.Info.ActiveTurnID != turn {
			out = append(out, protocol.Event{Type: "turn", Status: "started", TurnID: turn})
		}
		s.busy = true
		s.Info.Status = "running"
		s.Info.ActiveTurnID = turn
		out = append(out, protocol.Event{Type: "message", ID: hermesReplyID(s, admission), Role: "assistant", Text: s.streams[admission], TurnID: turn})
	case "message.complete":
		if !containsHermes([]string{"completed", "failed", "cancelled", "interrupted", "unknown"}, p.Outcome) {
			s.uncertain = true
			s.Info.Status = "unknown"
			a.mu.Unlock()
			a.emit(id, protocol.Event{Type: "notice", Level: "warning", Text: "Hermes 任务终态未确认，请刷新原会话"})
			return
		}
		hermesReceiptFromEvent(s, event)
		text := p.Text
		if text == "" {
			text = p.Content
		}
		if text == "" {
			text = s.streams[admission]
		}
		delete(s.streams, admission)
		s.completed[admission] = true
		status := "completed"
		if p.Outcome == "failed" || p.Outcome == "unknown" || p.Status == "failed" {
			status = "failed"
		}
		if p.Outcome == "cancelled" || p.Outcome == "interrupted" || p.Status == "interrupted" {
			status = "interrupted"
		}
		s.busy = false
		s.Info.Status = "idle"
		if status == "failed" {
			s.Info.Status = "failed"
		}
		s.Info.ActiveTurnID = ""
		s.Info.LastTerminalTurnID = turn
		if p.Outcome == "unknown" {
			s.uncertain = true
			s.Info.Status = "unknown"
		}
		out = append(out, protocol.Event{Type: "message", ID: hermesReplyID(s, admission), Role: "assistant", Text: truncHead(text, 100000), Final: true, TurnID: turn}, protocol.Event{Type: "turn", Status: status, TurnID: turn})
	case "tool.start", "tool.complete":
		if event.Type == "tool.start" {
			if s.Info.ActiveTurnID != turn {
				out = append(out, protocol.Event{Type: "turn", Status: "started", TurnID: turn})
			}
			s.busy = true
			s.Info.Status = "running"
			s.Info.ActiveTurnID = turn
		}
		status := "running"
		if event.Type == "tool.complete" {
			status = "completed"
			if p.Error {
				status = "failed"
			}
		}
		var output string
		if json.Unmarshal(p.Result, &output) != nil {
			output = string(p.Result)
		}
		out = append(out, protocol.Event{Type: "tool", ID: p.ToolID, Title: p.Name, Kind: "other", Detail: truncHead(string(p.Args), 4000), Output: truncHead(output, 10000), OutputFinal: event.Type == "tool.complete", Status: status, TurnID: turn})
	case "approval.request":
		var prompt hermesPrompt
		if json.Unmarshal(event.Payload, &prompt) == nil && ValidTurnID(prompt.ID) && prompt.Generation == s.Generation {
			s.prompts[prompt.ID] = prompt
			s.Info.Status = "waiting_approval"
			s.busy = true
			s.Info.ActiveTurnID = turn
			out = append(out, hermesApprovalEvent(id, prompt))
		}
	case "approval.settled":
		delete(s.prompts, p.PromptID)
		out = append(out, protocol.Event{Type: "approval.resolved", ApprovalID: hermesID + ":" + id + ":" + p.PromptID})
		if len(s.prompts) == 0 && s.busy {
			s.Info.Status = "running"
		}
	case "clarify.request":
		s.uncertain = true
		s.Info.Status = "waiting_approval"
		out = append(out, protocol.Event{Type: "notice", Level: "warning", Text: "Hermes 需要补充回答，请在电脑打开原会话处理"})
	case "session.info":
		var state struct {
			Generation int64 `json:"execution_generation"`
			Pending    *[]struct {
				Status string `json:"status"`
			} `json:"pending"`
		}
		if json.Unmarshal(event.Payload, &state) == nil && state.Pending != nil {
			busy, started, unknown := false, false, false
			for _, item := range *state.Pending {
				busy = busy || item.Status == "queued" || item.Status == "started"
				started = started || item.Status == "started"
				unknown = unknown || item.Status == "unknown"
			}
			if state.Generation > s.Generation {
				s.Generation = state.Generation
				turn = hermesTurnID(s.Epoch, s.Generation)
			}
			s.busy = busy
			s.uncertain = s.uncertain || unknown
			if unknown {
				s.Info.Status = "unknown"
			} else if len(s.prompts) > 0 {
				s.Info.Status = "waiting_approval"
			} else if busy {
				s.Info.Status = "running"
			} else {
				s.Info.Status = "idle"
				s.Info.ActiveTurnID = ""
			}
			if started && s.Info.ActiveTurnID != turn {
				s.Info.ActiveTurnID = turn
				out = append(out, protocol.Event{Type: "turn", Status: "started", TurnID: turn})
			}
		}
	case "status.update", "notification.show":
		if p.Text != "" {
			out = append(out, protocol.Event{Type: "notice", Text: truncHead(p.Text, 4000)})
		}
	}
	s.Info.UpdatedAt = time.Now().UnixMilli()
	info := s.Info
	_ = a.persistLocked(id, s)
	a.mu.Unlock()
	for _, e := range out {
		a.emit(id, e)
	}
	a.emit(id, protocol.Event{Type: "session.updated", Session: &info})
}
