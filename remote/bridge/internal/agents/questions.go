package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/mail"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"salcara/bridge/internal/protocol"
)

const (
	maxQuestions       = 16
	maxQuestionOptions = 32
	maxQuestionText    = 4096
	maxQuestionID      = 256
	maxAnswerText      = 8192
)

var errQuestionUnsupported = errors.New("这个问题需要在电脑上处理")

type sourceQuestion struct {
	ID          string                    `json:"id"`
	Header      string                    `json:"header"`
	Question    string                    `json:"question"`
	Options     []protocol.QuestionOption `json:"options"`
	MultiSelect bool                      `json:"multiSelect"`
	IsOther     bool                      `json:"isOther"`
	IsSecret    bool                      `json:"isSecret"`
}

func parseToolQuestions(raw json.RawMessage, mode string) ([]protocol.Question, error) {
	var input struct {
		Questions []sourceQuestion `json:"questions"`
	}
	if json.Unmarshal(raw, &input) != nil || len(input.Questions) == 0 || len(input.Questions) > maxQuestions {
		return nil, errQuestionUnsupported
	}
	seenID, seenText := map[string]bool{}, map[string]bool{}
	out := make([]protocol.Question, 0, len(input.Questions))
	for i, src := range input.Questions {
		if src.IsSecret || src.Question == "" || !boundedQuestionText(src.Question, maxQuestionText) || !boundedQuestionText(src.Header, 80) || len(src.Options) > maxQuestionOptions {
			return nil, errQuestionUnsupported
		}
		id := src.ID
		if mode == "claude" {
			id = "q_" + strconv.Itoa(i)
		}
		if id == "" || !boundedQuestionText(id, maxQuestionID) || seenID[id] || mode == "claude" && seenText[src.Question] {
			return nil, errQuestionUnsupported
		}
		seenID[id], seenText[src.Question] = true, true
		labels := map[string]bool{}
		for _, option := range src.Options {
			if option.Label == "" || !boundedQuestionText(option.Label, 1024) || !boundedQuestionText(option.Description, maxQuestionText) || labels[option.Label] {
				return nil, errQuestionUnsupported
			}
			labels[option.Label] = true
		}
		inputType := "enum"
		if len(src.Options) == 0 {
			inputType = "text"
		}
		out = append(out, protocol.Question{ID: id, Header: src.Header, Question: src.Question, Options: src.Options,
			MultiSelect: src.MultiSelect, AllowCustom: mode == "claude" || src.IsOther || len(src.Options) == 0, Required: true, InputType: inputType})
	}
	if !boundedQuestions(out) {
		return nil, errQuestionUnsupported
	}
	return out, nil
}

func boundedQuestions(qs []protocol.Question) bool {
	data, err := json.Marshal(qs)
	return err == nil && len(data) <= 64<<10
}

func boundedQuestionText(s string, max int) bool {
	return utf8.ValidString(s) && utf8.RuneCountInString(s) <= max && !strings.ContainsRune(s, '\x00')
}

func cloneQuestions(qs []protocol.Question) []protocol.Question {
	if qs == nil {
		return nil
	}
	out := append([]protocol.Question(nil), qs...)
	for i := range out {
		out[i].Options = append([]protocol.QuestionOption(nil), qs[i].Options...)
		if qs[i].Min != nil {
			n := *qs[i].Min
			out[i].Min = &n
		}
		if qs[i].Max != nil {
			n := *qs[i].Max
			out[i].Max = &n
		}
	}
	return out
}

func cloneAnswers(answers map[string][]string) map[string][]string {
	if answers == nil {
		return nil
	}
	out := make(map[string][]string, len(answers))
	for id, values := range answers {
		out[id] = append([]string(nil), values...)
	}
	return out
}

