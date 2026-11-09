package apiservice

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

var errNativeRequest = errors.New("unsupported or invalid native request")

// Reject duplicate/case-ambiguous fields before decoding tagged Go structs.
// Unknown protocol options must not disappear in a cross-protocol translation.
func decodeNative(body []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(body))
	var value func() error
	value = func() error {
		token, err := d.Token()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'):
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok {
					return errNativeRequest
				}
				name = strings.ToLower(name)
				if seen[name] {
					return errNativeRequest
				}
				seen[name] = true
				if err := value(); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		case json.Delim('['):
			for d.More() {
				if err := value(); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		}
		return nil
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errNativeRequest
	}
	d = json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	return d.Decode(out)
}

type nativeFunction struct {
	Name                 string          `json:"name"`
	Description          string          `json:"description,omitempty"`
	Parameters           json.RawMessage `json:"parameters,omitempty"`
	ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema,omitempty"`
}
type geminiPart struct {
	Text       *string `json:"text,omitempty"`
	InlineData *struct {
		MIME string `json:"mimeType"`
		Data string `json:"data"`
	} `json:"inlineData,omitempty"`
	Call *struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
		ID   string          `json:"id,omitempty"`
	} `json:"functionCall,omitempty"`
	Result *struct {
		Name     string          `json:"name"`
		Response json.RawMessage `json:"response"`
		ID       string          `json:"id,omitempty"`
	} `json:"functionResponse,omitempty"`
}
type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}
type geminiRequest struct {
	Contents []geminiContent `json:"contents"`
	System   *geminiContent  `json:"systemInstruction,omitempty"`
	Tools    []struct {
		Functions []nativeFunction `json:"functionDeclarations"`
	} `json:"tools,omitempty"`
	ToolConfig *struct {
		Function struct {
			Mode  string   `json:"mode"`
			Names []string `json:"allowedFunctionNames,omitempty"`
		} `json:"functionCallingConfig"`
	} `json:"toolConfig,omitempty"`
	Config *struct {
		Temperature *float64        `json:"temperature,omitempty"`
		TopP        *float64        `json:"topP,omitempty"`
		MaxTokens   *int            `json:"maxOutputTokens,omitempty"`
		Seed        *int            `json:"seed,omitempty"`
		Candidates  *int            `json:"candidateCount,omitempty"`
		Stop        []string        `json:"stopSequences,omitempty"`
		MIME        string          `json:"responseMimeType,omitempty"`
		Schema      json.RawMessage `json:"responseSchema,omitempty"`
		JSONSchema  json.RawMessage `json:"responseJsonSchema,omitempty"`
	} `json:"generationConfig,omitempty"`
}

func nativeSchema(raw json.RawMessage) (any, error) {
	var schema map[string]any
	if len(raw) == 0 {
		return map[string]any{"type": "object", "properties": map[string]any{}}, nil
	}
	if json.Unmarshal(raw, &schema) != nil || schema == nil {
		return nil, errNativeRequest
	}
	var normalize func(any)
	normalize = func(v any) {
		switch item := v.(type) {
		case map[string]any:
			if name, ok := item["type"].(string); ok {
				item["type"] = strings.ToLower(name)
			}
			// Only schema-bearing keywords contain more schemas. Values in
			// defaults, consts, enums and examples are user data, even when
			// they contain a property called "type".
			for _, key := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"} {
				if children, ok := item[key].(map[string]any); ok {
					for _, child := range children {
						normalize(child)
					}
				}
			}
			for _, key := range []string{"items", "prefixItems", "contains", "additionalItems", "additionalProperties", "unevaluatedItems", "unevaluatedProperties", "propertyNames", "not", "if", "then", "else", "allOf", "anyOf", "oneOf"} {
				normalize(item[key])
			}
		case []any:
			for _, child := range item {
				normalize(child)
			}
		}
	}
	normalize(schema)
	return schema, nil
}
func nativeImage(mime, data string) (map[string]any, error) {
	if mime != "image/png" && mime != "image/jpeg" && mime != "image/webp" && mime != "image/gif" {
		return nil, errNativeRequest
	}
	if data == "" {
		return nil, errNativeRequest
	}
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		return nil, errNativeRequest
	}
	return map[string]any{"type": "image_url", "image_url": map[string]string{"url": "data:" + mime + ";base64," + data}}, nil
}

