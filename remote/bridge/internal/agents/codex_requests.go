package agents

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"salcara/bridge/internal/protocol"
)

type cxApprovalParams struct {
	ThreadID               string          `json:"threadId"`
	TurnID                 string          `json:"turnId"`
	ItemID                 string          `json:"itemId"`
	Reason                 *string         `json:"reason"`
	Command                *string         `json:"command"`
	Cwd                    *string         `json:"cwd"`
	CommandActions         []cxAction      `json:"commandActions"`
	Kind                   string          `json:"kind"`
	GrantRoot              *string         `json:"grantRoot"`
	Permissions            json.RawMessage `json:"permissions"`
	AutoResolutionMS       *uint64         `json:"autoResolutionMs"`
	AvailableDecisions     json.RawMessage `json:"availableDecisions"`
	NetworkApprovalContext *struct {
		Host     string `json:"host"`
		Protocol string `json:"protocol"`
	} `json:"networkApprovalContext"`
	AdditionalPermissions json.RawMessage `json:"additionalPermissions"`
	EnvironmentID         string          `json:"environmentId"`
}

// Only simple decisions we can faithfully encode are advertised. Structured
// policy amendments remain native-only; an unknown list never grants access.
func cxRequestOptions(raw json.RawMessage) *protocol.ApprovalOptions {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return &protocol.ApprovalOptions{Version: 1, Decisions: []string{"allow", "allow_session", "deny"}}
	}
	out := &protocol.ApprovalOptions{Version: 1, Decisions: []string{}}
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil {
		return out
	}
	canDecline, canCancel := false, false
	for _, value := range values {
		var decision string
		if json.Unmarshal(value, &decision) != nil {
			continue
		}
		switch decision {
		case "accept":
			out.Decisions = append(out.Decisions, "allow")
		case "acceptForSession":
			out.Decisions = append(out.Decisions, "allow_session")
		case "decline", "cancel":
			out.Decisions = append(out.Decisions, "deny")
			canDecline = canDecline || decision == "decline"
			canCancel = canCancel || decision == "cancel"
		}
	}
	if canCancel && !canDecline {
		out.DenyEffect = "cancel_turn"
	}
	return cloneApprovalOptions(out)
}

