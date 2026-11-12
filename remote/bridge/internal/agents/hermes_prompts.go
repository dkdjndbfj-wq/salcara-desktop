package agents

import (
	"encoding/json"
	"errors"
	"math"
	"salcara/bridge/internal/protocol"
	"time"
)

// Frozen upstream session_pending_controls projects one clarify prompt. The
// native answer is a string; multi-select strings contain a JSON label array,
// matching tools/clarify_gateway._coerce_multi_select_text.
func hermesQuestions(p hermesPrompt) ([]protocol.Question, error) {
	if p.Kind != "clarify" || !ValidTurnID(p.ID) || !boundedQuestionText(p.Question, maxQuestionText) || p.Question == "" || len(p.Choices) > maxQuestionOptions {
		return nil, errQuestionUnsupported
	}
	q := protocol.Question{ID: p.ID, Question: p.Question, Required: true, AllowCustom: true, InputType: "text", MultiSelect: p.MultiSelect && len(p.Choices) > 0}
	seen := map[string]bool{}
	for _, label := range p.Choices {
		if label == "" || !boundedQuestionText(label, 1024) || seen[label] {
			return nil, errQuestionUnsupported
		}
		seen[label] = true
		q.Options = append(q.Options, protocol.QuestionOption{Label: label})
	}
	if len(q.Options) > 0 {
		q.InputType = "enum"
	}
	return []protocol.Question{q}, nil
}

func hermesClarifyAnswer(p hermesPrompt, response ApprovalResponse) (string, error) {
	qs, err := hermesQuestions(p)
	if err != nil || response.Decision != "allow" || response.Message != "" {
		return "", errQuestionUnsupported
	}
	if err := validateQuestionAnswers(qs, response.Answers); err != nil {
		return "", err
	}
	values := response.Answers[p.ID]
	answer := values[0]
	if qs[0].MultiSelect {
		data, _ := json.Marshal(values)
		answer = string(data)
	}
	if len(answer) > 16384 {
		return "", errors.New("回答过长")
	}
	return answer, nil
}

// The original timeout_s is not a fresh lease on every resume. Keep the first
// observed deadline across reconnect/restart; the owner may expire it earlier.
func (a *hermesAgent) hermesPromptProjectionLocked(id string, s *hermesSession, p hermesPrompt) protocol.Event {
	warn := func(text string) protocol.Event {
		return protocol.Event{Type: "notice", Tool: hermesID, SessionKey: s.Info.SessionKey, ID: "hermes-prompt-" + p.ID, Level: "warn", Text: text}
	}
	s.Info.Status = "waiting_approval"
	s.busy = true
	s.Info.ActiveTurnID = hermesTurnID(s.Epoch, s.Generation)
	if !ValidTurnID(p.ID) || p.Generation != s.Generation || (p.Kind != "approval" && p.Kind != "clarify") || math.IsNaN(p.TimeoutSeconds) || math.IsInf(p.TimeoutSeconds, 0) || p.TimeoutSeconds > 86400 {
		s.uncertain = true
		return warn("Hermes 问题格式或所属任务无法确认，请在电脑核对原会话")
	}
	if attempt, exists := s.Approvals[p.ID]; exists {
		if attempt.Status == "pending" {
			s.uncertain = true
		}
		event := warn("此审批已提交，结果待核对；请刷新原会话，不会重复授权或改选")
		event.OperationID = attempt.OperationID
		return event
	}
	expiry := s.PromptDeadlines[p.ID]
	if p.TimeoutSeconds > 0 && expiry == 0 {
		if len(s.PromptDeadlines) >= 400 {
			s.uncertain = true
			return warn("Hermes 问题记录已达上限，请在电脑处理")
		}
		expiry = time.Now().Add(time.Duration(p.TimeoutSeconds * float64(time.Second))).UnixMilli()
		s.PromptDeadlines[p.ID] = expiry
	}
	if expiry > 0 && time.Now().UnixMilli() >= expiry {
		return warn("Hermes 问题已超过可回答时间，请刷新核对或停止当前任务")
	}
	event := hermesApprovalEvent(id, p)
	event.ExpiresAt = expiry
	if p.Kind == "clarify" {
		if !a.questions {
			s.uncertain = true
			return warn("此 Hermes 运行器未声明手机问答能力，请在电脑处理或更新 Hermes")
		}
		qs, err := hermesQuestions(p)
		if err != nil {
			s.uncertain = true
			return warn("Hermes 的此类问题需要在电脑处理")
		}
		event.Kind, event.Title, event.Questions = "question", "Hermes 需要补充信息", qs
		event.QuestionMode = "hermes"
		event.ApprovalOptions = &protocol.ApprovalOptions{Version: 1, Decisions: []string{"allow"}}
	}
	s.prompts[p.ID] = p
	return event
}
