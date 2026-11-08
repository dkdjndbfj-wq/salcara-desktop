package agents

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode"

	"salcara/bridge/internal/protocol"
)

// These are the control messages used by the official Agent SDK. No
// user prompt, model request, directory scan or fabricated slash catalog is
// needed to initialize a worker and ask reload_skills for its actual skills.
type claudeSpawnOptions struct {
	mode  string
	probe bool
}

type claudeControlReply struct {
	data json.RawMessage
	err  error
}

const maxClaudeSkills = protocol.MaxFeatureSkills
const maxClaudeControlResponseBytes = protocol.MaxFeatureCatalogBytes // tighter than the shared 64 MiB agent-line transport limit

var errClaudeControlInvalid = errors.New("feature_response_invalid: Claude Code 返回的原生控制响应无效")
var errClaudeFeatureTooLarge = errors.New("feature_response_too_large: Claude 技能目录超过读取上限")

type claudeControlRejection struct{ unsupportedSubtype string }

func (*claudeControlRejection) Error() string {
	return "feature_control_rejected: Claude Code 拒绝了原生控制请求，请检查 CLI 版本及配置"
}

// Only an explicit native unknown/unsupported-subtype response establishes
// lack of support. Other rejections, schema failures and transport errors are
// operational failures; never turn them into a successful empty catalog.
func claudeUnsupportedControlSubtype(message string) string {
	message = strings.ToLower(strings.TrimSpace(message))
	for _, prefix := range []string{"unsupported control request subtype:", "unknown control request subtype:", "unknown request subtype:"} {
		if strings.HasPrefix(message, prefix) {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(message, prefix)), "\"'.")
		}
	}
	return ""
}

func (p *claudeProc) failControlRequests(err error) {
	p.controlMu.Lock()
	for id, ch := range p.controlPending {
		delete(p.controlPending, id)
		ch <- claudeControlReply{err: err}
	}
	p.controlMu.Unlock()
}

func (p *claudeProc) acceptControlResponse(raw json.RawMessage) {
	if len(raw) > maxClaudeControlResponseBytes {
		p.failControlRequests(errClaudeFeatureTooLarge)
		return
	}
	var response struct {
		Subtype   string          `json:"subtype"`
		RequestID string          `json:"request_id"`
		Response  json.RawMessage `json:"response"`
		Error     string          `json:"error"`
	}
	if json.Unmarshal(raw, &response) != nil || response.RequestID == "" {
		p.failControlRequests(errClaudeControlInvalid)
		return
	}
	p.controlMu.Lock()
	ch := p.controlPending[response.RequestID]
	delete(p.controlPending, response.RequestID)
	p.controlMu.Unlock()
	if ch == nil {
		return
	}
	reply := claudeControlReply{data: response.Response}
	payload := strings.TrimSpace(string(response.Response))
	if response.Subtype == "error" {
		reply.err = &claudeControlRejection{unsupportedSubtype: claudeUnsupportedControlSubtype(response.Error)}
	} else if response.Subtype != "success" || payload != "" && payload != "null" && payload[0] != '{' {
		reply.err = errClaudeControlInvalid
	}
	ch <- reply
}

func (a *claudeAgent) claudeControl(ctx context.Context, p *claudeProc, request map[string]any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := newID("control_")
	ch := make(chan claudeControlReply, 1)
	p.controlMu.Lock()
	if p.controlPending == nil {
		p.controlPending = make(map[string]chan claudeControlReply)
	}
	p.controlPending[id] = ch
	p.controlMu.Unlock()
	defer func() {
		p.controlMu.Lock()
		delete(p.controlPending, id)
		p.controlMu.Unlock()
	}()
	frame, _ := json.Marshal(map[string]any{"type": "control_request", "request_id": id, "request": request})
	frame = append(frame, '\n')
	p.wmu.Lock()
	if err := ctx.Err(); err != nil {
		p.wmu.Unlock()
		return nil, err
	}
	if !a.readerWorkerActive(p) {
		p.wmu.Unlock()
		return nil, errors.New("Claude Code 连接已结束")
	}
	n, err := p.stdin.Write(frame)
	p.wmu.Unlock()
	if err != nil || n != len(frame) {
		a.stop(p) // Never leave a possibly partial JSON control frame reusable.
		if err == nil {
			err = io.ErrShortWrite
		}
		return nil, err
	}
	select {
	case reply := <-ch:
		return reply.data, reply.err
	case <-ctx.Done():
		// A timed-out control can still be applied later. Retire this exact
		// worker so a subsequent prompt cannot run under an unknown mode.
		a.stop(p)
		return nil, ctx.Err()
	case <-p.stopCh:
		return nil, errors.New("Claude Code 连接已结束")
	case <-p.done:
		return nil, errors.New("Claude Code 连接已结束")
	}
}

