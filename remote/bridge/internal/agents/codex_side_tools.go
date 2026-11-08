package agents

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"salcara/bridge/internal/protocol"
)

const sideToolLease = 45 * time.Second
const maxSideToolSessions = 8
const maxSideToolTombstones = 128
const maxSideToolStateBytes = 240 << 10

var errSideToolUnavailable = errors.New("side_tools_unavailable: 当前电脑未确认支持独立工具侧聊")
var errSideToolClosed = errors.New("side_tools_closed: 临时工具侧聊已关闭或来源发生变化")

func sideToolText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	i := limit - len("…")
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i] + "…"
}

type sideToolWorkerFactory func(context.Context, Settings, string, Sink) (*codexAgent, error)
type codexSideToolsRegistry struct {
	mu            sync.Mutex
	sessions      map[string]*codexSideToolSession
	closed        bool
	workerFactory sideToolWorkerFactory // tests only; production creates a private process
}
type sideToolReceipt struct {
	hash      string
	attempted bool
}
type codexSideToolSession struct {
	mu                          sync.Mutex
	opMu                        sync.Mutex
	state                       protocol.SideQuestionToolState
	parentID, owner, cwd        string
	binding                     sideQuestionBinding
	snapshot                    SideQuestionSnapshot
	settings                    Settings
	guard                       func() error // immutable source guard, independent of request cancellation
	executionGuard              func() error // includes workspace admission; only while occupied
	ctx                         context.Context
	cancel                      context.CancelFunc
	done                        chan struct{}
	worker                      *codexAgent
	workerConn                  *rpcConn // retained after worker.Close, until OS exit is proven
	home, threadID              string
	receipts                    map[string]sideToolReceipt
	pendingSend                 string
	turns                       int
	closed, uncertain, starting bool
}