func (route *nativeRoute) request(body []byte) ([]byte, error) {
	if route.kind == "gemini" {
		return route.geminiRequest(body)
	}
	return route.ollamaRequest(body)
}
func (route *nativeRoute) geminiRequest(body []byte) ([]byte, error) {
	var input geminiRequest
	if err := decodeNative(body, &input); err != nil || len(input.Contents) == 0 {
		return nil, errNativeRequest
	}
	messages := []any{}
	if input.System != nil {
		if input.System.Role != "" && input.System.Role != "system" {
			return nil, errNativeRequest
		}
		parts := []string{}
		for _, p := range input.System.Parts {
			if p.Text == nil || p.InlineData != nil || p.Call != nil || p.Result != nil {
				return nil, errNativeRequest
			}
			parts = append(parts, *p.Text)
		}
		messages = append(messages, map[string]any{"role": "system", "content": strings.Join(parts, "\n")})
	}
	// Native function results can omit IDs. A name is sufficient only when
	// exactly one matching call is outstanding; ambiguous histories are rejected.
	pending := map[string]string{}
	seenIDs := map[string]bool{}
	for i, content := range input.Contents {
		role := content.Role
		if role == "" {
			role = "user"
		}
		if role == "model" {
			role = "assistant"
		}
		if role != "user" && role != "assistant" || len(content.Parts) == 0 {
			return nil, errNativeRequest
		}
		parts, calls := []any{}, []any{}
		flush := func() {
			if len(parts) > 0 || len(calls) > 0 {
				m := map[string]any{"role": role, "content": parts}
				if len(calls) > 0 {
					m["tool_calls"] = calls
				}
				messages = append(messages, m)
				parts, calls = []any{}, []any{}
			}
		}
		for j, p := range content.Parts {
			count := 0
			if p.Text != nil {
				count++
			}
			if p.InlineData != nil {
				count++
			}
			if p.Call != nil {
				count++
			}
			if p.Result != nil {
				count++
			}
			if count != 1 {
				return nil, errNativeRequest
			}
			switch {
			case p.Text != nil:
				parts = append(parts, map[string]any{"type": "text", "text": *p.Text})
			case p.InlineData != nil:
				if role != "user" {
					return nil, errNativeRequest
				}
				image, err := nativeImage(p.InlineData.MIME, p.InlineData.Data)
				if err != nil {
					return nil, err
				}
				parts = append(parts, image)
			case p.Call != nil:
				if role != "assistant" || p.Call.Name == "" {
					return nil, errNativeRequest
				}
				var args map[string]any
				if json.Unmarshal(p.Call.Args, &args) != nil || args == nil {
					return nil, errNativeRequest
				}
				id := p.Call.ID
				if id == "" {
					id = fmt.Sprintf("gemini_%d_%d", i, j)
				}
				if seenIDs[id] {
					return nil, errNativeRequest
				}
				seenIDs[id] = true
				pending[id] = p.Call.Name
				calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": p.Call.Name, "arguments": string(p.Call.Args)}})
			case p.Result != nil:
				if role != "user" {
					return nil, errNativeRequest
				}
				flush()
				id := p.Result.ID
				if id == "" {
					for candidate, name := range pending {
						if name == p.Result.Name {
							if id != "" {
								return nil, errNativeRequest
							}
							id = candidate
						}
					}
				}
				if id == "" || pending[id] != p.Result.Name {
					return nil, errNativeRequest
				}
				var result map[string]any
				if json.Unmarshal(p.Result.Response, &result) != nil || result == nil {
					return nil, errNativeRequest
				}
				delete(pending, id)
				messages = append(messages, map[string]any{"role": "tool", "tool_call_id": id, "content": string(p.Result.Response)})
			}
		}
		flush()
	}
	out := map[string]any{"model": route.model, "messages": messages, "stream": route.stream}
	if route.stream {
		out["stream_options"] = map[string]bool{"include_usage": true}
	}
	tools := []any{}
	for _, group := range input.Tools {
		for _, fn := range group.Functions {
			if fn.Name == "" || len(fn.Parameters) > 0 && len(fn.ParametersJSONSchema) > 0 {
				return nil, errNativeRequest
			}
			raw := fn.Parameters
			if len(fn.ParametersJSONSchema) > 0 {
				raw = fn.ParametersJSONSchema
			}
			schema, err := nativeSchema(raw)
			if err != nil {
				return nil, err
			}
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": fn.Name, "description": fn.Description, "parameters": schema}})
		}
	}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	if input.ToolConfig != nil {
		f := input.ToolConfig.Function
		switch f.Mode {
		case "AUTO", "NONE":
			if len(f.Names) > 0 {
				return nil, errNativeRequest
			}
			out["tool_choice"] = strings.ToLower(f.Mode)
		case "ANY":
			if len(f.Names) > 1 {
				return nil, errNativeRequest
			}
			out["tool_choice"] = "required"
			if len(f.Names) == 1 {
				out["tool_choice"] = map[string]any{"type": "function", "function": map[string]string{"name": f.Names[0]}}
			}
		default:
			return nil, errNativeRequest
		}
	}
	if c := input.Config; c != nil {
		if c.Candidates != nil && *c.Candidates != 1 {
			return nil, errNativeRequest
		}
		if c.Temperature != nil {
			out["temperature"] = *c.Temperature
		}
		if c.TopP != nil {
			out["top_p"] = *c.TopP
		}
		if c.MaxTokens != nil {
			out["max_tokens"] = *c.MaxTokens
		}
		if c.Seed != nil {
			out["seed"] = *c.Seed
		}
		if c.Stop != nil {
			out["stop"] = c.Stop
		}
		if len(c.Schema) > 0 && len(c.JSONSchema) > 0 {
			return nil, errNativeRequest
		}
		raw := c.Schema
		if len(c.JSONSchema) > 0 {
			raw = c.JSONSchema
		}
		if c.MIME != "" && c.MIME != "text/plain" && c.MIME != "application/json" || len(raw) > 0 && c.MIME != "application/json" {
			return nil, errNativeRequest
		}
		if c.MIME == "application/json" {
			format := map[string]any{"type": "json_object"}
			if len(raw) > 0 {
				schema, err := nativeSchema(raw)
				if err != nil {
					return nil, err
				}
				format = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "response", "schema": schema}}
			}
			out["response_format"] = format
		}
	}
	return json.Marshal(out)
}