func cxDecisionReply(c *rpcConn, id, available json.RawMessage, decision string) {
	mapped := map[string]string{"allow": "accept", "allow_session": "acceptForSession", "deny": "decline"}[decision]
	if len(available) != 0 && strings.TrimSpace(string(available)) != "null" {
		var values []json.RawMessage
		_ = json.Unmarshal(available, &values)
		allowed, canCancel := false, false
		for _, value := range values {
			var candidate string
			_ = json.Unmarshal(value, &candidate)
			allowed = allowed || candidate == mapped
			canCancel = canCancel || candidate == "cancel"
		}
		if !allowed {
			if decision == "deny" && canCancel {
				mapped = "cancel"
			} else {
				_ = c.ReplyError(id, -32602, "approval decision not supported")
				return
			}
		}
	}
	_ = c.Reply(id, map[string]any{"decision": mapped})
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// onRequest answers server→client requests. It runs on its own goroutine.
func (a *codexAgent) onRequest(c *rpcConn, id json.RawMessage, method string, params json.RawMessage) {
	var p cxApprovalParams
	parseErr := json.Unmarshal(params, &p)
	if parseErr != nil || strings.TrimSpace(p.ThreadID) == "" {
		_ = c.ReplyError(id, -32602, "invalid approval request")
		return
	}
	sk := "codex:" + p.ThreadID

	a.mu.Lock()
	// During initialization a.conn has not been published yet; the callback
	// still owns this live connection's immutable done channel. Once a current
	// connection exists, a delayed callback from another one is retired.
	if a.closed || c == nil || !c.alive() || a.conn != nil && a.conn != c {
		a.mu.Unlock()
		return
	}
	t := a.threadLocked(p.ThreadID)
	policy := t.policy
	if policy == "" {
		policy = normalizePolicy(a.settings().Approval)
	}
	cwd := t.info.Cwd
	var changes []cxChange
	if l := t.items[p.ItemID]; l != nil {
		changes = l.changes
	}
	a.mu.Unlock()
	options := cxRequestOptions(p.AvailableDecisions)
	decorate := func(ev protocol.Event) protocol.Event {
		ev.TurnID, ev.ParentID = p.TurnID, p.ItemID
		ev.ApprovalOptions = options
		if ev.Kind == "question" {
			ev.ApprovalOptions = &protocol.ApprovalOptions{Version: 1, Decisions: []string{"allow", "deny"}}
		}
		return ev
	}

	switch method {
	case "item/commandExecution/requestApproval":
		if len(options.Decisions) == 0 {
			_ = c.ReplyError(id, -32602, "approval decisions not supported")
			return
		}
		if policy == "auto_all" && approvalDecisionAllowed(options, "allow") {
			_ = c.Reply(id, map[string]any{"decision": "accept"})
			return
		}
		cmd := deref(p.Command)
		title := "运行 " + cmd
		if p.Kind == "writeStdin" {
			title = "向命令输入 " + cmd
		}
		if cmd == "" {
			title = "运行命令"
		}
		ev := protocol.Event{Kind: "command", Title: titleText(title), Detail: truncHead(strings.TrimSpace(cmd+"\n\n"+deref(p.Reason)), maxDetailChars), Cwd: deref(p.Cwd)}
		if network := p.NetworkApprovalContext; network != nil {
			// This is a managed network destination, not necessarily a shell command.
			ev.Kind, ev.Title = "permission", titleText("访问网络 · "+network.Host)
			ev.Detail = truncHead(strings.TrimSpace(network.Protocol+" · "+network.Host+"\n"+cmd+"\n"+deref(p.Reason)), maxDetailChars)
		}
		if len(p.AdditionalPermissions) > 0 && strings.TrimSpace(string(p.AdditionalPermissions)) != "null" {
			ev.Detail = truncHead("请求权限："+jsonCompact(p.AdditionalPermissions)+"\n"+ev.Detail, maxDetailChars)
		}
		if p.EnvironmentID != "" {
			ev.Detail = truncHead("环境："+truncHead(p.EnvironmentID, 512)+"\n"+ev.Detail, maxDetailChars)
		}
		ans, ok := a.askPhone(sk, string(id), decorate(ev), c.done)
		if !ok {
			return
		}
		cxDecisionReply(c, id, p.AvailableDecisions, ans.decision)

	case "item/fileChange/requestApproval":
		if len(options.Decisions) == 0 {
			_ = c.ReplyError(id, -32602, "approval decisions not supported")
			return
		}
		if (policy == "auto_all" || policy == "auto_edits") && approvalDecisionAllowed(options, "allow") {
			_ = c.Reply(id, map[string]any{"decision": "accept"})
			return
		}
		ev := protocol.Event{Kind: "file_change", Title: codexChangesTitle(changes, cwd), Diff: codexChangesDiff(changes), Cwd: cwd}
		detail := deref(p.Reason)
		if p.GrantRoot != nil {
			detail = strings.TrimSpace(detail + "\n申请写入：" + *p.GrantRoot)
		}
		ev.Detail = truncHead(detail, maxDetailChars)
		ans, ok := a.askPhone(sk, string(id), decorate(ev), c.done)
		if !ok {
			return
		}
		cxDecisionReply(c, id, p.AvailableDecisions, ans.decision)

	case "item/permissions/requestApproval":
		granted := grantedPermissions(p.Permissions)
		if policy == "auto_all" {
			_ = c.Reply(id, map[string]any{"permissions": granted, "scope": "turn"})
			return
		}
		ev := protocol.Event{Kind: "permission", Title: "申请额外权限", Cwd: deref(p.Cwd),
			Detail: truncHead(strings.TrimSpace(deref(p.Reason)+"\n"+jsonCompact(p.Permissions)), maxDetailChars)}
		ans, ok := a.askPhone(sk, string(id), decorate(ev), c.done)
		if !ok {
			return
		}
		switch ans.decision {
		case "allow":
			_ = c.Reply(id, map[string]any{"permissions": granted, "scope": "turn"})
		case "allow_session":
			_ = c.Reply(id, map[string]any{"permissions": granted, "scope": "session"})
		default:
			_ = c.Reply(id, map[string]any{"permissions": map[string]any{}, "scope": "turn"})
		}

	case "item/tool/requestUserInput":
		qs, err := parseToolQuestions(params, "codex")
		if err != nil || p.AutoResolutionMS != nil && *p.AutoResolutionMS == 0 {
			if err != nil {
				a.emit(unsupportedQuestionNotice(sk, "Codex"))
			}
			_ = c.Reply(id, map[string]any{"answers": map[string]any{}})
			return
		}
		ev := questionEvent("codex", qs, "需要你的回答", "", cwd)
		if p.AutoResolutionMS != nil && *p.AutoResolutionMS < uint64(approvalTimeout.Milliseconds()) {
			ev.ExpiresAt = time.Now().Add(time.Duration(*p.AutoResolutionMS) * time.Millisecond).UnixMilli()
		}
		ans, ok := a.askPhoneRequest(sk, string(id), decorate(ev), func(answers map[string][]string) error { return validateQuestionAnswers(qs, answers) }, c.done)
		if !ok {
			return
		}
		answers := map[string]any{}
		if ans.decision == "allow" {
			for questionID, values := range ans.answers {
				answers[questionID] = map[string]any{"answers": values}
			}
		}
		_ = c.Reply(id, map[string]any{"answers": answers})

	case "mcpServer/elicitation/request":
		var request struct {
			Mode       string          `json:"mode"`
			Message    string          `json:"message"`
			ServerName string          `json:"serverName"`
			URL        string          `json:"url"`
			Schema     json.RawMessage `json:"requestedSchema"`
		}
		err := json.Unmarshal(params, &request)
		form, formErr := parseElicitationForm(request.Schema)
		if err != nil || request.Mode != "" && request.Mode != "form" && request.Mode != "openai/form" || request.URL != "" || formErr != nil || !boundedQuestionText(request.Message, maxQuestionText) || secretFormField(request.Message) || strings.TrimSpace(request.ServerName) == "" || !boundedQuestionText(request.ServerName, 256) {
			a.emit(unsupportedQuestionNotice(sk, "MCP"))
			_ = c.Reply(id, map[string]any{"action": "decline", "content": nil, "_meta": nil})
			return
		}
		ev := questionEvent("mcp-form", form.questions, "MCP · "+request.ServerName, request.Message, cwd)
		ans, ok := a.askPhoneRequest(sk, string(id), decorate(ev), form.validate, c.done)
		if !ok {
			return
		}
		action := "decline"
		var content map[string]any
		if ans.decision == "allow" {
			content, err = form.content(ans.answers)
			if err == nil {
				action = "accept"
			}
		}
		_ = c.Reply(id, map[string]any{"action": action, "content": content, "_meta": nil})

	case "execCommandApproval", "applyPatchApproval": // legacy v1 API, not used by v2 threads
		_ = c.Reply(id, map[string]any{"decision": "denied"})

	default:
		_ = c.ReplyError(id, -32601, "not supported by Salcara bridge: "+method)
	}
}

// grantedPermissions echoes the requested permission profile without null fields.
func grantedPermissions(raw json.RawMessage) map[string]any {
	var req map[string]any
	_ = json.Unmarshal(raw, &req)
	out := map[string]any{}
	for k, v := range req {
		if v != nil {
			out[k] = v
		}
	}
	return out
}

// askPhone raises approval.request and waits. ok=false means the tool withdrew the request (no reply).
func (a *codexAgent) askPhone(sk, ref string, ev protocol.Event, sourceClosed <-chan struct{}) (approvalAnswer, bool) {
	return a.askPhoneRequest(sk, ref, ev, nil, sourceClosed)
}

func (a *codexAgent) askPhoneRequest(sk, ref string, ev protocol.Event, validate func(map[string][]string) error, sourceClosed <-chan struct{}) (approvalAnswer, bool) {
	p, ev := a.aps.addValidatedRequestForSource(sk, "codex", ref, ev, validate, sourceClosed)
	if p == nil {
		return approvalAnswer{decision: "deny", noReply: true}, false
	}
	a.emit(ev)
	a.setStatus(sk, "waiting_approval")
	stop := a.closeCh
	finished := make(chan struct{})
	if sourceClosed != nil {
		merged := make(chan struct{})
		stop = merged
		go func() {
			select {
			case <-a.closeCh:
			case <-sourceClosed:
			case <-finished:
				return
			}
			close(merged)
		}()
	}
	ans := a.aps.wait(p, stop)
	close(finished)
	if sourceClosed != nil {
		select {
		case <-sourceClosed:
			ans.noReply = true
		default:
		}
	}
	a.emit(protocol.Event{SessionKey: sk, Type: "approval.resolved", ApprovalID: p.id, Decision: ans.decision, By: ans.by})
	if a.aps.countFor(sk) == 0 {
		a.setStatus(sk, "running")
	}
	return ans, !ans.noReply
}

func (a *codexAgent) setStatus(sk, status string) {
	id := strings.TrimPrefix(sk, "codex:")
	a.mu.Lock()
	t := a.threadLocked(id)
	if status == "running" && t.turnID == "" {
		status = "idle"
	}
	if t.info.Status == status {
		a.mu.Unlock()
		return
	}
	t.info.Status = status
	t.info.UpdatedAt = nowMs()
	info := t.info
	a.mu.Unlock()
	a.emit(protocol.Event{SessionKey: sk, Type: "session.updated", Session: ptrInfo(info)})
}

// watch polls thread/list so threads run by the Codex CLI / IDE / app show up and update on the phone.
// For external threads the phone has opened, new history items are streamed as events.
func (a *codexAgent) watch(ctx context.Context, interval time.Duration) {
	prev := map[string]protocol.SessionInfo{}
	first := true
	for {
		if _, ok := a.exe(); ok {
			a.pollOnce(ctx, prev, first)
			first = false
		}
		wait := interval
		if a.watchingExternalRun() && wait > fastWatch {
			// A phone is looking at a thread that Codex Desktop / CLI is running:
			// follow it closely so progress appears on the phone within seconds.
			wait = fastWatch
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-a.closeCh:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

const fastWatch = 2 * time.Second

func (a *codexAgent) watchingExternalRun() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range a.threads {
		if t.opened && !t.loaded && (t.info.Status == "running" || t.info.Status == "waiting_approval") {
			return true
		}
	}
	return false
}

func (a *codexAgent) pollOnce(ctx context.Context, prev map[string]protocol.SessionInfo, first bool) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	list, err := a.Sessions(cctx)
	if err != nil {
		return
	}
	for _, info := range list {
		old, known := prev[info.SessionKey]
		prev[info.SessionKey] = info
		if first {
			continue
		}
		id := strings.TrimPrefix(info.SessionKey, "codex:")
		a.mu.Lock()
		t := a.threads[id]
		loaded, opened := false, false
		if t != nil {
			loaded, opened = t.loaded, t.opened
		}
		a.mu.Unlock()
		changed := !known || old.UpdatedAt != info.UpdatedAt || old.Status != info.Status ||
			old.Title != info.Title || old.Controllable != info.Controllable
		if !changed {
			continue
		}
		if !loaded {
			// Live events for loaded threads come straight from the app-server.
			a.emit(protocol.Event{SessionKey: info.SessionKey, Type: "session.updated", Session: ptrInfo(info)})
			if opened && known && (old.UpdatedAt != info.UpdatedAt || old.Status != info.Status) {
				a.streamNewItems(cctx, id)
			}
		}
	}
}

func (a *codexAgent) streamNewItems(ctx context.Context, id string) {
	a.mu.Lock()
	item := a.threadLocked(id)
	tail, baseline := item.tailHistory, item.tailBaselineTurn
	version := item.historyReadVersion
	if !tail {
		item.historyReadVersion++
		version = item.historyReadVersion
	}
	a.mu.Unlock()
	if tail {
		a.streamTailItems(ctx, id, baseline)
		return
	}
	th, err := a.readThread(ctx, id)
	if err != nil {
		return
	}
	sk := "codex:" + id
	th.Turns = codexHistoryWindow(th.Turns)
	itemDigests, turnDigests, bounded := codexHistoryDigests(th.Turns)
	a.mu.Lock()
	t := a.threadLocked(id)
	if t.tailHistory || t.historyReadVersion != version {
		a.mu.Unlock()
		return
	}
	if !bounded {
		t.historyItemDigests, t.historyTurnDigests = itemDigests, turnDigests
		a.mu.Unlock()
		a.emit(protocol.Event{SessionKey: sk, Type: "notice", ID: "history-gap:" + sk, TS: nowMs(), Level: "warn", Text: "[salcara:history-gap:v1] 正在重新读取电脑上的原会话。"})
		return
	}
	changed := codexChangedHistory(th, t.historyItemDigests, t.historyTurnDigests, turnDigests)
	t.historyItemDigests, t.historyTurnDigests = itemDigests, turnDigests
	a.mu.Unlock()
	for _, e := range a.historyEvents(sk, changed) {
		a.emit(e)
	}
}
