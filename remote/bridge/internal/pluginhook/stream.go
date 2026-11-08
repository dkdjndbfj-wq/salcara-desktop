package pluginhook

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
)

// ObserveStream forwards bytes/flushes immediately and samples complete SSE JSON
// frames for plugins. Oversized frames are skipped without retaining their body.
// It is deliberately observation-only: changing deltas can corrupt resumable or
// WebSocket conversation state and must never delay the live transport.
func (h *Hooks) ObserveStream(w http.ResponseWriter, ctx context.Context, tool string) http.ResponseWriter {
	if !h.Enabled() {
		return w
	}
	return &streamWriter{ResponseWriter: w, hooks: h, ctx: ctx, tool: tool}
}

type streamWriter struct {
	http.ResponseWriter
	hooks  *Hooks
	ctx    context.Context
	tool   string
	buffer []byte
	skip   bool
	tail   byte
}

func (w *streamWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *streamWriter) Flush()                      { _ = http.NewResponseController(w.ResponseWriter).Flush() }
func (w *streamWriter) Write(raw []byte) (int, error) {
	n, err := w.ResponseWriter.Write(raw)
	for _, b := range raw[:n] {
		if b == '\r' {
			continue
		}
		if w.skip {
			if w.tail == '\n' && b == '\n' {
				w.skip = false
				w.tail = 0
			} else {
				w.tail = b
			}
			continue
		}
		w.buffer = append(w.buffer, b)
		if len(w.buffer) > maxEventBytes-1024 {
			w.buffer = nil
			w.skip = true
			w.tail = b
			continue
		}
		if b == '\n' && len(w.buffer) >= 2 && w.buffer[len(w.buffer)-2] == '\n' {
			w.observe()
			w.buffer = w.buffer[:0]
		}
	}
	return n, err
}
func (w *streamWriter) observe() {
	var data []byte
	for _, line := range bytes.Split(w.buffer, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("data:")) {
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, bytes.TrimSpace(line[5:])...)
		}
	}
	if json.Valid(data) {
		w.hooks.FireEvent(w.ctx, "stream", map[string]any{"tool": w.tool, "event": json.RawMessage(data)})
	}
}
