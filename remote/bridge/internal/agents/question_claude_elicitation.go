package agents

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"salcara/bridge/internal/protocol"
)

const ClaudeElicitationPath = "/internal/agents/claude/elicitation"
const claudeElicitationEnv = "SALCARA_CLAUDE_ELICITATION_TOKEN"
const claudeElicitationHeader = "X-Salcara-Elicitation-Token"

// Claude Code added HTTP hooks in 2.1.63 and Elicitation in 2.1.76.
// Unknown versions are deliberately not given unsupported settings.
func claudeElicitationVersionSupported(version string) bool {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	numbers := [3]int{}
	for i, part := range parts {
		if part == "" || strings.Trim(part, "0123456789") != "" {
			return false
		}
		value, err := strconv.Atoi(part)
		if err != nil {
			return false
		}
		numbers[i] = value
	}
	return numbers[0] > 2 || numbers[0] == 2 && (numbers[1] > 1 || numbers[1] == 1 && numbers[2] >= 76)
}

type claudeElicitationLease struct {
	token string
	cwd   string
	done  chan struct{}
	once  sync.Once
}

func (a *claudeAgent) prepareElicitationHook(exe string, settings Settings, id, cwd string) (*claudeElicitationLease, string) {
	if settings.LocalPort <= 0 || settings.LocalPort > 65535 {
		return nil, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	version, ok := toolVersion(ctx, exe)
	cancel()
	if !ok || !claudeElicitationVersionSupported(version) {
		a.emit(protocol.Event{SessionKey: "claude:" + id, Type: "notice", Level: "info", Text: "手机 MCP 表单需要 Claude Code 2.1.76 或更新版本"})
		return nil, ""
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, ""
	}
	lease := &claudeElicitationLease{token: base64.RawURLEncoding.EncodeToString(secret), cwd: cwd, done: make(chan struct{})}
	config := map[string]any{"hooks": map[string]any{"Elicitation": []any{map[string]any{"matcher": "*", "hooks": []any{map[string]any{
		"type": "http", "url": "http://127.0.0.1:" + strconv.Itoa(settings.LocalPort) + ClaudeElicitationPath,
		// The Bridge deadline expires before the host's HTTP timeout so it can
		// return an explicit decline, not an HTTP failure's normal host flow.
		"timeout":        int((approvalTimeout + 10*time.Second) / time.Second),
		"headers":        map[string]string{claudeElicitationHeader: "$" + claudeElicitationEnv},
		"allowedEnvVars": []string{claudeElicitationEnv},
	}}}}}}
	encoded, err := json.Marshal(config)
	if err != nil {
		return nil, ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.elicitationHooks[id] != nil {
		return nil, ""
	}
	a.elicitationHooks[id] = lease
	return lease, string(encoded)
}

func (a *claudeAgent) revokeElicitationHook(id string, lease *claudeElicitationLease) {
	if lease == nil {
		return
	}
	a.mu.Lock()
	if a.elicitationHooks[id] == lease {
		delete(a.elicitationHooks, id)
	}
	a.mu.Unlock()
	lease.once.Do(func() { close(lease.done) })
}

func (a *claudeAgent) activeElicitationLease(id string, lease *claudeElicitationLease) bool {
	a.mu.Lock()
	active := !a.closed && a.elicitationHooks[id] == lease
	a.mu.Unlock()
	if !active {
		return false
	}
	select {
	case <-lease.done:
		return false
	default:
		return true
	}
}

// Unused transcript_path, URL and other native context are not forwarded.
type claudeElicitationRequest struct {
	Session string          `json:"session_id"`
	Event   string          `json:"hook_event_name"`
	Server  string          `json:"mcp_server_name"`
	Message string          `json:"message"`
	Mode    string          `json:"mode"`
	Schema  json.RawMessage `json:"requested_schema"`
}

type claudeElicitationOutput struct {
	Specific claudeElicitationDecision `json:"hookSpecificOutput"`
}
type claudeElicitationDecision struct {
	Event   string `json:"hookEventName"`
	Action  string `json:"action"`
	Content any    `json:"content,omitempty"`
}

func claudeElicitationResult(action string, content map[string]any) claudeElicitationOutput {
	// An accepted, all-optional form can legitimately yield {}. Keeping the
	// non-nil map behind an interface includes that object instead of omitting
	// content; decline/cancel have no fabricated content at all.
	decision := claudeElicitationDecision{Event: "Elicitation", Action: action}
	if content != nil {
		decision.Content = content
	}
	return claudeElicitationOutput{Specific: decision}
}

func (a *claudeAgent) elicitationHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Decode bounded JSON first only to find the session's capability. No
	// visible event or pending request is created before authentication.
	var req claudeElicitationRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
	err := decoder.Decode(&req)
	var trailing any
	if err != nil || decoder.Decode(&trailing) != io.EOF || req.Session == "" || len(req.Session) > maxQuestionID {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	lease := a.elicitationHooks[req.Session]
	closed := a.closed
	a.mu.Unlock()
	if lease == nil || closed || subtle.ConstantTimeCompare([]byte(r.Header.Get(claudeElicitationHeader)), []byte(lease.token)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	output := claudeElicitationResult("decline", nil)
	if a.activeElicitationLease(req.Session, lease) && req.Event == "Elicitation" {
		output = a.answerElicitation(r, req, lease)
	}
	// A reply racing worker stop or HTTP cancellation must never accept.
	if r.Context().Err() != nil || !a.activeElicitationLease(req.Session, lease) {
		output = claudeElicitationResult("cancel", nil)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(output)
}

func (a *claudeAgent) answerElicitation(r *http.Request, req claudeElicitationRequest, lease *claudeElicitationLease) claudeElicitationOutput {
	sk := "claude:" + req.Session
	form, err := parseElicitationForm(req.Schema)
	if req.Mode != "" && req.Mode != "form" && req.Mode != "openai/form" || err != nil || req.Server == "" || !boundedQuestionText(req.Server, maxQuestionID) || !boundedQuestionText(req.Message, maxQuestionText) || secretFormField(req.Message) {
		a.emit(unsupportedQuestionNotice(sk, "MCP"))
		return claudeElicitationResult("decline", nil)
	}
	ev := questionEvent("mcp-form", form.questions, "MCP · "+req.Server, req.Message, lease.cwd)
	validate := func(answers map[string][]string) error {
		// No agent mutex here: the approval registry validates under its own
		// mutex, while session decoration acquires locks in the opposite order.
		select {
		case <-lease.done:
			return errQuestionUnsupported
		case <-a.closeCh:
			return errQuestionUnsupported
		default:
		}
		if r.Context().Err() != nil {
			return errQuestionUnsupported
		}
		return form.validate(answers)
	}
	p, ev := a.aps.addValidatedRequest(sk, "claude", "", ev, validate)
	a.emit(ev)
	a.setApprovalStatus(req.Session)
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		select {
		case <-r.Context().Done():
		case <-lease.done:
		case <-a.closeCh:
		case <-finished:
			return
		}
		close(stop)
	}()
	ans := a.aps.wait(p, stop)
	close(finished)
	a.emit(protocol.Event{SessionKey: sk, Type: "approval.resolved", ApprovalID: p.id, Decision: ans.decision, By: ans.by})
	a.setApprovalStatus(req.Session)
	if ans.decision == "allow" && r.Context().Err() == nil && a.activeElicitationLease(req.Session, lease) {
		if content, err := form.content(ans.answers); err == nil {
			return claudeElicitationResult("accept", content)
		}
	}
	if r.Context().Err() != nil || !a.activeElicitationLease(req.Session, lease) {
		return claudeElicitationResult("cancel", nil)
	}
	return claudeElicitationResult("decline", nil)
}