func validateQuestionAnswers(qs []protocol.Question, answers map[string][]string) error {
	known := map[string]bool{}
	for _, q := range qs {
		known[q.ID] = true
	}
	for id := range answers {
		if !known[id] {
			return errors.New("unknown question")
		}
	}
	total := 0
	for _, q := range qs {
		values := answers[q.ID]
		if len(values) == 0 {
			if q.Required {
				return errors.New("missing answer")
			}
			continue
		}
		if !q.MultiSelect && len(values) != 1 || len(values) > maxQuestionOptions {
			return errors.New("invalid answer count")
		}
		options := map[string]bool{}
		for _, opt := range q.Options {
			options[opt.Label] = true
		}
		seen := map[string]bool{}
		for _, value := range values {
			total += len(value)
			if !boundedQuestionText(value, maxAnswerText) || total > 64<<10 || seen[value] || q.Required && !q.AllowEmpty && strings.TrimSpace(value) == "" {
				return errors.New("invalid answer text")
			}
			seen[value] = true
			switch q.InputType {
			case "enum":
				if !options[value] && !q.AllowCustom {
					return errors.New("invalid option")
				}
			case "boolean":
				if value != "true" && value != "false" {
					return errors.New("invalid boolean")
				}
			case "number", "integer":
				n, err := strconv.ParseFloat(value, 64)
				if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || q.InputType == "integer" && (math.Trunc(n) != n || math.Abs(n) > 9007199254740991) || q.Min != nil && n < *q.Min || q.Max != nil && n > *q.Max {
					return errors.New("invalid number")
				}
			case "text":
			default:
				return errors.New("unsupported input type")
			}
		}
	}
	return nil
}

func (a *codexAgent) RespondWithAnswers(id string, response ApprovalResponse) bool {
	return a.aps.answer(id, approvalAnswer{decision: normalizeDecision(response.Decision), message: response.Message, by: "phone", answers: response.Answers})
}

func (a *claudeAgent) RespondWithAnswers(id string, response ApprovalResponse) bool {
	return a.aps.answer(id, approvalAnswer{decision: normalizeDecision(response.Decision), message: response.Message, by: "phone", answers: response.Answers})
}

type formRule struct {
	minLength, maxLength *int
	pattern              *regexp.Regexp
	format               string
}

type elicitationForm struct {
	questions []protocol.Question
	rules     map[string]formRule
}

