package agents

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"salcara/bridge/internal/protocol"
)

// Limits from docs/PROTOCOL.md.
const (
	maxOutputChars   = 4000
	maxDiffChars     = 20000
	maxTitleChars    = 80
	maxDetailChars   = 2000
	maxHistoryEvents = 400
	maxSessions      = 100
	approvalTimeout  = 10 * time.Minute
	externalActive   = 30 * time.Second
	streamInterval   = 300 * time.Millisecond
)

const errExternalRunning = "这个会话正在电脑上运行，结束后才能继续"

func nowMs() int64 { return time.Now().UnixMilli() }

func newID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// newUUID returns a random RFC 4122 v4 UUID (Claude's --session-id needs one).
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// truncHead keeps the first n characters (runes).
func truncHead(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// truncTail keeps the last n characters (command output: the end is what matters).
func truncTail(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return "…" + string(r[len(r)-(n-1):])
}

// oneLine collapses whitespace so a command or prompt fits a title.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func titleText(s string) string { return truncHead(oneLine(s), maxTitleChars) }

// shortPath shows p relative to cwd when it is inside it.
func shortPath(p, cwd string) string {
	if p == "" {
		return p
	}
	if cwd != "" && filepath.IsAbs(p) {
		if rel, err := filepath.Rel(cwd, p); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	// Fixtures and transcripts can carry POSIX paths even when the bridge is
	// running on Windows (for example a WSL/remote worker). filepath.IsAbs on
	// Windows intentionally rejects `/work/app`, so use slash semantics for
	// this wire representation without changing native Windows paths.
	if cwd != "" && strings.HasPrefix(p, "/") && strings.HasPrefix(cwd, "/") {
		base, target := path.Clean(cwd), path.Clean(p)
		if target == base {
			return "."
		}
		prefix := strings.TrimRight(base, "/") + "/"
		if strings.HasPrefix(target, prefix) {
			return filepath.ToSlash(strings.TrimPrefix(target, prefix))
		}
	}
	return p
}

func jsonCompact(v any) string {
	if v == nil {
		return ""
	}
	if raw, ok := v.(json.RawMessage); ok {
		if len(raw) == 0 || string(raw) == "null" {
			return ""
		}
		var x any
		if json.Unmarshal(raw, &x) == nil {
			v = x
		}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func str(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return v
	case nil:
		return ""
	case float64:
		return fmt.Sprint(v)
	default:
		return ""
	}
}

func intPtr(v int) *int { return &v }

// dedupeEvents collapses repeated updates of the same item (streaming message, tool running→done) into one
// event at the position of its first occurrence, carrying the latest content, then keeps the last max.
func dedupeEvents(evs []protocol.Event, max int) []protocol.Event {
	out := make([]protocol.Event, 0, len(evs))
	pos := map[string]int{}
	for _, e := range evs {
		key := ""
		switch e.Type {
		case "message", "reasoning", "tool":
			key = e.Type + "|" + e.ID
		}
		if key != "" {
			if i, ok := pos[key]; ok {
				out[i] = e
				continue
			}
			pos[key] = len(out)
		}
		out = append(out, e)
	}
	if max > 0 && len(out) > max {
		out = out[len(out)-max:]
	}
	return out
}

// throttle coalesces rapid streaming updates (text so far, output so far) so the hub's event cache isn't
// flooded: at most one event per id every streamInterval; Final() flushes and cancels anything pending.
type throttle struct {
	mu      sync.Mutex
	emit    func(protocol.Event)
	pending map[string]*throttleEntry
	closed  bool
}

type throttleEntry struct {
	ev    protocol.Event
	last  time.Time
	timer *time.Timer
}

func newThrottle(emit func(protocol.Event)) *throttle {
	return &throttle{emit: emit, pending: map[string]*throttleEntry{}}
}

func (t *throttle) key(e protocol.Event) string { return e.SessionKey + "|" + e.Type + "|" + e.ID }

// Update records a partial event; it is emitted now if the id was quiet long enough, otherwise later.
func (t *throttle) Update(e protocol.Event) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	k := t.key(e)
	ent := t.pending[k]
	if ent == nil {
		ent = &throttleEntry{}
		t.pending[k] = ent
	}
	ent.ev = e
	if ent.timer != nil {
		t.mu.Unlock()
		return
	}
	wait := streamInterval - time.Since(ent.last)
	if wait <= 0 {
		ent.last = time.Now()
		t.mu.Unlock()
		t.emit(e)
		return
	}
	ent.timer = time.AfterFunc(wait, func() {
		t.mu.Lock()
		cur := t.pending[k]
		if cur != ent || t.closed {
			t.mu.Unlock()
			return
		}
		ent.timer = nil
		ent.last = time.Now()
		ev := ent.ev
		t.mu.Unlock()
		t.emit(ev)
	})
	t.mu.Unlock()
}

// Final emits e immediately and drops any pending partial for the same id.
func (t *throttle) Final(e protocol.Event) {
	t.mu.Lock()
	k := t.key(e)
	if ent := t.pending[k]; ent != nil {
		if ent.timer != nil {
			ent.timer.Stop()
		}
		delete(t.pending, k)
	}
	closed := t.closed
	t.mu.Unlock()
	if !closed {
		t.emit(e)
	}
}

// Drop forgets a pending partial without emitting it.
func (t *throttle) Drop(e protocol.Event) {
	t.mu.Lock()
	k := t.key(e)
	if ent := t.pending[k]; ent != nil {
		if ent.timer != nil {
			ent.timer.Stop()
		}
		delete(t.pending, k)
	}
	t.mu.Unlock()
}

func (t *throttle) Close() {
	t.mu.Lock()
	t.closed = true
	for k, ent := range t.pending {
		if ent.timer != nil {
			ent.timer.Stop()
		}
		delete(t.pending, k)
	}
	t.mu.Unlock()
}

// approvals is a small registry of approvals waiting for the phone.
type approvals struct {
	mu      sync.Mutex
	waiting map[string]*pendingApproval
}

type pendingApproval struct {
	id         string
	sessionKey string
	tool       string
	ref        string // agent-specific reference (Codex: JSON-RPC request id)
	ch         chan approvalAnswer
	expiresAt  time.Time
	request    *protocol.Event // current registry state, not a replay cache
	validate   func(map[string][]string) error
}

type approvalAnswer struct {
	decision string // allow | allow_session | deny
	message  string
	by       string // phone | desktop | timeout
	noReply  bool   // the request was withdrawn by the tool itself; nothing to answer
	answers  map[string][]string
}

func newApprovals() *approvals { return &approvals{waiting: map[string]*pendingApproval{}} }

func (a *approvals) add(sessionKey, tool string) *pendingApproval {
	p := &pendingApproval{id: newID("ap_"), sessionKey: sessionKey, tool: tool, ch: make(chan approvalAnswer, 1), expiresAt: time.Now().Add(approvalTimeout)}
	a.mu.Lock()
	a.waiting[p.id] = p
	a.mu.Unlock()
	return p
}

// addRequest publishes the exact, bounded visible request atomically with its
// registry entry. A reconnect never re-creates an approval from transcript text.
func (a *approvals) addRequest(sessionKey, tool, ref string, ev protocol.Event) (*pendingApproval, protocol.Event) {
	return a.addValidatedRequest(sessionKey, tool, ref, ev, nil)
}

func (a *approvals) addValidatedRequest(sessionKey, tool, ref string, ev protocol.Event, validate func(map[string][]string) error) (*pendingApproval, protocol.Event) {
	p := &pendingApproval{id: newID("ap_"), sessionKey: sessionKey, tool: tool, ref: ref,
		ch: make(chan approvalAnswer, 1), expiresAt: time.Now().Add(approvalTimeout), validate: validate}
	if ev.ExpiresAt != 0 {
		deadline := time.UnixMilli(ev.ExpiresAt)
		if deadline.Before(p.expiresAt) {
			p.expiresAt = deadline
		}
	}
	ev = protocol.Event{SessionKey: sessionKey, Tool: tool, TS: nowMs(), Type: "approval.request", ApprovalID: p.id,
		Kind: ev.Kind, Title: truncHead(ev.Title, maxTitleChars), Detail: truncHead(ev.Detail, maxDetailChars),
		Diff: truncHead(ev.Diff, maxDiffChars), Cwd: truncHead(ev.Cwd, 4096),
		Questions: cloneQuestions(ev.Questions), QuestionMode: ev.QuestionMode, ExpiresAt: p.expiresAt.UnixMilli()}
	stored := ev
	stored.Questions = cloneQuestions(ev.Questions)
	p.request = &stored
	a.mu.Lock()
	a.waiting[p.id] = p
	a.mu.Unlock()
	return p, ev
}

// snapshot is read-only: only requests still owned by the active local process
// and inside their original deadline can appear. It does not extend a lease,
// answer a request, persist approval state, or dispatch any tool operation.
func (a *approvals) snapshot(sessionKey string) []protocol.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	out := make([]protocol.Event, 0, maxHistoryEvents)
	for _, pending := range a.waiting {
		if pending.sessionKey == sessionKey && pending.request != nil && now.Before(pending.expiresAt) {
			ev := *pending.request
			ev.Questions = cloneQuestions(ev.Questions)
			i := sort.Search(len(out), func(i int) bool {
				return out[i].TS > ev.TS || out[i].TS == ev.TS && out[i].ApprovalID >= ev.ApprovalID
			})
			if len(out) == maxHistoryEvents {
				if i == 0 {
					continue
				}
				copy(out, out[1:])
				i--
			} else {
				out = append(out, protocol.Event{})
			}
			copy(out[i+1:], out[i:len(out)-1])
			out[i] = ev
		}
	}
	return out
}

func historyWithApprovals(history, pending []protocol.Event) []protocol.Event {
	remaining := maxHistoryEvents - len(pending)
	if len(history) > remaining {
		history = history[len(history)-remaining:]
	}
	return append(history, pending...)
}

// resolveByRef withdraws the approval whose ref matches (the tool resolved it on its own).
func (a *approvals) resolveByRef(ref string) {
	a.mu.Lock()
	var p *pendingApproval
	for id, x := range a.waiting {
		if x.ref != "" && x.ref == ref {
			p = x
			delete(a.waiting, id)
			break
		}
	}
	a.mu.Unlock()
	if p != nil {
		p.ch <- approvalAnswer{decision: "deny", by: "desktop", noReply: true}
	}
}

// wait blocks until the approval is answered, times out (10 min) or stop closes.
func (a *approvals) wait(p *pendingApproval, stop <-chan struct{}) approvalAnswer {
	a.mu.Lock()
	expiresAt := p.expiresAt // read under the same lock that guards the pending entry
	a.mu.Unlock()
	timer := time.NewTimer(time.Until(expiresAt))
	defer timer.Stop()
	select {
	case ans := <-p.ch:
		return ans
	case <-timer.C:
		if a.answer(p.id, approvalAnswer{decision: "deny", message: "10 分钟内没有人处理，已自动拒绝", by: "timeout"}) {
			return <-p.ch
		}
		return <-p.ch // answered concurrently
	case <-stop:
		a.remove(p.id)
		return approvalAnswer{decision: "deny", message: "桥接程序已退出", by: "desktop"}
	}
}

func (a *approvals) remove(id string) {
	a.mu.Lock()
	delete(a.waiting, id)
	a.mu.Unlock()
}

// answer delivers a decision; false if the id is unknown (or already answered).
func (a *approvals) answer(id string, ans approvalAnswer) bool {
	a.mu.Lock()
	p := a.waiting[id]
	if p == nil {
		a.mu.Unlock()
		return false
	}
	if !time.Now().Before(p.expiresAt) {
		delete(a.waiting, id)
		a.mu.Unlock()
		p.ch <- approvalAnswer{decision: "deny", message: "审批已过期，未执行操作", by: "timeout"}
		return false
	}
	if p.request != nil && p.request.Kind == "question" && ans.decision != "deny" {
		// A question never grants session-wide tool permission. Invalid answers
		// leave the live request open so the phone can correct them.
		if ans.decision != "allow" || p.validate == nil || p.validate(ans.answers) != nil {
			a.mu.Unlock()
			return false
		}
	} else if ans.decision != "deny" && len(ans.answers) != 0 {
		a.mu.Unlock()
		return false
	}
	delete(a.waiting, id)
	ans.answers = cloneAnswers(ans.answers)
	a.mu.Unlock()
	p.ch <- ans
	return true
}

// countFor returns how many approvals are waiting for a session.
func (a *approvals) countFor(sessionKey string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, p := range a.waiting {
		if p.sessionKey == sessionKey && time.Now().Before(p.expiresAt) {
			n++
		}
	}
	return n
}

// cancelSession denies every approval of a session (process stopped / turn interrupted).
func (a *approvals) cancelSession(sessionKey string) {
	a.mu.Lock()
	var list []*pendingApproval
	for id, p := range a.waiting {
		if p.sessionKey == sessionKey {
			list = append(list, p)
			delete(a.waiting, id)
		}
	}
	a.mu.Unlock()
	for _, p := range list {
		p.ch <- approvalAnswer{decision: "deny", message: "任务已停止", by: "desktop"}
	}
}

func (a *approvals) cancelAll() {
	a.mu.Lock()
	list := make([]*pendingApproval, 0, len(a.waiting))
	for id, p := range a.waiting {
		list = append(list, p)
		delete(a.waiting, id)
	}
	a.mu.Unlock()
	for _, p := range list {
		p.ch <- approvalAnswer{decision: "deny", message: "桥接程序已退出", by: "desktop"}
	}
}

func normalizeDecision(d string) string {
	switch d {
	case "allow", "allow_session":
		return d
	default:
		return "deny"
	}
}

func normalizePolicy(p string) string {
	switch p {
	case "auto_edits", "auto_all":
		return p
	default:
		return "ask"
	}
}

func isAbs(p string) bool { return filepath.IsAbs(p) }

func joinPath(a, b string) string { return filepath.Join(a, b) }
