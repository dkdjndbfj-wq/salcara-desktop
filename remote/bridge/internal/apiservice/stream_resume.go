// stream_resume.go — SSE stream interruption detection and automatic continuation.
//
// When an upstream SSE stream breaks mid-way (network error, upstream 5xx during
// streaming, or abrupt TCP close), StreamResumeProxy:
//  1. Detects that the stream ended without a proper [DONE] marker.
//  2. Extracts the last `id` field from the streamed events (OpenAI uses this as
//     the response ID; Anthropic uses message ID in the data payload).
//  3. Builds a continuation request using `previous_response_id` (OpenAI Responses
//     API) or `conversation_id` (Anthropic), selects a fresh account from the pool,
//     and resumes transparently from where the first stream broke.
//
// The caller (service.go) wraps the gateway's streaming response writer with
// StreamResumeProxy before calling h.ServeHTTP. If the stream completes normally
// the proxy is a zero-overhead pass-through.
//
// Drop this file into internal/apiservice/.
package apiservice

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"

	"salcara/bridge/internal/config"
)

const (
	// maxResumeAttempts limits how many times we try to resume a broken stream.
	maxResumeAttempts = 2
	// maxSSELineBytes bounds metadata parsing for a malformed or unexpectedly
	// large upstream event. Forwarding is unaffected; only that line is skipped.
	maxSSELineBytes = 8 * 1024
)

// StreamState tracks state accumulated during SSE forwarding.
type StreamState struct {
	mu sync.Mutex

	// lastEventID is the last `id:` field seen in an SSE event.
	// OpenAI Responses API sets this to the response ID.
	lastEventID string

	// lastResponseID is the last response_id seen inside a data payload.
	// This covers both OpenAI and Anthropic shapes.
	lastResponseID string

	// done is set when we see a `data: [DONE]` or `event: done` marker.
	done bool

	// bytesWritten counts bytes forwarded to the client.
	bytesWritten int64

	// tokenCount is the running SSE token counter.
	TokenCounter apierrorStreamCounter
}

// apierrorStreamCounter is a local alias to avoid the import cycle between
// apiservice and apierror. The actual type is apierror.StreamCounter.
// We embed the needed methods via interface here so the file stays in-package.
type apierrorStreamCounter interface {
	Update(line []byte)
	Total() int
}

// SSEResumeWriter wraps a ResponseWriter and intercepts SSE events to:
//   - Update StreamState with event IDs and done markers.
//   - Forward all bytes to the underlying writer as they arrive.
//   - Expose whether the stream completed cleanly.
type SSEResumeWriter struct {
	http.ResponseWriter
	state          *StreamState
	buf            bytes.Buffer
	parseMu        sync.Mutex
	discardingLine bool
	once           sync.Once
	flusher        http.Flusher
}

// NewSSEResumeWriter creates a writer that intercepts SSE events.
// state must be non-nil. Pass nil for flusher to disable intermediate flushing.
func NewSSEResumeWriter(w http.ResponseWriter, state *StreamState) *SSEResumeWriter {
	f, _ := w.(http.Flusher)
	return &SSEResumeWriter{ResponseWriter: w, state: state, flusher: f}
}

func (rw *SSEResumeWriter) WriteHeader(code int) {
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *SSEResumeWriter) Write(p []byte) (int, error) {
	// Parse SSE events from the chunk.
	rw.parseSSEChunk(p)
	n, err := rw.ResponseWriter.Write(p)
	rw.state.mu.Lock()
	rw.state.bytesWritten += int64(n)
	rw.state.mu.Unlock()
	return n, err
}

func (rw *SSEResumeWriter) Flush() {
	if rw.flusher != nil {
		rw.flusher.Flush()
	}
}

// Finish parses the final unterminated SSE line, if any.  A network response
// is allowed to end immediately after a marker, without the usual trailing
// newline; the normal Write path deliberately keeps that fragment so a line
// split over two writes is not parsed prematurely.
func (rw *SSEResumeWriter) Finish() {
	rw.parseMu.Lock()
	defer rw.parseMu.Unlock()
	if rw.discardingLine {
		rw.buf.Reset()
		return
	}
	if rw.buf.Len() == 0 {
		return
	}
	line := append([]byte(nil), rw.buf.Bytes()...)
	rw.buf.Reset()
	rw.parseLine(bytes.TrimSuffix(line, []byte("\r")))
}