// Only flat, non-secret primitive forms are relayed. Unsupported validation is
// never discarded: a complex schema stays on the computer instead.
func parseElicitationForm(raw json.RawMessage) (*elicitationForm, error) {
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil || !schemaKeys(root, "type", "properties", "required", "title", "description", "additionalProperties", "$schema") {
		return nil, errQuestionUnsupported
	}
	var typ string
	_ = json.Unmarshal(root["type"], &typ)
	if typ != "object" {
		return nil, errQuestionUnsupported
	}
	if ap := root["additionalProperties"]; len(ap) != 0 && string(ap) != "false" && string(ap) != "true" {
		return nil, errQuestionUnsupported
	}
	var props map[string]json.RawMessage
	if json.Unmarshal(root["properties"], &props) != nil || len(props) == 0 || len(props) > maxQuestions {
		return nil, errQuestionUnsupported
	}
	var required []string
	if len(root["required"]) != 0 && json.Unmarshal(root["required"], &required) != nil {
		return nil, errQuestionUnsupported
	}
	req := map[string]bool{}
	for _, id := range required {
		if _, ok := props[id]; !ok || req[id] {
			return nil, errQuestionUnsupported
		}
		req[id] = true
	}
	ids := make([]string, 0, len(props))
	for id := range props {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	f := &elicitationForm{rules: map[string]formRule{}}
	for _, id := range ids {
		var prop map[string]json.RawMessage
		if id == "" || !boundedQuestionText(id, maxQuestionID) || json.Unmarshal(props[id], &prop) != nil || !schemaKeys(prop, "type", "title", "description", "default", "enum", "enumNames", "oneOf", "minLength", "maxLength", "pattern", "format", "minimum", "maximum") {
			return nil, errQuestionUnsupported
		}
		var title, description, inputType string
		if !schemaString(prop, "title", &title) || !schemaString(prop, "description", &description) || !schemaString(prop, "type", &inputType) || !boundedQuestionText(title, 80) || !boundedQuestionText(description, maxQuestionText) {
			return nil, errQuestionUnsupported
		}
		if secretFormField(id + " " + title + " " + description) {
			return nil, errQuestionUnsupported
		}
		question := description
		if question == "" {
			question = title
		}
		if question == "" {
			question = id
		}
		if title == "" {
			title = id
		}
		q := protocol.Question{ID: id, Header: title, Question: question, Required: req[id], InputType: inputType}
		rule := formRule{}
		switch inputType {
		case "string":
			q.InputType = "text"
			q.PreserveWhitespace = true
			q.AllowEmpty = true
			if len(prop["enum"]) != 0 && len(prop["oneOf"]) != 0 {
				return nil, errQuestionUnsupported
			}
			if len(prop["enum"]) != 0 {
				var choices []string
				if json.Unmarshal(prop["enum"], &choices) != nil {
					return nil, errQuestionUnsupported
				}
				var names []string
				if len(prop["enumNames"]) != 0 && string(prop["enumNames"]) != "null" && (json.Unmarshal(prop["enumNames"], &names) != nil || len(names) != len(choices)) {
					return nil, errQuestionUnsupported
				}
				for i, choice := range choices {
					label := ""
					if len(names) != 0 {
						label = names[i]
					}
					q.Options = append(q.Options, protocol.QuestionOption{Label: choice, Description: label})
				}
			} else if len(prop["oneOf"]) != 0 {
				var choices []map[string]json.RawMessage
				if json.Unmarshal(prop["oneOf"], &choices) != nil {
					return nil, errQuestionUnsupported
				}
				for _, choice := range choices {
					var value, label string
					if !schemaKeys(choice, "const", "title") || !schemaString(choice, "const", &value) || !schemaString(choice, "title", &label) {
						return nil, errQuestionUnsupported
					}
					q.Options = append(q.Options, protocol.QuestionOption{Label: value, Description: label})
				}
			}
			if anySchemaKey(prop, "enumNames") && !anySchemaKey(prop, "enum") {
				return nil, errQuestionUnsupported
			}
			if len(prop["enum"]) != 0 || len(prop["oneOf"]) != 0 {
				if len(q.Options) == 0 || len(q.Options) > maxQuestionOptions {
					return nil, errQuestionUnsupported
				}
				labels := map[string]bool{}
				for _, opt := range q.Options {
					if labels[opt.Label] || !boundedQuestionText(opt.Label, 1024) || !boundedQuestionText(opt.Description, maxQuestionText) {
						return nil, errQuestionUnsupported
					}
					labels[opt.Label] = true
				}
				q.InputType = "enum"
			}
			if !schemaInt(prop, "minLength", &rule.minLength) || !schemaInt(prop, "maxLength", &rule.maxLength) {
				return nil, errQuestionUnsupported
			}
			if rule.minLength != nil && *rule.minLength > maxAnswerText || rule.maxLength != nil && rule.minLength != nil && *rule.maxLength < *rule.minLength {
				return nil, errQuestionUnsupported
			}
			var pattern string
			if !schemaString(prop, "pattern", &pattern) || len(pattern) > 1024 || !schemaString(prop, "format", &rule.format) {
				return nil, errQuestionUnsupported
			}
			if pattern != "" {
				var err error
				rule.pattern, err = regexp.Compile(pattern)
				if err != nil {
					return nil, errQuestionUnsupported
				}
			}
			if rule.format != "" && rule.format != "email" && rule.format != "uri" && rule.format != "date" && rule.format != "date-time" {
				return nil, errQuestionUnsupported
			}
		case "number", "integer":
			if !schemaNumber(prop, "minimum", &q.Min) || !schemaNumber(prop, "maximum", &q.Max) || q.Min != nil && q.Max != nil && *q.Min > *q.Max {
				return nil, errQuestionUnsupported
			}
		case "boolean":
		default:
			return nil, errQuestionUnsupported
		}
		// Validation belonging to a different primitive is unsupported, rather
		// than silently ignored after rendering a looser phone control.
		if inputType != "string" && anySchemaKey(prop, "enum", "enumNames", "oneOf", "minLength", "maxLength", "pattern", "format") || inputType != "number" && inputType != "integer" && anySchemaKey(prop, "minimum", "maximum") {
			return nil, errQuestionUnsupported
		}
		f.questions = append(f.questions, q)
		f.rules[id] = rule
	}
	if !boundedQuestions(f.questions) {
		return nil, errQuestionUnsupported
	}
	return f, nil
}

func schemaKeys(m map[string]json.RawMessage, keys ...string) bool {
	allowed := map[string]bool{}
	for _, key := range keys {
		allowed[key] = true
	}
	for key := range m {
		if !allowed[key] {
			return false
		}
	}
	return true
}
func anySchemaKey(m map[string]json.RawMessage, keys ...string) bool {
	for _, key := range keys {
		if len(m[key]) != 0 && string(m[key]) != "null" {
			return true
		}
	}
	return false
}
func schemaString(m map[string]json.RawMessage, key string, dst *string) bool {
	return len(m[key]) == 0 || json.Unmarshal(m[key], dst) == nil
}
func schemaInt(m map[string]json.RawMessage, key string, dst **int) bool {
	if len(m[key]) == 0 || string(m[key]) == "null" {
		return true
	}
	var n int
	if json.Unmarshal(m[key], &n) != nil || n < 0 {
		return false
	}
	*dst = &n
	return true
}
func schemaNumber(m map[string]json.RawMessage, key string, dst **float64) bool {
	if len(m[key]) == 0 || string(m[key]) == "null" {
		return true
	}
	var n float64
	if json.Unmarshal(m[key], &n) != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return false
	}
	*dst = &n
	return true
}
func secretFormField(text string) bool {
	compact := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(text))
	for _, forbidden := range []string{"password", "passwd", "apikey", "accesstoken", "authtoken", "bearertoken", "secret", "credential", "creditcard", "cvv", "密码", "密钥", "口令", "支付凭证"} {
		if strings.Contains(compact, forbidden) {
			return true
		}
	}
	return false
}

