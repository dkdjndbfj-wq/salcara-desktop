package apiservice

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

const maxNativeResponse = 32 << 20

type chatNativeCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type chatNativeMessage struct {
	Role      string           `json:"role"`
	Content   *string          `json:"content"`
	Reasoning string           `json:"reasoning_content"`
	Refusal   *string          `json:"refusal"`
	Calls     []chatNativeCall `json:"tool_calls"`
}
type chatNativeResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Index   int               `json:"index"`
		Message chatNativeMessage `json:"message"`
		Delta   chatNativeMessage `json:"delta"`
		Finish  *string           `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		Input  *int64 `json:"prompt_tokens"`
		Output *int64 `json:"completion_tokens"`
		Total  *int64 `json:"total_tokens"`
	} `json:"usage"`
	Error json.RawMessage `json:"error"`
}

func nativeError(kind string, code int, message string) any {
	if kind == "ollama" {
		return map[string]any{"error": message}
	}
	status := map[int]string{400: "INVALID_ARGUMENT", 401: "UNAUTHENTICATED", 403: "PERMISSION_DENIED", 404: "NOT_FOUND", 405: "INVALID_ARGUMENT", 409: "ABORTED", 413: "RESOURCE_EXHAUSTED", 429: "RESOURCE_EXHAUSTED", 501: "UNIMPLEMENTED", 502: "UNAVAILABLE", 503: "UNAVAILABLE"}[code]
	if status == "" {
		status = "INTERNAL"
	}
	return map[string]any{"error": map[string]any{"code": code, "message": message, "status": status}}
}

// Converts early authentication/admission errors too; it never buffers a body.
type nativeErrorWriter struct {
	http.ResponseWriter
	kind   string
	status int
}

func (w *nativeErrorWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}
func (w *nativeErrorWriter) Write(b []byte) (int, error) {
	if w.status >= 400 {
		out, _ := json.Marshal(nativeError(w.kind, w.status, http.StatusText(w.status)))
		_, err := w.ResponseWriter.Write(append(out, '\n'))
		if err != nil {
			return 0, err
		}
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}
func (w *nativeErrorWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *nativeErrorWriter) Flush()                      { _ = http.NewResponseController(w.ResponseWriter).Flush() }

func (route *nativeRoute) catalog(data []map[string]string) any {
	models := []any{}
	for _, row := range data {
		if route.kind == "gemini" {
			models = append(models, map[string]any{"name": "models/" + row["id"], "displayName": row["id"], "supportedGenerationMethods": []string{"generateContent", "streamGenerateContent"}})
		} else {
			models = append(models, map[string]any{"name": row["id"], "model": row["id"]})
		}
	}
	return map[string]any{"models": models}
}

// Streaming stays synchronous: a slow reader backpressures the existing gateway,
// whose request context owns cancellation and timeouts. Only a bounded SSE frame
// and function arguments are retained; generated text is never accumulated.
type nativeResponseWriter struct {
	http.ResponseWriter
	route                        *nativeRoute
	status                       int
	buffer, event                []byte
	calls                        map[int]*chatNativeCall
	arguments                    map[int][]byte
	callBytes                    int
	finish                       string
	last                         chatNativeResponse
	wrote, emitted, done, failed bool
	err                          error
}

func newNativeResponseWriter(w http.ResponseWriter, route *nativeRoute) *nativeResponseWriter {
	return &nativeResponseWriter{ResponseWriter: w, route: route, status: 200, calls: map[int]*chatNativeCall{}, arguments: map[int][]byte{}}
}
func (w *nativeResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *nativeResponseWriter) WriteHeader(code int)        { w.status = code }
func (w *nativeResponseWriter) Flush() {
	if w.wrote {
		if err := http.NewResponseController(w.ResponseWriter).Flush(); err != nil {
			w.err = err
		}
	}
}
func (w *nativeResponseWriter) Write(b []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if len(w.buffer)+len(w.event)+len(b) > maxNativeResponse {
		w.err = errors.New("native response exceeds limit")
		return 0, w.err
	}
	w.buffer = append(w.buffer, b...)
	if !w.route.stream || w.status >= 400 {
		return len(b), nil
	}
	for {
		i := bytes.IndexByte(w.buffer, '\n')
		if i < 0 {
			break
		}
		line := bytes.TrimSuffix(w.buffer[:i], []byte{'\r'})
		if len(line) == 0 {
			if len(w.event) > 0 {
				if err := w.frame(bytes.TrimSuffix(w.event, []byte{'\n'})); err != nil {
					w.err = err
					return 0, err
				}
			}
			w.event = nil
		} else if bytes.HasPrefix(line, []byte("data:")) {
			w.event = append(w.event, bytes.TrimPrefix(line[5:], []byte{' '})...)
			w.event = append(w.event, '\n')
		}
		w.buffer = w.buffer[i+1:]
	}
	return len(b), nil
}
func (w *nativeResponseWriter) start() {
	if w.wrote {
		return
	}
	w.wrote = true
	w.Header().Del("Content-Length")
	w.Header().Del("Content-Encoding")
	kind := "application/json"
	if w.route.stream {
		if w.route.kind == "ollama" {
			kind = "application/x-ndjson"
		} else if w.route.sse {
			kind = "text/event-stream"
		}
	}
	w.Header().Set("Content-Type", kind)
	w.Header().Set("Cache-Control", "no-store")
	w.ResponseWriter.WriteHeader(w.status)
}
func (w *nativeResponseWriter) emit(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	w.start()
	if w.route.stream {
		if w.route.kind == "gemini" && w.route.sse {
			b = append(append([]byte("data: "), b...), []byte("\n\n")...)
		} else if w.route.kind == "gemini" {
			prefix := byte('[')
			if w.emitted {
				prefix = ','
			}
			b = append([]byte{prefix}, b...)
		} else {
			b = append(b, '\n')
		}
	}
	_, err = w.ResponseWriter.Write(b)
	w.emitted = true
	if err == nil {
		w.Flush()
		err = w.err
	}
	return err
}
func (w *nativeResponseWriter) output(m chatNativeMessage, finished bool) (any, error) {
	text := ""
	if m.Content != nil {
		text = *m.Content
	}
	if m.Refusal != nil && *m.Refusal != "" {
		text += *m.Refusal
	}
	parts := []any{}
	if text != "" {
		parts = append(parts, map[string]any{"text": text})
	}
	if m.Reasoning != "" {
		parts = append(parts, map[string]any{"text": m.Reasoning, "thought": true})
	}
	tools := []any{}
	for _, call := range m.Calls {
		var args map[string]any
		if call.Function.Name == "" || json.Unmarshal([]byte(call.Function.Arguments), &args) != nil || args == nil {
			return nil, errors.New("invalid native tool response")
		}
		f := map[string]any{"name": call.Function.Name, "args": args}
		if call.ID != "" {
			f["id"] = call.ID
		}
		parts = append(parts, map[string]any{"functionCall": f})
		tools = append(tools, map[string]any{"function": map[string]any{"name": call.Function.Name, "arguments": args}})
	}
	if w.route.kind == "gemini" {
		candidate := map[string]any{"index": 0, "content": map[string]any{"role": "model", "parts": parts}}
		if finished {
			reason := map[string]string{"stop": "STOP", "tool_calls": "STOP", "length": "MAX_TOKENS", "content_filter": "SAFETY"}[w.finish]
			if reason == "" {
				return nil, errors.New("unknown completion reason")
			}
			candidate["finishReason"] = reason
		}
		out := map[string]any{"candidates": []any{candidate}, "modelVersion": w.route.model}
		if w.last.ID != "" {
			out["responseId"] = w.last.ID
		}
		if finished && w.last.Usage != nil {
			u := w.last.Usage
			usage := map[string]any{}
			if u.Input != nil {
				usage["promptTokenCount"] = *u.Input
			}
			if u.Output != nil {
				usage["candidatesTokenCount"] = *u.Output
			}
			if u.Total != nil {
				usage["totalTokenCount"] = *u.Total
			}
			out["usageMetadata"] = usage
		}
		return out, nil
	}
	out := map[string]any{"model": w.route.model, "created_at": time.Now().UTC().Format(time.RFC3339Nano), "done": finished}
	if w.route.generate {
		if len(tools) > 0 {
			return nil, errors.New("generate returned tool calls")
		}
		out["response"] = text
		if m.Reasoning != "" {
			out["thinking"] = m.Reasoning
		}
	} else {
		message := map[string]any{"role": "assistant", "content": text}
		if len(tools) > 0 {
			message["tool_calls"] = tools
		}
		if m.Reasoning != "" {
			message["thinking"] = m.Reasoning
		}
		out["message"] = message
	}
	if finished {
		reason := map[string]string{"stop": "stop", "tool_calls": "stop", "length": "length"}[w.finish]
		if reason == "" {
			return nil, errors.New("unsupported Ollama completion reason")
		}
		out["done_reason"] = reason
		if u := w.last.Usage; u != nil {
			if u.Input != nil {
				out["prompt_eval_count"] = *u.Input
			}
			if u.Output != nil {
				out["eval_count"] = *u.Output
			}
		}
	}
	return out, nil
}
func (w *nativeResponseWriter) frame(b []byte) error {
	if w.done || w.failed {
		return errors.New("data after native completion")
	}
	if string(b) == "[DONE]" {
		if w.finish == "" {
			return errors.New("missing completion reason")
		}
		m := chatNativeMessage{}
		for i := 0; i < len(w.calls); i++ {
			call := w.calls[i]
			if call == nil {
				return errors.New("invalid tool indexes")
			}
			call.Function.Arguments = string(w.arguments[i])
			m.Calls = append(m.Calls, *call)
		}
		out, err := w.output(m, true)
		if err != nil {
			return err
		}
		if err = w.emit(out); err != nil {
			return err
		}
		w.done = true
		return nil
	}
	var frame chatNativeResponse
	if json.Unmarshal(b, &frame) != nil || len(frame.Error) > 0 {
		return errors.New("upstream stream error")
	}
	if frame.ID != "" {
		w.last.ID = frame.ID
	}
	if frame.Usage != nil {
		w.last.Usage = frame.Usage
	}
	if len(frame.Choices) == 0 {
		return nil
	}
	if len(frame.Choices) != 1 || frame.Choices[0].Index != 0 {
		return errors.New("multiple native choices unsupported")
	}
	choice := frame.Choices[0]
	if choice.Finish != nil && *choice.Finish != "" {
		w.finish = *choice.Finish
	}
	for _, call := range choice.Delta.Calls {
		if call.Index == nil || *call.Index < 0 || *call.Index > 1023 {
			return errors.New("missing tool index")
		}
		index := *call.Index
		if w.calls[index] == nil {
			w.calls[index] = &chatNativeCall{}
		}
		current := w.calls[index]
		current.ID += call.ID
		current.Function.Name += call.Function.Name
		w.arguments[index] = append(w.arguments[index], call.Function.Arguments...)
		if len(current.ID) > 4096 || len(current.Function.Name) > 4096 {
			return errors.New("tool identity exceeds limit")
		}
		w.callBytes += len(call.ID) + len(call.Function.Name) + len(call.Function.Arguments)
		if w.callBytes > maxNativeResponse {
			return errors.New("tool response exceeds limit")
		}
	}
	choice.Delta.Calls = nil
	if choice.Delta.Content != nil && *choice.Delta.Content != "" || choice.Delta.Reasoning != "" || choice.Delta.Refusal != nil {
		out, err := w.output(choice.Delta, false)
		if err != nil {
			return err
		}
		return w.emit(out)
	}
	return nil
}
func (w *nativeResponseWriter) Finish() {
	if w.status >= 400 {
		w.route.stream = false
		_ = w.emit(nativeError(w.route.kind, w.status, http.StatusText(w.status)))
		return
	}
	if !w.route.stream && w.err == nil {
		var response chatNativeResponse
		if json.Unmarshal(w.buffer, &response) != nil || len(response.Error) > 0 || len(response.Choices) != 1 || response.Choices[0].Finish == nil {
			w.err = errors.New("invalid completion response")
		} else {
			w.last = response
			w.finish = *response.Choices[0].Finish
			out, err := w.output(response.Choices[0].Message, true)
			if err != nil {
				w.err = err
			} else {
				w.err = w.emit(out)
				w.done = w.err == nil
			}
		}
	}
	if !w.done || w.err != nil {
		if !w.wrote {
			w.status = 502
			w.route.stream = false
		}
		// Never expose provider bodies or mark interrupted streams as done.
		w.err = nil
		_ = w.emit(nativeError(w.route.kind, 502, "upstream_stream_error: incomplete or unsupported response"))
		w.failed = true
	}
	if w.route.stream && w.route.kind == "gemini" && !w.route.sse && w.emitted {
		_, _ = fmt.Fprint(w.ResponseWriter, "]")
		w.Flush()
	}
}
