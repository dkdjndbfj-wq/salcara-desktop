package agents

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"salcara/bridge/internal/protocol"
)

// PermissionPath is where the `mcp-approval` helper asks the bridge for a Claude permission decision.
const PermissionPath = "/internal/agents/claude/permission"

// PermissionRequest is the helper → bridge body.
type PermissionRequest struct {
	Session   string          `json:"session"`
	ToolName  string          `json:"tool_name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
}

// PermissionDecision is what Claude Code expects back from a --permission-prompt-tool.
type PermissionDecision struct {
	Behavior     string          `json:"behavior"` // allow | deny
	UpdatedInput json.RawMessage `json:"updatedInput,omitempty"`
	Message      string          `json:"message,omitempty"`
}

func allowDecision(input json.RawMessage) PermissionDecision {
	if len(input) == 0 || string(input) == "null" {
		input = json.RawMessage("{}")
	}
	return PermissionDecision{Behavior: "allow", UpdatedInput: input}
}

func (a *claudeAgent) permissionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req PermissionRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
	var trailing any
	if err := decoder.Decode(&req); err != nil || decoder.Decode(&trailing) != io.EOF || req.Session == "" || req.ToolName == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	worker := a.permissionWorkerForToken(req.Session, r.Header.Get("X-Salcara-Token"))
	if worker == nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	dec := a.decideForWorker(r, req, worker)
	if r.Context().Err() != nil || !a.permissionWorkerActive(worker) {
		dec = PermissionDecision{Behavior: "deny", Message: "审批连接已结束"}
	}
	if req.ToolName == "ExitPlanMode" && dec.Behavior != "allow" {
		worker.mu.Lock()
		worker.requirePlan, worker.planExitApproved = true, false
		worker.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(dec)
}

func (a *claudeAgent) decide(r *http.Request, req PermissionRequest) PermissionDecision {
	a.mu.Lock()
	worker := a.procs[req.Session]
	a.mu.Unlock()
	return a.decideForWorker(r, req, worker)
}

func (a *claudeAgent) decideForWorker(r *http.Request, req PermissionRequest, worker *claudeProc) (decision PermissionDecision) {
	id := req.Session
	sk := "claude:" + id
	a.mu.Lock()
	if !a.permissionWorkerActiveLocked(worker) || worker.id != id || r.Context().Err() != nil {
		a.mu.Unlock()
		return PermissionDecision{Behavior: "deny", Message: "审批连接已结束"}
	}
	cwd := worker.cwd
	a.mu.Unlock()

	isQuestion := req.ToolName == "AskUserQuestion"
	// The legacy permission-prompt-tool payload cannot express why the runtime
	// is asking (explicit ask rules, interaction-required tools, etc.). Never
	// override that prompt with a broad ToolName grant or a second policy engine.
	// acceptEdits is enforced by Claude itself, before reaching this callback.

	var input map[string]any
	_ = json.Unmarshal(req.Input, &input)
	if req.ToolName == "ExitPlanMode" {
		worker.mu.Lock()
		worker.requirePlan, worker.planExitApproved = true, false
		worker.mu.Unlock()
		// The tool's own inline proposal is safe to project. Paths in its
		// input/output are never authority to read arbitrary local files.
		if plan := strings.TrimSpace(inStr(input, "plan")); plan != "" {
			proposalID := req.ToolUseID
			if proposalID == "" {
				proposalID = newID("plan_")
			}
			proposal := protocol.Event{SessionKey: sk, Type: "tool", ID: proposalID, TurnID: worker.activeTurnID(), Kind: "plan", PlanKind: "proposal", Title: "提交计划", Output: truncHead(plan, maxOutputChars), Status: "running", Cwd: cwd}
			a.emit(proposal)
			if req.ToolUseID == "" {
				// A generated ID has no native tool_result to complete its card.
				// Settle that exact proposal on every approval return path.
				defer func() {
					proposal.Status = "failed"
					if decision.Behavior == "allow" && r.Context().Err() == nil && a.permissionWorkerActive(worker) {
						proposal.Status = "done"
					}
					a.emit(proposal)
				}()
			}
		}
	}
	d := describeClaudeTool(req.ToolName, input, cwd, true)
	ev := protocol.Event{Kind: claudeApprovalKind(d.kind), Title: d.title, Detail: d.detail, Diff: d.diff, Cwd: cwd}
	var questions []protocol.Question
	var validate func(map[string][]string) error
	if isQuestion {
		var err error
		questions, err = parseToolQuestions(req.Input, "claude")
		if err != nil {
			a.emit(unsupportedQuestionNotice(sk, "Claude"))
			return PermissionDecision{Behavior: "deny", Message: "这个问题请在电脑上处理"}
		}
		ev = questionEvent("claude", questions, "需要你的回答", "", cwd)
		validate = func(answers map[string][]string) error { return validateQuestionAnswers(questions, answers) }
	}
	ev.ApprovalOptions = &protocol.ApprovalOptions{Version: 1, Decisions: []string{"allow", "deny"}}
	p, ev := a.aps.addValidatedRequestForLifetimes(sk, "claude", req.ToolUseID, ev, validate, worker.permissionLease.done, r.Context().Done())
	if p == nil {
		return PermissionDecision{Behavior: "deny", Message: "审批连接已结束"}
	}
	if !a.permissionWorkerActive(worker) {
		a.aps.remove(p.id)
		return PermissionDecision{Behavior: "deny", Message: "审批连接已结束"}
	}
	a.emit(ev)
	a.setApprovalStatus(worker)

	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		select {
		case <-r.Context().Done(): // the helper (or Claude Code) went away
		case <-a.closeCh:
		case <-worker.permissionLease.done:
		case <-finished:
			return
		}
		close(stop)
	}()
	ans := a.aps.wait(p, stop)
	close(finished)
	active := r.Context().Err() == nil && a.permissionWorkerActive(worker)
	if !active {
		ans = approvalAnswer{decision: "deny", by: "desktop", noReply: true}
	}
	if ans.by == "" {
		ans.by = "desktop"
	}
	a.emit(protocol.Event{SessionKey: sk, Type: "approval.resolved", ApprovalID: p.id, Decision: ans.decision, By: ans.by})
	if a.permissionWorkerActive(worker) {
		a.setApprovalStatus(worker)
	}
	if !active {
		return PermissionDecision{Behavior: "deny", Message: "审批连接已结束"}
	}

	if isQuestion && ans.decision == "allow" {
		// Claude's keys are the original question text, not the phone's opaque
		// IDs. Preserve every other tool input field and return actual answers.
		answers := map[string]any{}
		for _, question := range questions {
			values := ans.answers[question.ID]
			if question.MultiSelect {
				answers[question.Question] = strings.Join(values, ", ")
			} else {
				answers[question.Question] = values[0]
			}
		}
		input["answers"] = answers
		updated, err := json.Marshal(input)
		if err != nil {
			return PermissionDecision{Behavior: "deny", Message: "回答格式无效"}
		}
		return allowDecision(updated)
	}
	switch ans.decision {
	case "allow":
		if req.ToolName == "ExitPlanMode" {
			worker.mu.Lock()
			worker.planExitApproved = true
			worker.mu.Unlock()
		}
		return allowDecision(req.Input)
	}
	msg := strings.TrimSpace(ans.message)
	if msg == "" {
		msg = "用户在手机上拒绝了这个操作"
	}
	return PermissionDecision{Behavior: "deny", Message: msg}
}

// setApprovalStatus never projects a retired callback onto a successor which
// happens to use the same session ID. Registry reads stay outside vendor locks.
func (a *claudeAgent) setApprovalStatus(p *claudeProc) {
	a.setScopedApprovalStatus(p, nil)
}

func (a *claudeAgent) setScopedApprovalStatus(p *claudeProc, hook *claudeElicitationLease) {
	if p == nil {
		return
	}
	var permissionDone, hookDone <-chan struct{}
	if p.permissionLease != nil {
		permissionDone = p.permissionLease.done
	}
	if p.elicitationHook != nil {
		hookDone = p.elicitationHook.done
	}
	// Bounded retries: concurrent registry mutations have their own callbacks.
	for attempt := 0; attempt < 4; attempt++ {
		snapshot := a.aps.statusSnapshot("claude:"+p.id, permissionDone, hookDone)
		if a.commitApprovalStatus(p, hook, snapshot) {
			return
		}
	}
}

// false means only that a newer registry snapshot is needed. All other stale
// worker/lease cases are final no-ops, not permission to target the next worker.
func (a *claudeAgent) commitApprovalStatus(p *claudeProc, hook *claudeElicitationLease, snapshot approvalStatusSnapshot) bool {
	a.mu.Lock()
	if a.closed || a.procs[p.id] != p || hook != nil && (p.elicitationHook != hook || a.elicitationHooks[p.id] != hook || !approvalSourceOpen(hook.done)) {
		a.mu.Unlock()
		return true
	}
	p.mu.Lock()
	if p.stopped || p.transportRetired() || p.permissionLease == nil || !approvalSourceOpen(p.permissionLease.done) {
		p.mu.Unlock()
		a.mu.Unlock()
		return true
	}
	if a.aps.revision.Load() != snapshot.revision {
		p.mu.Unlock()
		a.mu.Unlock()
		return false
	}
	status, turnID := "idle", ""
	if len(p.turnIDs) > 0 {
		turnID = p.turnIDs[0]
	}
	if p.turns > 0 || turnID != "" || p.activeSubtasks.Load() {
		status = "running"
	}
	if snapshot.pending(time.Now()) > 0 {
		status = "waiting_approval"
	}
	info, exists := a.live[p.id]
	if exists && info.Status == status && info.ActiveTurnID == turnID {
		p.mu.Unlock()
		a.mu.Unlock()
		return true
	}
	if !exists {
		info = protocol.SessionInfo{SessionKey: "claude:" + p.id, Tool: "claude", Client: "Claude Code", Cwd: p.cwd, Model: p.model}
	}
	info.Status, info.ActiveTurnID, info.Controllable = status, turnID, true
	info = a.commitLiveStateLocked(p.id, info)
	p.mu.Unlock()
	a.mu.Unlock()
	a.emit(protocol.Event{SessionKey: info.SessionKey, TS: info.UpdatedAt, Type: "session.updated", Session: ptrInfo(info)})
	return true
}