func sideToolSamePath(a, b string) bool {
	if !filepath.IsAbs(a) || !filepath.IsAbs(b) {
		return false
	}
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
func (a *codexAgent) sideToolOptions(id string, connection codexSideQuestionConnection) *protocol.SideQuestionToolOptions {
	c, ok := connection.binding.Runtime.(*rpcConn)
	if !ok || c == nil || connection.binding.Source == "" {
		return nil
	}
	if _, ok = c.sessionActionRuntime(); !ok {
		return nil
	}
	a.mu.Lock()
	t := a.threads[id]
	ready := !a.closed && a.privateHome == "" && a.conn == c && t != nil && !t.info.Archived && filepath.IsAbs(t.info.Cwd)
	a.mu.Unlock()
	if !ready {
		return nil
	}
	return &protocol.SideQuestionToolOptions{Version: 1, Permissions: []string{"workspace-write"}, Features: true}
}
func (r *codexSideToolsRegistry) snapshot() []*codexSideToolSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	sessions := make([]*codexSideToolSession, 0, len(r.sessions))
	for _, s := range r.sessions {
		sessions = append(sessions, s)
	}
	return sessions
}
func (s *codexSideToolSession) occupiedLocked() bool {
	if s.closed {
		return s.starting || s.workerConn != nil && !s.workerConn.processExitConfirmed()
	}
	if s.starting || s.state.State == "running" || s.state.State == "waiting_approval" {
		return true
	}
	return s.uncertain && s.workerConn != nil && !s.workerConn.processExitConfirmed()
}
func (a *codexAgent) SideQuestionToolWorkspaces() []protocol.SessionInfo {
	result := []protocol.SessionInfo{}
	for _, s := range a.sideTools.snapshot() {
		s.mu.Lock()
		if s.occupiedLocked() {
			result = append(result, protocol.SessionInfo{SessionKey: "side-tools:" + s.owner + ":" + s.state.ChainID, Cwd: s.cwd, Status: "running", Tool: "codex", Title: "临时工具侧聊"})
		}
		s.mu.Unlock()
	}
	return result
}
func (r *codexSideToolsRegistry) activeTurns() bool {
	for _, s := range r.snapshot() {
		s.mu.Lock()
		busy := s.occupiedLocked()
		s.mu.Unlock()
		if busy {
			return true
		}
	}
	return false
}
func (r *codexSideToolsRegistry) closeAll() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	var wait sync.WaitGroup
	for _, s := range r.snapshot() {
		wait.Add(1)
		go func(s *codexSideToolSession) { defer wait.Done(); s.close("临时工具侧聊已关闭") }(s)
	}
	wait.Wait()
}
func (s *codexSideToolSession) stopWorker() {
	s.mu.Lock()
	worker := s.worker
	s.mu.Unlock()
	if worker == nil {
		return
	}
	worker.mu.Lock()
	c := worker.conn
	if c == nil {
		c = worker.unconfirmedProcess
	}
	if c == nil && worker.starting != nil {
		c = worker.starting.conn
	}
	worker.mu.Unlock()
	s.mu.Lock()
	if c != nil {
		s.workerConn = c
	}
	s.mu.Unlock()
	worker.Close()
}
func (s *codexSideToolSession) close(reason string) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	// Keep startup reservation until its owner publishes any surviving worker.
	s.state.State, s.state.Error, s.state.CanSend = "closed", sideToolText(reason, 4096), false
	s.state.Sequence++
	s.cancel()
	close(s.done)
	s.mu.Unlock()
	s.stopWorker()
	s.opMu.Lock()
	s.stopWorker()
	s.mu.Lock()
	home, conn := s.home, s.workerConn
	s.starting = false
	stopped := conn == nil || conn.processExitConfirmed()
	if stopped {
		s.home = ""
	}
	s.mu.Unlock()
	s.opMu.Unlock()
	if stopped && home != "" {
		removeSideToolHome(home)
	}
}
func removeSideToolHome(home string) {
	root, err := filepath.Abs(os.TempDir())
	if err != nil {
		return
	}
	path, err := filepath.Abs(home)
	if err != nil || !sideToolSamePath(filepath.Dir(path), root) || !strings.HasPrefix(filepath.Base(path), "salcara-side-tools-") {
		return
	}
	st, err := os.Lstat(path)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return
	}
	_ = os.RemoveAll(path)
}
func (s *codexSideToolSession) watch() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-s.ctx.Done():
			s.close("临时工具侧聊已关闭")
			return
		case <-ticker.C:
			if s.checkExecution() != nil {
				s.close("连接已过期，或 API、电脑、项目、授权已变化；仅停止此临时任务")
				return
			}
		}
	}
}
func (s *codexSideToolSession) check() error {
	if s.ctx.Err() != nil {
		return errSideToolClosed
	}
	if err := s.guard(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || time.Now().UnixMilli() >= s.state.ExpiresAt {
		return errSideToolClosed
	}
	return nil
}
func (s *codexSideToolSession) checkExecution() error {
	if err := s.check(); err != nil {
		return err
	}
	s.mu.Lock()
	guard, busy := s.executionGuard, s.occupiedLocked()
	s.mu.Unlock()
	if busy && guard != nil {
		return guard()
	}
	return nil
}
func cloneSideToolState(v protocol.SideQuestionToolState) protocol.SideQuestionToolState {
	if v.Features != nil {
		features := *v.Features
		features.CollaborationModes = append([]string{}, features.CollaborationModes...)
		features.Skills = append([]protocol.Skill{}, features.Skills...)
		v.Features = &features
	}
	v.Events = append([]protocol.Event{}, v.Events...)
	for i := range v.Events {
		v.Events[i] = cloneProtocolEvent(v.Events[i])
	}
	v.Approvals = append([]protocol.Event{}, v.Approvals...)
	for i := range v.Approvals {
		v.Approvals[i] = cloneProtocolEvent(v.Approvals[i])
		v.Approvals[i].ApprovalOptions = cloneApprovalOptions(v.Approvals[i].ApprovalOptions)
	}
	return v
}
func sideToolApproval(ev protocol.Event, parentID string) protocol.Event {
	ev = cloneProtocolEvent(ev)
	ev.SessionKey, ev.Session = "codex:"+parentID, nil
	ev.Tool = "codex"
	ev.ImageData, ev.Attachments, ev.ContextReferences, ev.Skills = nil, nil, nil, nil
	ev.ChildSessionKeys, ev.MessageOrigin, ev.Context, ev.Browser, ev.Computer = nil, nil, nil, nil, nil
	options := &protocol.ApprovalOptions{Version: 1, Decisions: []string{"deny"}}
	if ev.ApprovalOptions != nil {
		options.DenyEffect = ev.ApprovalOptions.DenyEffect
	}
	raw, _ := json.Marshal(ev)
	// Truncated commands/diffs are not sufficient evidence for authorization.
	if ev.Kind == "question" || len(raw) > 8192 || strings.HasSuffix(ev.Detail, "…") || strings.HasSuffix(ev.Diff, "…") {
		// A question has no text-input contract in this panel. Expose its safe
		// denial action using the generic tool approval presentation.
		ev.Kind = "tool"
		ev.Title, ev.Detail, ev.Diff, ev.Text, ev.Output, ev.Error = "审批内容无法完整展示", "此请求仅可拒绝；请关闭临时任务后在电脑中处理。", "", "", "", ""
		ev.Questions = nil
	} else if approvalDecisionAllowed(ev.ApprovalOptions, "allow") {
		options.Decisions = []string{"allow", "deny"}
	}
	ev.ApprovalOptions = options
	return ev
}
func (s *codexSideToolSession) boundStateLocked() {
	for {
		raw, err := json.Marshal(s.state)
		if err == nil && len(raw) <= maxSideToolStateBytes {
			return
		}
		s.state.Truncated = true
		if len(s.state.Events) > 0 {
			s.state.Events = s.state.Events[1:]
			continue
		}
		if len(s.state.Answer) > 1024 {
			s.state.Answer = sideToolText(s.state.Answer, len(s.state.Answer)/2)
			continue
		}
		if len(s.state.Approvals) > 0 {
			s.state.Approvals = s.state.Approvals[:len(s.state.Approvals)-1]
			continue
		}
		return
	}
}
func (s *codexSideToolSession) view() protocol.SideQuestionToolState {
	s.mu.Lock()
	worker, id, closed := s.worker, s.threadID, s.closed
	s.mu.Unlock()
	approvals := []protocol.Event{}
	if !closed && worker != nil && id != "" {
		for _, ev := range worker.aps.snapshot("codex:" + id) {
			if len(approvals) == 16 {
				break
			}
			approvals = append(approvals, sideToolApproval(ev, s.parentID))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		approvals = []protocol.Event{}
	}
	if sideQuestionHash(s.state.Approvals) != sideQuestionHash(approvals) {
		s.state.Approvals = approvals
		s.state.Sequence++
	}
	if s.state.State == "waiting_approval" && len(approvals) == 0 && !s.closed {
		s.state.State = "running"
		s.state.Sequence++
	}
	s.state.CanSend = !s.closed && !s.starting && !s.uncertain && len(approvals) == 0 && s.turns < maxSideQuestionTurns && (s.state.State == "idle" || s.state.State == "completed" || s.state.State == "failed" || s.state.State == "interrupted")
	s.boundStateLocked()
	return cloneSideToolState(s.state)
}
func (a *codexAgent) SideQuestionTools(ctx context.Context, parentID, owner string, snapshot SideQuestionSnapshot, request protocol.SideQuestionToolRequest, liveValidate func() error) (protocol.SideQuestionToolState, error) {
	var zero protocol.SideQuestionToolState
	if !ValidSideQuestionToolRequest(request) || !workflowText(owner, 4096, true) || liveValidate == nil {
		return zero, errSideQuestionInvalid
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	connection, reason, err := a.sideQuestionConnection(ctx, parentID, liveValidate)
	if err != nil {
		return zero, err
	}
	if reason != "" || a.sideToolOptions(parentID, connection) == nil {
		return zero, errSideToolUnavailable
	}
	a.mu.Lock()
	t := a.threads[parentID]
	cwd := ""
	if t != nil {
		cwd = t.info.Cwd
	}
	a.mu.Unlock()
	if cwd == "" {
		return zero, errSideQuestionStale
	}
	key := sideQuestionHash([]string{owner, request.ChainID})
	a.sideTools.mu.Lock()
	s, factory, registryClosed := a.sideTools.sessions[key], a.sideTools.workerFactory, a.sideTools.closed
	a.sideTools.mu.Unlock()
	if registryClosed {
		return zero, errSideToolClosed
	}
	if s == nil {
		if request.Action != "open" {
			return zero, errSideQuestionStale
		}
		a.sideQuestions.mu.Lock()
		catalog := a.sideQuestions.sessions[request.ExpectedRevision]
		valid := catalog != nil && catalog.key == "codex:"+parentID && catalog.reading == sideQuestionHash(snapshot) && catalog.binding == connection.binding && !catalog.expire(time.Now())
		a.sideQuestions.mu.Unlock()
		if !valid {
			return zero, errSideQuestionStale
		}
		background, cancel := context.WithCancel(context.WithoutCancel(ctx))
		s = &codexSideToolSession{parentID: parentID, owner: owner, cwd: cwd, binding: connection.binding, snapshot: snapshot, settings: settingsForContext(ctx, a.settings), ctx: background, cancel: cancel, done: make(chan struct{}), receipts: map[string]sideToolReceipt{}, state: protocol.SideQuestionToolState{SessionKey: "codex:" + parentID, ChainID: request.ChainID, Revision: request.ExpectedRevision, Permission: "workspace-write", State: "idle", Sequence: 1, Events: []protocol.Event{}, Approvals: []protocol.Event{}, ExpiresAt: time.Now().Add(sideToolLease).UnixMilli()}}
		s.snapshot.Messages = append([]SideQuestionMessage(nil), snapshot.Messages...)
		guard := a.sideQuestionGuard(background, parentID, connection.binding, liveValidate)
		s.guard = func() error {
			if err := guard(); err != nil {
				return err
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			t := a.threads[parentID]
			if t == nil || t.info.Archived || !sideToolSamePath(t.info.Cwd, cwd) {
				return errSideQuestionStale
			}
			return nil
		}
		a.sideTools.mu.Lock()
		if a.sideTools.sessions == nil {
			a.sideTools.sessions = map[string]*codexSideToolSession{}
		}
		active := 0
		for _, existing := range a.sideTools.sessions {
			existing.mu.Lock()
			if !existing.closed || existing.occupiedLocked() {
				active++
			}
			existing.mu.Unlock()
		}
		if old := a.sideTools.sessions[key]; old != nil {
			cancel()
			s = old
		} else if a.sideTools.closed || active >= maxSideToolSessions || len(a.sideTools.sessions) >= maxSideToolTombstones {
			a.sideTools.mu.Unlock()
			cancel()
			return zero, errSideQuestionLimit
		} else {
			a.sideTools.sessions[key] = s
			go s.watch()
		}
		a.sideTools.mu.Unlock()
	}
	if s.parentID != parentID || s.owner != owner || s.binding != connection.binding || s.state.Revision != request.ExpectedRevision || !sideToolSamePath(s.cwd, cwd) {
		return zero, errSideQuestionStale
	}
	if request.Action == "read" {
		s.mu.Lock()
		closed, expired := s.closed, time.Now().UnixMilli() >= s.state.ExpiresAt
		s.mu.Unlock()
		// The current source and owner were validated above. A closed chain
		// remains readable so reconnect can confirm retirement without retrying.
		if closed {
			return s.view(), nil
		}
		if expired {
			s.close("临时执行连接已过期")
			return s.view(), nil
		}
		if err := s.check(); err != nil {
			go s.close("临时工具侧聊来源已变化")
			return zero, err
		}
		s.mu.Lock()
		s.state.ExpiresAt = time.Now().Add(sideToolLease).UnixMilli()
		s.mu.Unlock()
		return s.view(), nil
	}
	hash := sideQuestionHash(request)
	// Close can cancel startup before waiting for the serialized write path.
	if request.Action == "close" {
		s.mu.Lock()
		prior, exists := s.receipts[request.OperationID]
		if exists && prior.hash != hash {
			s.mu.Unlock()
			return zero, errSideQuestionStale
		}
		s.receipts[request.OperationID] = sideToolReceipt{hash: hash, attempted: true}
		s.mu.Unlock()
		s.close("临时工具侧聊已关闭，仅停止了此临时任务")
		return s.view(), nil
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := s.check(); err != nil {
		return zero, err
	}
	s.mu.Lock()
	prior, exists := s.receipts[request.OperationID]
	if exists {
		s.mu.Unlock()
		if prior.hash != hash {
			return zero, errSideQuestionStale
		}
		return s.view(), nil
	}
	if len(s.receipts) >= 64 {
		s.mu.Unlock()
		return zero, errSideQuestionLimit
	}
	s.receipts[request.OperationID] = sideToolReceipt{hash: hash}
	s.mu.Unlock()
	switch request.Action {
	case "features":
		err = s.loadFeatures(ctx, a, factory)
	case "send":
		if err = validateSideQuestionModel(ctx, "codex", request.Model); err == nil {
			err = s.send(ctx, a, request, factory, liveValidate)
		}
	case "stop":
		err = s.stop(ctx, request)
	case "approve":
		err = s.approve(ctx, request, liveValidate)
	}
	if err != nil {
		s.mu.Lock()
		if !s.receipts[request.OperationID].attempted {
			delete(s.receipts, request.OperationID)
		}
		s.mu.Unlock()
		return s.view(), err
	}
	if err = s.check(); err != nil {
		return zero, err
	}
	return s.view(), nil
}

var _ SideQuestionToolProvider = (*codexAgent)(nil)
var _ SideQuestionToolWorkspaceProvider = (*codexAgent)(nil)