// Caller holds turnMu, including across the final user frame. This prevents
// another sender from changing native modes between validation and admission.
func (a *claudeAgent) initializeClaudeControl(ctx context.Context, p *claudeProc) error {
	if p.initialized {
		return nil
	}
	raw, err := a.claudeControl(ctx, p, map[string]any{"subtype": "initialize"})
	if err != nil {
		return err
	}
	if payload := strings.TrimSpace(string(raw)); payload == "" || payload[0] != '{' {
		return errClaudeControlInvalid
	}
	p.initialized = true
	return nil
}

func validClaudeSkillName(name string) bool {
	if ValidateTurnFeatures(TurnOptions{Skills: []protocol.SkillReference{{Name: name}}}) != nil || strings.HasPrefix(name, "/") {
		return false
	}
	for _, r := range name {
		if unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune("(),*\\/", r) {
			return false
		}
	}
	return true
}

func validateClaudeFeatures(opts TurnOptions) error {
	if opts.CollaborationMode != "" && opts.CollaborationMode != "default" && opts.CollaborationMode != "plan" {
		return errors.New("collaboration_mode_invalid: Claude 仅支持常规和计划模式")
	}
	if len(opts.Skills) > 1 {
		return errors.New("skill_invalid: Claude 每条消息只能选择一个技能")
	}
	for _, skill := range opts.Skills {
		if !validClaudeSkillName(skill.Name) || skill.Path != "" {
			return errors.New("skill_invalid: Claude 技能必须使用原生名称且不能包含文件路径")
		}
	}
	return nil
}

func (a *claudeAgent) claudeFeatureIdle(p *claudeProc) error {
	if !a.readerWorkerActive(p) {
		return errors.New("Claude Code 连接已结束")
	}
	p.mu.Lock()
	busy := p.turns != 0 || len(p.turnIDs) != 0 || p.activeSubtasks.Load() || p.contextCompacting
	p.mu.Unlock()
	if busy || a.aps.countFor("claude:"+p.id) != 0 {
		return errors.New("feature_busy: Claude 正在处理任务，请完成后再选择模式或技能")
	}
	return nil
}

func (a *claudeAgent) reloadClaudeSkills(ctx context.Context, p *claudeProc) ([]protocol.Skill, bool, error) {
	// The official SDK reloads skill definitions from disk here. Its SlashCommand
	// response has no source path or content digest, so a successful reload can
	// confirm the current name but cannot establish file identity or a body hash.
	raw, err := a.claudeControl(ctx, p, map[string]any{"subtype": "reload_skills"})
	if err != nil {
		var rejection *claudeControlRejection
		if errors.As(err, &rejection) && rejection.unsupportedSubtype == "reload_skills" {
			return nil, false, nil
		}
		return nil, false, err
	}
	if len(raw) > maxClaudeControlResponseBytes {
		return nil, false, errClaudeFeatureTooLarge
	}
	var payload struct {
		Skills *[]struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Builtin     bool   `json:"builtin"`
		} `json:"skills"`
	}
	if json.Unmarshal(raw, &payload) != nil || payload.Skills == nil {
		return nil, false, errClaudeControlInvalid
	}
	if len(*payload.Skills) > maxClaudeSkills {
		return nil, false, errClaudeFeatureTooLarge
	}
	skills := make([]protocol.Skill, 0, len(*payload.Skills))
	counts := make(map[string]int, len(*payload.Skills))
	for _, skill := range *payload.Skills {
		counts[skill.Name]++
	}
	for _, skill := range *payload.Skills {
		// A native builtin wins /name resolution over a same-named user skill.
		// Do not advertise either builtins or an ambiguous name as an exact skill
		// selection; choosing the first row could invoke a different command.
		if !validClaudeSkillName(skill.Name) || skill.Builtin || counts[skill.Name] != 1 {
			continue
		}
		skills = append(skills, protocol.Skill{Name: skill.Name, Description: truncHead(skill.Description, 4000)})
	}
	return skills, true, nil
}