type ollamaMessage struct {
	Role     string   `json:"role"`
	Content  string   `json:"content"`
	Images   []string `json:"images,omitempty"`
	ToolName string   `json:"tool_name,omitempty"`
	Calls    []struct {
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls,omitempty"`
}
type ollamaRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages,omitempty"`
	Prompt   *string         `json:"prompt,omitempty"`
	System   *string         `json:"system,omitempty"`
	Images   []string        `json:"images,omitempty"`
	Stream   *bool           `json:"stream,omitempty"`
	Tools    []struct {
		Type     string         `json:"type"`
		Function nativeFunction `json:"function"`
	} `json:"tools,omitempty"`
	Format  json.RawMessage `json:"format,omitempty"`
	Options *struct {
		Temperature *float64 `json:"temperature,omitempty"`
		TopP        *float64 `json:"top_p,omitempty"`
		MaxTokens   *int     `json:"num_predict,omitempty"`
		Seed        *int     `json:"seed,omitempty"`
		Stop        []string `json:"stop,omitempty"`
	} `json:"options,omitempty"`
}

func (route *nativeRoute) ollamaRequest(body []byte) ([]byte, error) {
	var input ollamaRequest
	if decodeNative(body, &input) != nil || input.Model == "" {
		return nil, errNativeRequest
	}
	route.model = input.Model
	route.stream = input.Stream == nil || *input.Stream
	if route.generate {
		if input.Prompt == nil || len(input.Messages) > 0 || len(input.Tools) > 0 {
			return nil, errNativeRequest
		}
		if input.System != nil {
			input.Messages = append(input.Messages, ollamaMessage{Role: "system", Content: *input.System})
		}
		input.Messages = append(input.Messages, ollamaMessage{Role: "user", Content: *input.Prompt, Images: input.Images})
	} else if len(input.Messages) == 0 || input.Prompt != nil || input.System != nil || len(input.Images) > 0 {
		return nil, errNativeRequest
	}
	messages := []any{}
	pending := map[string]string{}
	for i, m := range input.Messages {
		if m.Role != "user" && m.Role != "assistant" && m.Role != "system" && m.Role != "tool" {
			return nil, errNativeRequest
		}
		row := map[string]any{"role": m.Role, "content": m.Content}
		if len(m.Images) > 0 {
			if m.Role != "user" {
				return nil, errNativeRequest
			}
			parts := []any{map[string]any{"type": "text", "text": m.Content}}
			for _, data := range m.Images {
				decoded, err := base64.StdEncoding.DecodeString(data)
				if err != nil {
					return nil, errNativeRequest
				}
				mime := ""
				switch {
				case bytes.HasPrefix(decoded, []byte("\x89PNG")):
					mime = "image/png"
				case bytes.HasPrefix(decoded, []byte("\xff\xd8\xff")):
					mime = "image/jpeg"
				case bytes.HasPrefix(decoded, []byte("GIF8")):
					mime = "image/gif"
				case len(decoded) > 12 && string(decoded[:4]) == "RIFF" && string(decoded[8:12]) == "WEBP":
					mime = "image/webp"
				}
				image, err := nativeImage(mime, data)
				if err != nil {
					return nil, err
				}
				parts = append(parts, image)
			}
			row["content"] = parts
		}
		if len(m.Calls) > 0 {
			if m.Role != "assistant" {
				return nil, errNativeRequest
			}
			calls := []any{}
			for j, c := range m.Calls {
				var args map[string]any
				if c.Function.Name == "" || json.Unmarshal(c.Function.Arguments, &args) != nil || args == nil {
					return nil, errNativeRequest
				}
				id := fmt.Sprintf("ollama_%d_%d", i, j)
				pending[id] = c.Function.Name
				calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": c.Function.Name, "arguments": string(c.Function.Arguments)}})
			}
			row["tool_calls"] = calls
		}
		if m.Role == "tool" {
			id := ""
			for candidate, name := range pending {
				if m.ToolName == "" || name == m.ToolName {
					if id != "" {
						return nil, errNativeRequest
					}
					id = candidate
				}
			}
			if id == "" {
				return nil, errNativeRequest
			}
			row["tool_call_id"] = id
			delete(pending, id)
		} else if m.ToolName != "" {
			return nil, errNativeRequest
		}
		messages = append(messages, row)
	}
	out := map[string]any{"model": route.model, "messages": messages, "stream": route.stream}
	if route.stream {
		out["stream_options"] = map[string]bool{"include_usage": true}
	}
	if len(input.Tools) > 0 {
		tools := []any{}
		for _, tool := range input.Tools {
			if tool.Type != "function" || tool.Function.Name == "" || len(tool.Function.ParametersJSONSchema) > 0 {
				return nil, errNativeRequest
			}
			schema, err := nativeSchema(tool.Function.Parameters)
			if err != nil {
				return nil, err
			}
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": tool.Function.Name, "description": tool.Function.Description, "parameters": schema}})
		}
		out["tools"] = tools
	}
	if c := input.Options; c != nil {
		if c.Temperature != nil {
			out["temperature"] = *c.Temperature
		}
		if c.TopP != nil {
			out["top_p"] = *c.TopP
		}
		if c.MaxTokens != nil {
			if *c.MaxTokens < 1 {
				return nil, errNativeRequest
			}
			out["max_tokens"] = *c.MaxTokens
		}
		if c.Seed != nil {
			out["seed"] = *c.Seed
		}
		if c.Stop != nil {
			out["stop"] = c.Stop
		}
	}
	if len(input.Format) > 0 && string(input.Format) != `""` {
		if string(input.Format) == `"json"` {
			out["response_format"] = map[string]any{"type": "json_object"}
		} else {
			schema, err := nativeSchema(input.Format)
			if err != nil {
				return nil, err
			}
			out["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "response", "schema": schema}}
		}
	}
	return json.Marshal(out)
}
