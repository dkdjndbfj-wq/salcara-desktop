package agents

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"salcara/bridge/internal/protocol"
)

var errCodexPermissionMode = errors.New("permission_mode_unsupported: 当前 Agent 不支持这个权限模式")
var errCodexPermissionUnconfirmed = errors.New("permission_mode_unconfirmed: Codex 未确认所选权限，消息未发送；请检查电脑版本或管理策略")

func validCodexPermissionMode(mode string) bool {
	return mode == "ask" || mode == "auto_review" || mode == "full_access"
}

func (a *codexAgent) KnownSessionPermission(id string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t := a.threads[id]; t != nil {
		return t.info.PermissionMode
	}
	return ""
}

// A legacy/native resume can expose a dangerous sandbox without enough
// reviewer metadata to label a complete permission mode. Admission still
// needs desktop consent; an absent label is not evidence of a safe sandbox.
func (a *codexAgent) SessionRequiresFullAccessConsent(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	t := a.threads[id]
	return t != nil && (t.info.PermissionMode == "full_access" || t.inheritedFullAccess)
}

// These fields follow the generated app-server ThreadStart/Resume schema.
// A reviewer is not an approval policy. auto_review keeps the workspace sandbox
// and delegates risk assessment to Codex, not to Salcara's legacy auto_all.
func codexPermissionParams(params map[string]any, mode string) {
	params["approvalPolicy"], params["sandbox"], params["approvalsReviewer"] = "on-request", "workspace-write", "user"
	if mode == "auto_review" {
		params["approvalsReviewer"] = "auto_review"
	} else if mode == "full_access" {
		params["approvalPolicy"], params["sandbox"] = "never", "danger-full-access"
	}
}

// Native policies also include a granular object. Decode that policy without
// assigning it one of the three UI modes: preserving an inherited native policy
// is safe, while claiming it confirms an explicit mode would be misleading.
type codexApprovalPolicy string

func (p *codexApprovalPolicy) UnmarshalJSON(data []byte) error {
	var policy string
	if err := json.Unmarshal(data, &policy); err == nil {
		*p = codexApprovalPolicy(policy)
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || len(object) != 1 {
		return errors.New("invalid Codex approval policy")
	}
	var granular map[string]*bool
	if err := json.Unmarshal(object["granular"], &granular); err != nil || granular == nil {
		return errors.New("invalid Codex granular approval policy")
	}
	for _, required := range []string{"rules", "mcp_elicitations", "sandbox_approval"} {
		if granular[required] == nil {
			return errors.New("invalid Codex granular approval policy")
		}
	}
	for _, value := range granular {
		if value == nil {
			return errors.New("invalid Codex granular approval policy")
		}
	}
	*p = "granular"
	return nil
}

type codexPermissionReply struct {
	ApprovalPolicy    codexApprovalPolicy `json:"approvalPolicy"`
	ApprovalsReviewer string              `json:"approvalsReviewer"`
	Sandbox           struct {
		Type string `json:"type"`
	} `json:"sandbox"`
}

func (r codexPermissionReply) matches(mode string) bool {
	if mode == "full_access" {
		return r.ApprovalPolicy == "never" && r.ApprovalsReviewer == "user" && r.Sandbox.Type == "dangerFullAccess"
	}
	reviewer := "user"
	if mode == "auto_review" {
		reviewer = "auto_review"
	}
	return validCodexPermissionMode(mode) && r.ApprovalPolicy == "on-request" && r.ApprovalsReviewer == reviewer && r.Sandbox.Type == "workspaceWrite"
}

type codexPermissionAcknowledgement struct {
	Thread cxThread `json:"thread"`
	codexPermissionReply
}

// Recent app-servers ignore legacy resume overrides for an already loaded
// thread. Use the explicit settings RPC, then read its effective tuple before
// allowing input. An empty update acknowledgement alone grants nothing.
func codexPermissionUpdateParams(id, mode string) map[string]any {
	params := map[string]any{"threadId": id}
	codexPermissionParams(params, mode)
	delete(params, "sandbox")
	policy := map[string]any{"type": "workspaceWrite", "writableRoots": []string{}, "networkAccess": false,
		"excludeTmpdirEnvVar": false, "excludeSlashTmp": false}
	if mode == "full_access" {
		policy = map[string]any{"type": "dangerFullAccess"}
	}
	params["sandboxPolicy"] = policy
	return params
}

func updateCodexPermission(ctx context.Context, c *rpcConn, id, mode string) (codexPermissionAcknowledgement, error) {
	var reply codexPermissionAcknowledgement
	if !validChildID(id) || !validCodexPermissionMode(mode) {
		return reply, errCodexPermissionUnconfirmed
	}
	if c.Call(ctx, "thread/settings/update", codexPermissionUpdateParams(id, mode), nil) != nil ||
		c.Call(ctx, "thread/resume", map[string]any{"threadId": id}, &reply) != nil ||
		reply.Thread.ID != id || reply.Thread.Status.Type != "idle" || codexThreadInProgress(reply.Thread) || !reply.matches(mode) {
		return reply, errCodexPermissionUnconfirmed
	}
	return reply, nil
}

// A new thread has no rollout until its first input, so thread/resume cannot
// read it back. The native settings notification is the effective tuple for
// this case. Subscribe before updating so either response ordering is safe.
func updateNewCodexPermission(ctx context.Context, c *rpcConn, id, mode string) (codexPermissionReply, error) {
	var empty codexPermissionReply
	if !validChildID(id) || !validCodexPermissionMode(mode) {
		return empty, errCodexPermissionUnconfirmed
	}
	observed := make(chan codexPermissionReply, 1)
	c.mu.Lock()
	if c.closed || c.permissionObservers[id] != nil {
		c.mu.Unlock()
		return empty, errCodexPermissionUnconfirmed
	}
	if c.permissionObservers == nil {
		c.permissionObservers = make(map[string]chan codexPermissionReply)
	}
	c.permissionObservers[id] = observed
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.permissionObservers[id] == observed {
			delete(c.permissionObservers, id)
		}
		c.mu.Unlock()
	}()
	if c.Call(ctx, "thread/settings/update", codexPermissionUpdateParams(id, mode), nil) != nil {
		return empty, errCodexPermissionUnconfirmed
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case reply := <-observed:
			if reply.matches(mode) {
				return reply, nil
			}
		case <-ctx.Done():
			return empty, errCodexPermissionUnconfirmed
		case <-c.done:
			return empty, errCodexPermissionUnconfirmed
		case <-deadline.C:
			return empty, errCodexPermissionUnconfirmed
		}
	}
}