func (a *claudeAgent) Features(ctx context.Context, cwd, sessionID string) (out protocol.AgentFeatures, err error) {
	out.CollaborationModes = []string{}
	out.Skills = []protocol.Skill{}
	a.mu.Lock()
	p := a.procs[sessionID]
	live, hasLive := a.live[sessionID]
	a.mu.Unlock()
	if sessionID != "" {
		if p != nil {
			cwd = p.cwd
			if p.workerSettingsSig != claudeSettingsSig(settingsForContext(ctx, a.settings), a.exeOverride) {
				return out, errors.New("feature_settings_changed: 当前 worker 使用另一 API 配置，请完成任务并切换后再读取技能")
			}
		} else if hasLive {
			cwd = live.Cwd
		} else if info, ok := a.hist.info(sessionID); ok {
			cwd = info.Cwd
		} else {
			return out, errors.New("找不到这个会话")
		}
	}
	if p == nil {
		p, err = a.spawnConfigured(ctx, newUUID(), cwd, "", "ask", false, claudeSpawnOptions{probe: true})
		if err != nil {
			return out, err
		}
		defer func() {
			a.stop(p)
			select {
			case <-p.done:
				if !p.processExitConfirmed() {
					err = errClaudeProcessExitUnconfirmed
				} else {
					a.mu.Lock()
					a.hasUnconfirmedWorkerLocked() // drop the confirmed probe's startup fence
					a.mu.Unlock()
				}
			case <-time.After(5 * time.Second):
				err = errClaudeProcessExitUnconfirmed
			}
		}()
	}
	p.turnMu.Lock()
	defer p.turnMu.Unlock()
	if err = a.claudeFeatureIdle(p); err != nil {
		return out, err
	}
	if err = a.initializeClaudeControl(ctx, p); err != nil {
		return out, err
	}
	out.CollaborationModes = []string{"default", "plan"}
	out.Skills, out.SkillsSupported, err = a.reloadClaudeSkills(ctx, p)
	if out.Skills == nil {
		out.Skills = []protocol.Skill{}
	}
	return out, err
}

func (a *claudeAgent) prepareClaudeFeatures(ctx context.Context, p *claudeProc, text string, opts TurnOptions) (string, error) {
	if err := validateClaudeFeatures(opts); err != nil {
		return "", err
	}
	mode := opts.CollaborationMode
	p.mu.Lock()
	if mode == "" && p.requirePlan && p.collaborationMode != "plan" {
		mode = "plan"
	}
	p.mu.Unlock()
	if mode == "" && len(opts.Skills) == 0 {
		return text, nil
	}
	if opts.ValidatePermission != nil {
		if err := opts.ValidatePermission(); err != nil {
			return "", err
		}
	}
	if err := a.claudeFeatureIdle(p); err != nil {
		return "", err
	}
	if err := a.initializeClaudeControl(ctx, p); err != nil {
		return "", err
	}
	if len(opts.Skills) != 0 {
		skills, supported, err := a.reloadClaudeSkills(ctx, p)
		if err != nil {
			return "", err
		}
		if !supported {
			return "", errors.New("skill_unsupported: 当前 Claude Code 无法列出原生技能，请升级 CLI")
		}
		found := false
		for _, skill := range skills {
			found = found || skill.Name == opts.Skills[0].Name
		}
		if !found {
			return "", errors.New("skill_unavailable: 当前项目中已找不到所选 Claude 技能，请刷新后重选")
		}
		text = "/" + opts.Skills[0].Name + " " + text
	}
	if mode != "" {
		if err := a.claudeFeatureIdle(p); err != nil {
			return "", err
		}
		if opts.ValidatePermission != nil {
			if err := opts.ValidatePermission(); err != nil {
				return "", err
			}
		}
		nativeMode := "plan"
		if mode == "default" {
			nativeMode = map[string]string{"ask": "default", "auto_edits": "acceptEdits", "auto_all": "bypassPermissions"}[p.policy]
			if nativeMode == "" {
				nativeMode = "default"
			}
		}
		if _, err := a.claudeControl(ctx, p, map[string]any{"subtype": "set_permission_mode", "mode": nativeMode}); err != nil {
			return "", err
		}
		a.observeClaudeMode(p, nativeMode, true)
	}
	return text, nil
}

// Native metadata can confirm plan, but cannot silently clear a pending or
// denied plan. An explicit next-turn default control or an approved exit is
// required. If a runtime reports an unexpected transition, publish unknown
// and reassert plan before admitting any later prompt.
func (a *claudeAgent) observeClaudeMode(p *claudeProc, nativeMode string, explicit bool) {
	mode := ""
	switch nativeMode {
	case "plan":
		mode = "plan"
	case "default", "acceptEdits", "bypassPermissions", "dontAsk", "auto":
		mode = "default"
	default:
		return
	}
	a.mu.Lock()
	if !a.readerWorkerActiveLocked(p) || p.featureProbe {
		a.mu.Unlock()
		return
	}
	p.mu.Lock()
	if mode == "plan" {
		p.requirePlan, p.planExitApproved = true, false
	} else if p.requirePlan && !explicit && !p.planExitApproved {
		mode = ""
	} else {
		p.requirePlan, p.planExitApproved = false, false
	}
	p.collaborationMode = mode
	if a.planRequired == nil {
		a.planRequired = make(map[string]bool)
	}
	a.planRequired[p.id] = p.requirePlan
	p.mu.Unlock()
	info, exists := a.live[p.id]
	changed := exists && info.CollaborationMode != mode
	if changed {
		info.CollaborationMode = mode
		info = a.commitLiveStateLocked(p.id, info)
	}
	a.mu.Unlock()
	if changed {
		a.emit(protocol.Event{SessionKey: info.SessionKey, Type: "session.updated", Session: ptrInfo(info)})
	}
}

var _ FeatureProvider = (*claudeAgent)(nil)