// parseSSEChunk scans raw SSE bytes for complete `id:`, `data:`, and `[DONE]`
// markers.  The upstream can split a single SSE line across multiple writes,
// so the incomplete suffix is retained until the next Write (or Finish).
func (rw *SSEResumeWriter) parseSSEChunk(p []byte) {
	rw.parseMu.Lock()
	defer rw.parseMu.Unlock()
	_, _ = rw.buf.Write(p)

	for {
		data := rw.buf.Bytes()
		i := bytes.IndexAny(data, "\r\n")
		if rw.discardingLine {
			if i < 0 {
				rw.buf.Reset()
				return
			}
			delimSize := 1
			if data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n' {
				delimSize++
			}
			rw.buf.Next(i + delimSize)
			rw.discardingLine = false
			continue
		}
		if i < 0 {
			// Keep an incomplete line for the next Write.  A malformed upstream
			// must not be able to make this metadata buffer grow without bound.
			if rw.buf.Len() > maxSSELineBytes {
				rw.buf.Reset()
				rw.discardingLine = true
			}
			return
		}
		if i > maxSSELineBytes {
			delimSize := 1
			if data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n' {
				delimSize++
			}
			rw.buf.Next(i + delimSize)
			continue
		}
		line := append([]byte(nil), data[:i]...)
		delimSize := 1
		if data[i] == '\r' && i+1 < len(data) && data[i+1] == '\n' {
			delimSize++
		}
		rw.buf.Next(i + delimSize)
		rw.parseLine(bytes.TrimSuffix(line, []byte("\r")))
	}
}

func (rw *SSEResumeWriter) parseLine(line []byte) {
	rw.state.mu.Lock()
	defer rw.state.mu.Unlock()

	// Some providers use an explicit done event instead of data: [DONE].
	if bytes.Equal(bytes.TrimSpace(line), []byte("event: done")) {
		rw.state.done = true
		return
	}

	// SSE id field
	if bytes.HasPrefix(line, []byte("id:")) {
		rw.state.lastEventID = strings.TrimSpace(string(line[3:]))
		return
	}

	// SSE data field
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	data := bytes.TrimSpace(line[5:])

	// [DONE] marker
	if bytes.Equal(data, []byte("[DONE]")) {
		rw.state.done = true
		return
	}

	// Feed to token counter
	if rw.state.TokenCounter != nil {
		rw.state.TokenCounter.Update(data)
	}

	// Extract response ID from the JSON payload for continuation.
	extractResponseID(data, rw.state)
}

// extractResponseID looks for `id`, `response_id`, or `message.id` in a JSON
// SSE data payload and updates state.lastResponseID if found.
func extractResponseID(data []byte, state *StreamState) {
	if len(data) == 0 || data[0] != '{' {
		return
	}
	var partial struct {
		ID         string `json:"id"`
		ResponseID string `json:"response_id"`
		Message    struct {
			ID string `json:"id"`
		} `json:"message"`
		// Anthropic message_start shape
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &partial) != nil {
		return
	}
	if partial.ID != "" {
		state.lastResponseID = partial.ID
	}
	if partial.ResponseID != "" {
		state.lastResponseID = partial.ResponseID
	}
	if partial.Message.ID != "" {
		state.lastResponseID = partial.Message.ID
	}
}

// Completed reports whether the stream ended with a proper [DONE] marker.
func (s *StreamState) Completed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

// LastResponseID returns the last response/message ID seen in the stream.
func (s *StreamState) LastResponseID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastResponseID != "" {
		return s.lastResponseID
	}
	return s.lastEventID
}

// BytesWritten returns total bytes forwarded to the client.
func (s *StreamState) BytesWritten() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytesWritten
}

