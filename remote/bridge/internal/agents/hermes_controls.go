package agents

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
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
	if err = a.resume(ctx, id, s, conn, "", true); err != nil {
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
	ok, _ := a.respondResult(context.Background(), "", approval, ApprovalResponse{Decision: decision, Message: message}, false)
	return ok
}
func (a *hermesAgent) RespondForSession(ctx context.Context, sessionKey, approval string, response ApprovalResponse) bool {
	ok, _ := a.RespondForSessionResult(ctx, sessionKey, approval, response)
	return ok
}
func (a *hermesAgent) RespondForSessionResult(ctx context.Context, sessionKey, approval string, response ApprovalResponse) (bool, error) {
	return a.respondResult(ctx, sessionKey, approval, response, true)
}
func (a *hermesAgent) RespondWithAnswersResult(ctx context.Context, approval string, response ApprovalResponse) (bool, error) {
	return a.respondResult(ctx, "", approval, response, false)
}

type hermesApprovalAttempt struct {
	OperationID string   `json:"operationId"`
	Digest      [32]byte `json:"digest"`
	Generation  int64    `json:"generation"`
	Epoch       int64    `json:"epoch"`
	Decision    string   `json:"decision"`
	By          string   `json:"by,omitempty"`
	Status      string   `json:"status"` // pending | confirmed | settled_unknown
}

func hermesApprovalUnknown() error {
	return &rpcDeliveryUncertainError{message: "Hermes 审批结果待确认；已保留原操作，请刷新原会话核对，不会重新授权或改选"}
}

type hermesApprovalFailure struct{ code, message string }

func (e *hermesApprovalFailure) Error() string              { return e.message }
func (e *hermesApprovalFailure) ExecutionErrorCode() string { return e.code }

func hermesApprovalNotSent(message string) error {
	return &hermesApprovalFailure{code: "approval_not_sent", message: message}
}

func hermesApprovalRPCFailure(err error) error {
	var unsent *hermesNotSentError
	if errors.As(err, &unsent) {
		return hermesApprovalNotSent(unsent.Error())
	}
	var remote *rpcError
	if errors.As(err, &remote) {
		if remote.Code == -32002 && remote.Message == "gateway_busy" || remote.Code == -32601 && remote.Message == "unsupported_method" {
			return hermesApprovalNotSent("Hermes 控制通道繁忙或不支持此操作，尚未发送；请刷新后重试")
		}
		// Frozen native contract: session_controls.dispatch emits 4001 for
		// RuntimeStoreError; respond/authorize/check_approval_generation and
		// PendingControls validate these before invoking the native resolver.
		// Other RPC errors (including internal/storage failures) may follow a
		// side effect, so they never release the durable uncertainty record.
		if remote.Code == 4001 && containsHermes([]string{"invalid_params", "permission_denied", "stale_generation", "profile_mismatch", "not_found"}, remote.Message) {
			return &hermesApprovalFailure{code: "approval_rejected", message: "Hermes 未采纳本次回答（" + remote.Message + "），请刷新原问题后重试"}
		}
	}
	return hermesApprovalUnknown()
}

