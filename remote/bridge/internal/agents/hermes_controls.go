package agents

import (
	"context"
	"salcara/bridge/internal/protocol"
	"strings"
	"time"
)

// Detaching a CLI/viewer never cancels gateway work. Only the exact observed
// execution generation is eligible for a native interrupt.
func (a *hermesAgent) Interrupt(context.Context, string) error { return ErrExpectedTurnRequired }
func (a *hermesAgent) InterruptTurn(ctx context.Context, id, expected, operation string) (protocol.InterruptResult, error) {
	result := protocol.InterruptResult{TurnID: expected}
	if !ValidTurnID(expected) || !ValidControlOperationID(operation) {
		return result, ErrExpectedTurnRequired
	}
	s, err := a.session(id)
	if err != nil {
		return result, err
	}
	s.control.Lock()
	defer s.control.Unlock()
	conn, err := a.runtime(ctx)
	if err != nil {
		return result, err
	}
	if err = a.resume(ctx, id, s, conn, ""); err != nil {
		return result, err
	}
	a.mu.Lock()
	generation, epoch, busy := s.Generation, s.Epoch, s.busy
	a.mu.Unlock()
	if expected != hermesTurnID(epoch, generation) {
		return result, ErrTurnControlStale
	}
	if !busy {
		result.Status = "already_finished"
		return result, nil
	}
	var handle struct {
		Generation int64 `json:"execution_generation"`
		Ref        struct {
			Session string `json:"session_id"`
		} `json:"ref"`
	}
	if err = conn.Call(ctx, "session.interrupt", map[string]any{"session_id": s.NativeID, "execution_generation": generation}, &handle); err != nil {
		return result, hermesError(err)
	}
	if handle.Generation != generation || handle.Ref.Session != s.NativeID {
		return result, hermesUnknown()
	}
	result.Status = "requested"
	return result, nil
}
func (a *hermesAgent) Respond(approval, decision, message string) bool {
	return a.respond(context.Background(), "", approval, ApprovalResponse{Decision: decision, Message: message}, false)
}
func (a *hermesAgent) RespondForSession(ctx context.Context, sessionKey, approval string, response ApprovalResponse) bool {
	return a.respond(ctx, sessionKey, approval, response, true)
}
func (a *hermesAgent) respond(ctx context.Context, sessionKey, approval string, response ApprovalResponse, scoped bool) bool {
	if response.Answers != nil || response.Message != "" {
		return false
	}
	parts := strings.SplitN(approval, ":", 3)
	if len(parts) != 3 || parts[0] != hermesID || scoped && sessionKey != hermesID+":"+parts[1] {
		return false
	}
	s, err := a.session(parts[1])
	if err != nil {
		return false
	}
	s.control.Lock()
	defer s.control.Unlock()
	a.mu.Lock()
	prompt, ok := s.prompts[parts[2]]
	source, model, conn := s.Source, s.Info.Model, a.conn
	generation := s.Generation
	closed := a.closed
	a.mu.Unlock()
	if !ok || conn == nil || !conn.alive() || closed || prompt.Generation != generation {
		return false
	}
	if scoped && extensionSource(settingsForContext(ctx, a.settings).Extensions[hermesID], model) != source {
		return false
	}
	choice := map[string]string{"allow": "once", "allow_session": "session", "deny": "deny"}[response.Decision]
	if choice == "" || !containsHermes(prompt.Choices, choice) {
		return false
	}
	replyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var result struct {
		Status string `json:"status"`
		ID     string `json:"prompt_id"`
	}
	err = conn.Call(replyCtx, "approval.respond", map[string]any{"session_id": s.NativeID, "execution_generation": prompt.Generation, "prompt_id": prompt.ID, "choice": choice}, &result)
	if err != nil || result.ID != prompt.ID || result.Status != "resolved" {
		return false
	}
	a.mu.Lock()
	delete(s.prompts, prompt.ID)
	a.mu.Unlock()
	a.emit(parts[1], protocol.Event{Type: "approval.resolved", ApprovalID: approval, Decision: response.Decision})
	return true
}

var _ PreciseInterrupter = (*hermesAgent)(nil)