// BuildContinuationBody takes the original request body and the last response ID
// and produces a new request body that continues from that point.
//
// For OpenAI Responses API: sets `previous_response_id`.
// For Anthropic Messages API: sets `conversation_id` (experimental extension).
// For OpenAI Chat Completions: no native continuation; returns nil.
func BuildContinuationBody(originalBody []byte, lastResponseID string, isMessages bool) []byte {
	if lastResponseID == "" || len(originalBody) == 0 {
		return nil
	}
	var input map[string]json.RawMessage
	if json.Unmarshal(originalBody, &input) != nil {
		return nil
	}
	if isMessages {
		// Anthropic continuation — set conversation_id (if the provider supports it).
		convID, _ := json.Marshal(lastResponseID)
		input["conversation_id"] = convID
	} else {
		// OpenAI Responses API continuation.
		prevID, _ := json.Marshal(lastResponseID)
		input["previous_response_id"] = prevID
		// Remove `input` array on continuation requests — the previous response
		// already includes the history. Only include the new user turn if present.
		// For now we pass through the original input; advanced continuations can
		// strip it if the provider rejects it.
	}
	out, err := json.Marshal(input)
	if err != nil {
		return nil
	}
	return out
}

// NullWriter discards all writes — used when probing whether a stream can start.
type NullWriter struct{ code int }

func (n *NullWriter) Header() http.Header         { return http.Header{} }
func (n *NullWriter) WriteHeader(code int)        { n.code = code }
func (n *NullWriter) Write(p []byte) (int, error) { return len(p), nil }

// ResumeDispatch is the function signature for the stream-resume retry dispatch.
// It is the streaming equivalent of DispatchFunc. The caller must ensure that
// the request body in continuationBody is a valid continuation request.
type ResumeDispatch func(
	ctx context.Context,
	w http.ResponseWriter,
	account config.LocalAccount,
	body []byte,
	state *StreamState,
) (status int, err error)

// StreamResume wraps a streaming dispatch call with automatic continuation on
// mid-stream interruption. It is called by service.go for streaming requests.
//
// On clean completion (state.Completed() == true after dispatch) it returns
// immediately. On an incomplete stream it attempts up to maxResumeAttempts
// continuations using accounts from the provided pool, in health-score order.
//
// This function writes directly to w — the caller must NOT write to w after
// calling this if StreamResume returns without error.
func StreamResume(
	ctx context.Context,
	w http.ResponseWriter,
	accounts []config.LocalAccount,
	originalBody []byte,
	isMessages bool,
	dispatch ResumeDispatch,
) {
	if len(accounts) == 0 {
		return
	}

	state := &StreamState{}
	sw := NewSSEResumeWriter(w, state)

	// First attempt uses the primary account.
	status, err := dispatch(ctx, sw, accounts[0], originalBody, state)
	sw.Finish()
	if err == nil && (status == 0 || (status >= 200 && status < 300)) {
		if state.Completed() {
			return // clean finish
		}
	}

	// Stream broke — attempt to resume using remaining accounts.
	lastID := state.LastResponseID()
	if lastID == "" || state.BytesWritten() == 0 {
		// Nothing was written to the client yet, or no ID to resume from.
		// Let the caller handle the error response.
		return
	}

	for attempt := 1; attempt <= maxResumeAttempts && attempt < len(accounts); attempt++ {
		if ctx.Err() != nil {
			return
		}
		contBody := BuildContinuationBody(originalBody, lastID, isMessages)
		if contBody == nil {
			return
		}

		resumeState := &StreamState{}
		rsw := NewSSEResumeWriter(w, resumeState)
		_, _ = dispatch(ctx, rsw, accounts[attempt], contBody, resumeState)
		rsw.Finish()

		if resumeState.Completed() {
			return
		}
		// Update the last ID for the next attempt.
		if newID := resumeState.LastResponseID(); newID != "" {
			lastID = newID
		}
	}
	// Stream still incomplete after all resume attempts — the caller's writer
	// already has partial content; nothing more we can do.
}

// IsStreamingResponse reports whether an HTTP response has an SSE Content-Type.
func IsStreamingResponse(h http.Header) bool {
	ct := h.Get("Content-Type")
	return strings.Contains(ct, "text/event-stream")
}

// drainSSEBody reads an SSE response body to completion, feeding each data line
// to a StreamCounter. It is used when we need to consume the body to keep the
// connection alive but are not forwarding to a client.
func drainSSEBody(body io.Reader, counter apierrorStreamCounter) {
	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		line := scanner.Bytes()
		if bytes.HasPrefix(line, []byte("data:")) {
			data := bytes.TrimSpace(line[5:])
			if counter != nil {
				counter.Update(data)
			}
		}
	}
}