func (a *hermesAgent) respondResult(ctx context.Context, sessionKey, approval string, response ApprovalResponse, scoped bool) (bool, error) {
	parts := strings.SplitN(approval, ":", 3)
	if len(parts) != 3 || parts[0] != hermesID {
		return false, nil
	}
	if scoped && sessionKey != hermesID+":"+parts[1] {
		return false, hermesApprovalNotSent("问题不属于这个会话，尚未发送")
	}
	s, err := a.session(parts[1])
	if err != nil {
		return false, hermesApprovalNotSent(err.Error())
	}
	s.control.Lock()
	defer s.control.Unlock()
	body, _ := json.Marshal(struct {
		Decision string
		Answers  map[string][]string
	}{response.Decision, response.Answers})
	digest := sha256.Sum256(body)
	a.mu.Lock()
	previous, attempted := s.Approvals[parts[2]]
	prompt, ok := s.prompts[parts[2]]
	source, model, conn := s.Source, s.Info.Model, a.conn
	generation, epoch := s.Generation, s.Epoch
	expiry := s.PromptDeadlines[parts[2]]
	closed := a.closed
	a.mu.Unlock()
	if scoped && extensionSource(settingsForContext(ctx, a.settings).Extensions[hermesID], model) != source {
		if attempted {
			return false, hermesApprovalUnknown()
		}
		return false, hermesApprovalNotSent("原会话的 API 来源已变化，回答尚未发送；请核对原配置")
	}
	if attempted {
		if previous.Status == "confirmed" && previous.OperationID == response.OperationID && previous.Digest == digest {
			return true, nil
		}
		return false, hermesApprovalUnknown()
	}
	if err := ctx.Err(); err != nil {
		return false, hermesApprovalNotSent(err.Error())
	}
	if response.Message != "" {
		return false, hermesApprovalNotSent("Hermes 问题需要结构化回答，尚未发送")
	}
	if !ok || conn == nil || !conn.alive() || closed || prompt.Generation != generation {
		return false, hermesApprovalNotSent("问题已变化或控制连接不可用，尚未发送；请刷新原会话")
	}
	if expiry > 0 && time.Now().UnixMilli() >= expiry {
		return false, hermesApprovalNotSent("Hermes 问题已过期，尚未发送；请刷新原会话")
	}
	method := "approval.respond"
	params := map[string]any{"session_id": s.NativeID, "execution_generation": prompt.Generation, "prompt_id": prompt.ID}
	if prompt.Kind == "clarify" {
		answer, err := hermesClarifyAnswer(prompt, response)
		if err != nil {
			return false, hermesApprovalNotSent(err.Error())
		}
		method, params["answer"] = "clarify.respond", answer
	} else {
		choice := map[string]string{"allow": "once", "allow_session": "session", "deny": "deny"}[response.Decision]
		if response.Answers != nil || choice == "" || !containsHermes(prompt.Choices, choice) {
			return false, hermesApprovalNotSent("回答不在当前问题允许的选择中，尚未发送")
		}
		params["choice"] = choice
	}
	operation := response.OperationID
	if operation == "" {
		operation = deepSeekNewID()
	}
	if operation == "" {
		return false, hermesApprovalNotSent("无法保存审批操作身份，尚未发送")
	}
	a.mu.Lock()
	for _, previousID := range s.ApprovalOrder {
		if len(s.Approvals) < 400 {
			break
		}
		old := s.Approvals[previousID]
		if _, active := s.prompts[previousID]; !active && (old.Status == "confirmed" || old.Status == "settled_unknown") {
			delete(s.Approvals, previousID)
		}
	}
	normalizeHermesOrder(s.Approvals, &s.ApprovalOrder)
	if len(s.Approvals) >= 400 {
		a.mu.Unlock()
		return false, hermesApprovalNotSent("Hermes 审批记录已达上限，请在电脑核对；尚未发送")
	}
	by := response.By
	if by == "" {
		by = "desktop"
		if scoped {
			by = "phone"
		}
	}
	attempt := hermesApprovalAttempt{OperationID: operation, Digest: digest, Generation: generation, Epoch: epoch, Decision: response.Decision, By: by, Status: "pending"}
	s.Approvals[prompt.ID] = attempt
	s.ApprovalOrder = append(s.ApprovalOrder, prompt.ID)
	err = a.persistHermesApprovalLocked(parts[1], s)
	if err != nil {
		delete(s.Approvals, prompt.ID)
	}
	a.mu.Unlock()
	if err != nil {
		return false, hermesApprovalNotSent("无法保存审批操作记录，尚未发送")
	}
	replyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var result struct {
		Status string `json:"status"`
		ID     string `json:"prompt_id"`
	}
	err = hermesGuardedCall(replyCtx, conn, method, params, &result, func() error {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.closed || a.conn != conn || s.Generation != generation || s.Epoch != epoch {
			return errors.New("审批所属运行实例已变化，尚未发送")
		}
		if expiry > 0 && time.Now().UnixMilli() >= expiry {
			return errors.New("Hermes 问题已过期，尚未发送")
		}
		return nil
	})
	if err != nil {
		mapped := hermesApprovalRPCFailure(err)
		if protocol.ExecutionErrorCode(mapped) != "command_delivery_uncertain" {
			a.mu.Lock()
			delete(s.Approvals, prompt.ID)
			_ = a.persistLocked(parts[1], s)
			a.mu.Unlock()
			return false, mapped
		}
		a.markUnknown(parts[1], s)
		return false, hermesApprovalUnknown()
	}
	if result.ID != prompt.ID || result.Status != "resolved" {
		if result.ID == prompt.ID && result.Status == "already_resolved" {
			a.mu.Lock()
			delete(s.prompts, prompt.ID)
			attempt.Status = "settled_unknown"
			s.Approvals[prompt.ID] = attempt
			_ = a.persistLocked(parts[1], s)
			a.mu.Unlock()
			a.emit(parts[1], protocol.Event{Type: "approval.resolved", ApprovalID: approval, Decision: "unknown", By: "unknown", OperationID: operation})
			return false, &hermesApprovalFailure{code: "approval_rejected", message: "Hermes 问题已由原生会话处理或过期，本次回答未被采用，请刷新核对"}
		}
		a.markUnknown(parts[1], s)
		return false, hermesApprovalUnknown()
	}
	a.mu.Lock()
	delete(s.prompts, prompt.ID)
	attempt.Status = "confirmed"
	s.Approvals[prompt.ID] = attempt
	if len(s.prompts) == 0 && s.busy {
		s.Info.Status = "running"
	}
	_ = a.persistLocked(parts[1], s)
	info := s.Info
	a.mu.Unlock()
	a.emit(parts[1], protocol.Event{Type: "approval.resolved", ApprovalID: approval, Decision: response.Decision, By: by, OperationID: operation})
	a.emit(parts[1], protocol.Event{Type: "session.updated", Session: &info})
	return true, nil
}

var _ PreciseInterrupter = (*hermesAgent)(nil)