// The observation is connection-local and keyed to the exact new thread.
// Reader callbacks never block; a later native tuple replaces an older one.
func (c *rpcConn) observeCodexPermission(params json.RawMessage) {
	var note struct {
		ThreadID string `json:"threadId"`
		Settings struct {
			codexPermissionReply
			SandboxPolicy struct {
				Type string `json:"type"`
			} `json:"sandboxPolicy"`
		} `json:"threadSettings"`
	}
	if json.Unmarshal(params, &note) != nil {
		return
	}
	reply := note.Settings.codexPermissionReply
	reply.Sandbox.Type = note.Settings.SandboxPolicy.Type
	c.mu.Lock()
	defer c.mu.Unlock()
	if observed := c.permissionObservers[note.ThreadID]; !c.closed && observed != nil {
		select {
		case <-observed:
		default:
		}
		observed <- reply
	}
}

// Caller holds controlMu. Resume changes only this thread, supplies no input,
// and reports effective settings. This explicit acknowledgement prevents old
// runtimes silently ignoring newer reviewer fields on turn/start. No fallback
// to a broader policy, no global config write and no automatic retry of input.
func (a *codexAgent) applyCodexPermission(ctx context.Context, c *rpcConn, t *codexThread, opts TurnOptions) error {
	if !validCodexPermissionMode(opts.PermissionMode) {
		return errCodexPermissionMode
	}
	a.mu.Lock()
	busy := a.conn != c || t.statusUnconfirmed || t.turnID != "" || t.info.ActiveTurnID != "" || t.info.Status == "running" || t.info.Status == "waiting_approval" || t.childOverflow
	for _, status := range t.childStates {
		if status != "completed" && status != "failed" && status != "shutdown" {
			busy = true
		}
	}
	id, version, model := t.id, t.turnVersion, t.info.Model
	alreadyConfirmed := t.loaded && !t.permissionUnconfirmed && t.info.PermissionMode == opts.PermissionMode
	a.mu.Unlock()
	if busy || a.aps.countFor("codex:"+id) != 0 {
		return errors.New("permission_mode_busy: 请在当前操作结束后更改权限")
	}
	if alreadyConfirmed {
		return nil
	}
	params := withProvider(map[string]any{"threadId": id}, settingsForContext(ctx, a.settings))
	codexPermissionParams(params, opts.PermissionMode)
	if opts.Model != "" {
		model = opts.Model
	}
	if model != "" {
		params["model"] = model
	}
	// Stop any legacy local blanket-approval before asking the runtime to
	// change reviewer. A failed switch may never keep that broader shortcut.
	a.mu.Lock()
	t.policy = "ask"
	a.mu.Unlock()
	var reply struct {
		Thread cxThread `json:"thread"`
		codexPermissionReply
	}
	if err := c.Call(ctx, "thread/resume", params, &reply); err != nil {
		a.invalidateCodexPermission(t)
		return err
	}
	if reply.Thread.ID == id && reply.Thread.Status.Type == "idle" && !codexThreadInProgress(reply.Thread) && !reply.codexPermissionReply.matches(opts.PermissionMode) {
		updated, err := updateCodexPermission(ctx, c, id, opts.PermissionMode)
		if err == nil {
			reply.Thread, reply.codexPermissionReply = updated.Thread, updated.codexPermissionReply
		}
	}
	if reply.Thread.ID != id || reply.Thread.Status.Type != "idle" || codexThreadInProgress(reply.Thread) || !reply.codexPermissionReply.matches(opts.PermissionMode) {
		// The host may have applied only part of a request. Do not claim either
		// the old or new mode is confirmed after an ambiguous acknowledgement.
		a.invalidateCodexPermission(t)
		return errCodexPermissionUnconfirmed
	}
	a.mu.Lock()
	if a.conn != c || t.turnVersion != version || t.turnID != "" {
		a.mu.Unlock()
		a.invalidateCodexPermission(t)
		return errCodexPermissionUnconfirmed
	}
	t.policy, t.info.PermissionMode = "ask", opts.PermissionMode
	t.inheritedFullAccess = opts.PermissionMode == "full_access"
	t.legacyPolicyConfigured = false
	t.permissionUnconfirmed = false
	info := t.info
	a.mu.Unlock()
	a.emit(protocol.Event{SessionKey: info.SessionKey, Type: "session.updated", Session: ptrInfo(info)})
	return nil
}

func (a *codexAgent) invalidateCodexPermission(t *codexThread) {
	a.mu.Lock()
	t.info.PermissionMode, t.permissionUnconfirmed = "", true
	info := t.info
	a.mu.Unlock()
	a.emit(protocol.Event{SessionKey: info.SessionKey, Type: "session.updated", Session: ptrInfo(info)})
}