func (f *elicitationForm) validate(answers map[string][]string) error {
	if err := validateQuestionAnswers(f.questions, answers); err != nil {
		return err
	}
	for _, q := range f.questions {
		values := answers[q.ID]
		if len(values) == 0 {
			continue
		}
		value, rule := values[0], f.rules[q.ID]
		length := utf8.RuneCountInString(value)
		if rule.minLength != nil && length < *rule.minLength || rule.maxLength != nil && length > *rule.maxLength || rule.pattern != nil && !rule.pattern.MatchString(value) {
			return errors.New("invalid string")
		}
		switch rule.format {
		case "email":
			if address, err := mail.ParseAddress(value); err != nil || address.Address != value {
				return errors.New("invalid email")
			}
		case "uri":
			if u, err := url.Parse(value); err != nil || u.Scheme == "" {
				return errors.New("invalid uri")
			}
		case "date":
			if _, err := time.Parse("2006-01-02", value); err != nil {
				return errors.New("invalid date")
			}
		case "date-time":
			if _, err := time.Parse(time.RFC3339, value); err != nil {
				return errors.New("invalid date-time")
			}
		}
	}
	return nil
}

func (f *elicitationForm) content(answers map[string][]string) (map[string]any, error) {
	if err := f.validate(answers); err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, q := range f.questions {
		values := answers[q.ID]
		if len(values) == 0 {
			continue
		}
		switch q.InputType {
		case "boolean":
			out[q.ID] = values[0] == "true"
		case "number", "integer":
			n, _ := strconv.ParseFloat(values[0], 64)
			out[q.ID] = n
		default:
			out[q.ID] = values[0]
		}
	}
	return out, nil
}

func questionEvent(mode string, questions []protocol.Question, title, detail, cwd string) protocol.Event {
	return protocol.Event{Kind: "question", Title: title, Detail: detail, Cwd: cwd, QuestionMode: mode, Questions: questions}
}

func unsupportedQuestionNotice(sk, source string) protocol.Event {
	return protocol.Event{SessionKey: sk, Type: "notice", Level: "warning", Text: fmt.Sprintf("%s：这个问题请在电脑上处理", source)}
}
