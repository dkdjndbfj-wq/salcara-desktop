package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// ValidChatEnvelope rejects ambiguous authorization-bearing fields before the
// named service resolves aliases. Native Chat payloads otherwise pass through.
func ValidChatEnvelope(raw []byte) bool {
	if !chatUniqueJSON(raw) {
		return false
	}
	v := chatObject(raw)
	if v == nil {
		return false
	}
	for key := range v {
		for _, canonical := range []string{"model", "messages", "stream", "tools", "tool_choice", "n", "stream_options", "parallel_tool_calls", "response_format", "reasoning_effort"} {
			if strings.EqualFold(key, canonical) && key != canonical {
				return false
			}
		}
	}
	var model string
	if json.Unmarshal(v["model"], &model) != nil || model == "" {
		return false
	}
	var messages []json.RawMessage
	if json.Unmarshal(v["messages"], &messages) != nil || len(messages) == 0 {
		return false
	}
	if stream, ok := v["stream"]; ok {
		var value bool
		if bytes.Equal(bytes.TrimSpace(stream), []byte("null")) || json.Unmarshal(stream, &value) != nil {
			return false
		}
	}
	for _, rawMessage := range messages {
		message := chatObject(rawMessage)
		var role string
		if message == nil || json.Unmarshal(message["role"], &role) != nil || role == "" {
			return false
		}
	}
	return true
}
func chatObject(raw []byte) map[string]json.RawMessage {
	var v map[string]json.RawMessage
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}
func chatText(v map[string]json.RawMessage, key string) string {
	var out string
	_ = json.Unmarshal(v[key], &out)
	return out
}
func chatOnly(v map[string]json.RawMessage, keys ...string) bool {
	if v == nil {
		return false
	}
	for key := range v {
		found := false
		for _, allowed := range keys {
			found = found || key == allowed
		}
		if !found {
			return false
		}
	}
	return true
}
func chatString(raw []byte) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
func chatExactBool(raw []byte, value bool) bool {
	var got bool
	return json.Unmarshal(raw, &got) == nil && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && got == value
}

func validateChatRequest(raw []byte, upstream translator.Format) (int, string) {
	if !ValidChatEnvelope(raw) {
		return http.StatusBadRequest, "Chat 请求需要明确的 model 和完整 messages，字段不能重复或混用大小写"
	}
	if upstream == translator.FormatOpenAI {
		return 0, ""
	}
	// The installed SDK intentionally drops sampling/token limits and emits only
	// one Chat choice for Responses. Reject unsupported intent before executing.
	v := chatObject(raw)
	unsupported := func() (int, string) {
		return http.StatusNotImplemented, "当前上游的 Chat 转换不支持这些请求选项；请使用原生接口或受支持的完整消息/函数工具请求"
	}
	allowed := []string{"model", "messages", "stream", "tools", "tool_choice", "n", "stream_options", "parallel_tool_calls"}
	if upstream == translator.FormatCodex {
		allowed = append(allowed, "reasoning_effort", "response_format")
	}
	if !chatOnly(v, allowed...) {
		return unsupported()
	}
	if value, ok := v["n"]; ok {
		var n int
		if json.Unmarshal(value, &n) != nil || n != 1 {
			return unsupported()
		}
	}
	if value, ok := v["parallel_tool_calls"]; ok && !chatExactBool(value, true) {
		return unsupported()
	}
	if value, ok := v["stream_options"]; ok {
		options := chatObject(value)
		if !chatOnly(options, "include_usage") || !chatExactBool(options["include_usage"], true) || !chatExactBool(v["stream"], true) {
			return unsupported()
		}
	}
	if value, ok := v["reasoning_effort"]; ok {
		if !chatString(value) || chatText(v, "reasoning_effort") == "" {
			return http.StatusBadRequest, "reasoning_effort 需要明确的文字值"
		}
	}
	if value, ok := v["response_format"]; ok {
		format := chatObject(value)
		kind := chatText(format, "type")
		switch kind {
		case "text":
			if !chatOnly(format, "type") {
				return unsupported()
			}
		case "json_schema":
			schema := chatObject(format["json_schema"])
			if !chatOnly(format, "type", "json_schema") || !chatOnly(schema, "name", "schema", "strict") || chatText(schema, "name") == "" || chatObject(schema["schema"]) == nil {
				return unsupported()
			}
			if strict, exists := schema["strict"]; exists && !chatExactBool(strict, true) && !chatExactBool(strict, false) {
				return unsupported()
			}
		default:
			return unsupported()
		}
	}
	var tools []json.RawMessage
	if value, ok := v["tools"]; ok {
		if json.Unmarshal(value, &tools) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return http.StatusBadRequest, "tools 需要数组"
		}
	}
	declared := map[string]bool{}
	for _, rawTool := range tools {
		tool := chatObject(rawTool)
		fn := chatObject(tool["function"])
		if !chatOnly(tool, "type", "function") || chatText(tool, "type") != "function" || !chatOnly(fn, "name", "description", "parameters", "strict") || chatText(fn, "name") == "" {
			return unsupported()
		}
		if declared[chatText(fn, "name")] {
			return http.StatusBadRequest, "函数工具名称不能重复"
		}
		declared[chatText(fn, "name")] = true
		if value, ok := fn["description"]; ok && !chatString(value) {
			return unsupported()
		}
		if value, ok := fn["parameters"]; ok && chatObject(value) == nil {
			return unsupported()
		}
		if value, ok := fn["parameters"]; ok && upstream == translator.FormatClaude {
			schema := chatObject(value)
			if kind, exists := schema["type"]; exists && (!chatString(kind) || chatText(schema, "type") != "object") {
				return unsupported()
			}
			for _, union := range []string{"anyOf", "oneOf", "allOf"} {
				if _, exists := schema[union]; exists {
					return unsupported()
				}
			}
			if properties, exists := schema["properties"]; exists && chatObject(properties) == nil {
				return unsupported()
			}
		}
		if value, ok := fn["strict"]; ok && !chatExactBool(value, true) && !chatExactBool(value, false) {
			return unsupported()
		}
		if _, ok := fn["strict"]; ok && upstream == translator.FormatClaude {
			return unsupported()
		}
	}
	if value, ok := v["tool_choice"]; ok {
		var choice string
		if json.Unmarshal(value, &choice) == nil {
			if choice != "auto" && choice != "none" && choice != "required" {
				return unsupported()
			}
			// This SDK omits Claude tool_choice for "none" while retaining the
			// tool declarations, which would allow a model to call them anyway.
			if choice == "none" && upstream == translator.FormatClaude {
				return unsupported()
			}
		} else {
			choice := chatObject(value)
			fn := chatObject(choice["function"])
			if !chatOnly(choice, "type", "function") || chatText(choice, "type") != "function" || !chatOnly(fn, "name") || !declared[chatText(fn, "name")] {
				return unsupported()
			}
		}
	}
	var messages []map[string]json.RawMessage
	_ = json.Unmarshal(v["messages"], &messages)
	pending := map[string]bool{}
	seenCallIDs := map[string]bool{}
	for _, message := range messages {
		if !chatOnly(message, "role", "content", "tool_calls", "tool_call_id") {
			return unsupported()
		}
		role := chatText(message, "role")
		if role != "system" && role != "developer" && role != "user" && role != "assistant" && role != "tool" {
			return unsupported()
		}
		if role == "tool" {
			id := chatText(message, "tool_call_id")
			if !pending[id] || !chatString(message["content"]) {
				return http.StatusBadRequest, "工具结果需要紧随完整 assistant 工具调用，且每个调用只能对应一个结果"
			}
			delete(pending, id)
			if _, ok := message["tool_calls"]; ok {
				return unsupported()
			}
			continue
		}
		pending = map[string]bool{}
		if _, ok := message["tool_call_id"]; ok {
			return unsupported()
		}
		content, exists := message["content"]
		null := bytes.Equal(bytes.TrimSpace(content), []byte("null"))
		if exists && !null && !chatString(content) {
			var parts []map[string]json.RawMessage
			if json.Unmarshal(content, &parts) != nil {
				return unsupported()
			}
			for _, part := range parts {
				switch chatText(part, "type") {
				case "text":
					if !chatOnly(part, "type", "text") || !chatString(part["text"]) {
						return unsupported()
					}
				case "image_url":
					image := chatObject(part["image_url"])
					if role != "user" || !chatOnly(part, "type", "image_url") || !chatOnly(image, "url") || chatText(image, "url") == "" {
						return unsupported()
					}
					if upstream == translator.FormatClaude && strings.HasPrefix(chatText(image, "url"), "data:") && !strings.Contains(chatText(image, "url"), ",") {
						return unsupported()
					}
				default:
					return unsupported()
				}
			}
		}
		callsRaw, hasCalls := message["tool_calls"]
		if !exists || null {
			if role != "assistant" || !hasCalls {
				return http.StatusBadRequest, "消息需要完整 content 或 assistant 工具调用"
			}
		}
		if hasCalls {
			if role != "assistant" {
				return unsupported()
			}
			var calls []map[string]json.RawMessage
			if json.Unmarshal(callsRaw, &calls) != nil || len(calls) == 0 {
				return unsupported()
			}
			for _, call := range calls {
				fn := chatObject(call["function"])
				id := chatText(call, "id")
				if !chatOnly(call, "id", "type", "function") || chatText(call, "type") != "function" || !chatOnly(fn, "name", "arguments") || chatText(fn, "name") == "" || !chatString(fn["arguments"]) || id == "" || pending[id] {
					return http.StatusBadRequest, "assistant 工具调用需要唯一 ID、函数名称和完整参数"
				}
				if upstream == translator.FormatClaude {
					// Anthropic only accepts object arguments; this SDK substitutes
					// {} for any other value and sanitizes IDs before matching them.
					if chatObject([]byte(chatText(fn, "arguments"))) == nil || !chatSafeToolID(id) {
						return unsupported()
					}
					// This SDK resolves tool results by ID across the whole input;
					// reusing an ID in another batch would replace earlier history.
					if seenCallIDs[id] {
						return http.StatusBadRequest, "完整消息历史中的工具调用 ID 不能重复"
					}
				}
				seenCallIDs[id] = true
				pending[id] = true
			}
		}
	}
	return 0, ""
}

func chatSafeToolID(id string) bool {
	for _, ch := range id {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-') {
			return false
		}
	}
	return id != ""
}

func chatStreamFailure(raw []byte) (string, bool) {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		var event map[string]any
		if json.Unmarshal(data, &event) == nil && (event["error"] != nil || event["type"] == "error" || event["type"] == "response.failed" || event["type"] == "response.incomplete") {
			return providerExecutionCode(data), true
		}
	}
	return "", false
}

// repairCodexChatNonStream restores Responses refusal content that the pinned
// SDK's Codex -> Chat translator currently omits. A detected refusal must be
// represented in the Chat message; returning nil makes the existing gateway
// validity check fail closed if the translated envelope cannot be amended.
func repairCodexChatNonStream(upstream, translated []byte) []byte {
	refusals, found, valid := codexTerminalRefusals(upstream)
	if !valid {
		return nil
	}
	if !found {
		return translated
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(translated, &root) != nil {
		return nil
	}
	var choices []map[string]json.RawMessage
	if json.Unmarshal(root["choices"], &choices) != nil || len(choices) == 0 {
		return nil
	}
	var message map[string]json.RawMessage
	if json.Unmarshal(choices[0]["message"], &message) != nil || message == nil {
		return nil
	}
	value, err := json.Marshal(strings.Join(refusals, ""))
	if err != nil {
		return nil
	}
	message["refusal"] = value
	choices[0]["message"], err = json.Marshal(message)
	if err != nil {
		return nil
	}
	root["choices"], err = json.Marshal(choices)
	if err != nil {
		return nil
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil
	}
	return out
}

const maxChatRefusalBytes = 16 << 20

type chatRefusalStreamState struct {
	id      string
	created int64
	model   string
	parts   map[string]string
	total   int
}

// translateCodexChatRefusal turns Responses refusal events into Chat delta
// chunks. It also observes response.created metadata and reconciles done or
// terminal snapshots so a provider that omits delta events still cannot yield
// a blank successful Chat response.
func translateCodexChatRefusal(data []byte, fallbackModel string, state *chatRefusalStreamState) ([][]byte, bool) {
	if state == nil {
		return nil, false
	}
	var event struct {
		Type         string          `json:"type"`
		ItemID       string          `json:"item_id"`
		OutputIndex  int             `json:"output_index"`
		ContentIndex int             `json:"content_index"`
		Delta        json.RawMessage `json:"delta"`
		Refusal      json.RawMessage `json:"refusal"`
		Response     json.RawMessage `json:"response"`
	}
	if json.Unmarshal(data, &event) != nil {
		return nil, true
	}
	switch event.Type {
	case "response.created":
		var response struct {
			ID        string `json:"id"`
			CreatedAt int64  `json:"created_at"`
			Model     string `json:"model"`
		}
		if json.Unmarshal(event.Response, &response) != nil || !state.setMetadata(response.ID, response.CreatedAt, response.Model, fallbackModel) {
			return nil, false
		}
		return nil, true
	case "response.refusal.delta":
		var delta string
		if json.Unmarshal(event.Delta, &delta) != nil || !state.ready(fallbackModel) {
			return nil, false
		}
		key, ok := chatRefusalPartKey(event.ItemID, event.OutputIndex, event.ContentIndex)
		if !ok || !state.append(key, delta) {
			return nil, false
		}
		if delta == "" {
			return nil, true
		}
		return [][]byte{state.chunk(delta)}, true
	case "response.refusal.done":
		var refusal string
		if json.Unmarshal(event.Refusal, &refusal) != nil || !state.ready(fallbackModel) {
			return nil, false
		}
		key, ok := chatRefusalPartKey(event.ItemID, event.OutputIndex, event.ContentIndex)
		if !ok {
			return nil, false
		}
		return state.reconcile(key, refusal)
	case "response.completed", "response.incomplete":
		var response struct {
			ID        string            `json:"id"`
			CreatedAt int64             `json:"created_at"`
			Model     string            `json:"model"`
			Output    []json.RawMessage `json:"output"`
		}
		if json.Unmarshal(event.Response, &response) != nil || !state.setMetadata(response.ID, response.CreatedAt, response.Model, fallbackModel) {
			return nil, false
		}
		var chunks [][]byte
		for outputIndex, rawOutput := range response.Output {
			var output struct {
				ID      string            `json:"id"`
				Type    string            `json:"type"`
				Content []json.RawMessage `json:"content"`
			}
			if json.Unmarshal(rawOutput, &output) != nil || output.Type != "message" {
				continue
			}
			for contentIndex, rawContent := range output.Content {
				partType, refusal, present, valid := codexRefusalPart(rawContent)
				if !valid {
					return nil, false
				}
				if partType != "refusal" || !present {
					continue
				}
				key, ok := chatRefusalPartKey(output.ID, outputIndex, contentIndex)
				if !ok {
					return nil, false
				}
				added, ok := state.reconcile(key, refusal)
				if !ok {
					return nil, false
				}
				chunks = append(chunks, added...)
			}
		}
		return chunks, true
	default:
		return nil, true
	}
}

func codexTerminalRefusals(raw []byte) ([]string, bool, bool) {
	var event struct {
		Type     string `json:"type"`
		Response struct {
			Output []json.RawMessage `json:"output"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return nil, false, true
	}
	if event.Type != "response.completed" && event.Type != "response.incomplete" {
		return nil, false, true
	}
	refusals := []string{}
	total := 0
	for _, rawOutput := range event.Response.Output {
		var output struct {
			Type    string            `json:"type"`
			Content []json.RawMessage `json:"content"`
		}
		if json.Unmarshal(rawOutput, &output) != nil || output.Type != "message" {
			continue
		}
		for _, rawContent := range output.Content {
			partType, refusal, present, valid := codexRefusalPart(rawContent)
			if !valid {
				return nil, false, false
			}
			if partType != "refusal" || !present {
				continue
			}
			total += len(refusal)
			if total > maxChatRefusalBytes {
				return nil, false, false
			}
			refusals = append(refusals, refusal)
		}
	}
	return refusals, len(refusals) > 0, true
}

func codexRefusalPart(raw json.RawMessage) (partType, refusal string, present, valid bool) {
	var part map[string]json.RawMessage
	if json.Unmarshal(raw, &part) != nil {
		return "", "", false, true
	}
	if json.Unmarshal(part["type"], &partType) != nil || partType != "refusal" {
		return partType, "", false, true
	}
	rawRefusal, present := part["refusal"]
	if !present || json.Unmarshal(rawRefusal, &refusal) != nil {
		return partType, "", present, false
	}
	return partType, refusal, true, true
}

func chatRefusalPartKey(itemID string, outputIndex, contentIndex int) (string, bool) {
	if len(itemID) > 256 || strings.ContainsAny(itemID, "\r\n\x00") || outputIndex < 0 || contentIndex < 0 {
		return "", false
	}
	// Output/content indexes are stable across delta, done and terminal events.
	// Some compatible providers omit the output item's id in the terminal copy.
	return strconv.Itoa(outputIndex) + "\x00" + strconv.Itoa(contentIndex), true
}

func (s *chatRefusalStreamState) setMetadata(id string, created int64, model, fallback string) bool {
	if id == "" {
		id = s.id
	}
	if model == "" {
		model = fallback
	}
	if created == 0 && s.id != "" {
		created = s.created
	}
	if id == "" || len(id) > 256 || strings.ContainsAny(id, "\r\n\x00") || model == "" || len(model) > 200 || strings.TrimSpace(model) != model || strings.ContainsAny(model, "\r\n\x00") || created < 0 {
		return false
	}
	if s.id != "" && (s.id != id || s.model != model || s.created != created) {
		return false
	}
	s.id, s.created, s.model = id, created, model
	if s.parts == nil {
		s.parts = map[string]string{}
	}
	return true
}

func (s *chatRefusalStreamState) ready(fallback string) bool {
	return s.setMetadata(s.id, s.created, s.model, fallback)
}

func (s *chatRefusalStreamState) append(key, delta string) bool {
	if len(delta) > maxChatRefusalBytes-s.total {
		return false
	}
	s.parts[key] += delta
	s.total += len(delta)
	return true
}

func (s *chatRefusalStreamState) reconcile(key, final string) ([][]byte, bool) {
	previous := s.parts[key]
	if !strings.HasPrefix(final, previous) {
		return nil, false
	}
	delta := strings.TrimPrefix(final, previous)
	if !s.append(key, delta) {
		return nil, false
	}
	if delta == "" {
		return nil, true
	}
	return [][]byte{s.chunk(delta)}, true
}

func (s *chatRefusalStreamState) chunk(delta string) []byte {
	chunk, _ := json.Marshal(map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "refusal": delta}, "finish_reason": nil, "native_finish_reason": nil}},
	})
	return chunk
}

func chatUniqueJSON(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var walk func(int) bool
	walk = func(depth int) bool {
		if depth > 64 {
			return false
		}
		token, err := d.Token()
		if err != nil {
			return false
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return true
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				token, err := d.Token()
				key, ok := token.(string)
				if err != nil || !ok || seen[key] {
					return false
				}
				seen[key] = true
				if !walk(depth + 1) {
					return false
				}
			}
			token, err := d.Token()
			return err == nil && token == json.Delim('}')
		case '[':
			for d.More() {
				if !walk(depth + 1) {
					return false
				}
			}
			token, err := d.Token()
			return err == nil && token == json.Delim(']')
		}
		return false
	}
	if !walk(0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}
