// Package pluginhook connects the authenticated desktop plugin runtime to local
// requests. An unavailable, expired or malformed hook always preserves the
// original request/response. Plugin observations are bounded and best-effort.
package pluginhook

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	LeaseDuration   = 90 * time.Second
	MaxPayloadBytes = 4 << 20
	maxEventBytes   = 64 << 10
	maxQueuedEvents = 64
)

type endpoint struct {
	port       int
	token      string
	expires    time.Time
	generation uint64
}
type queuedEvent struct {
	endpoint endpoint
	body     []byte
}

// Hooks is shared by the console, named API and local tool gateways. Its zero
// value is disabled. It starts no background goroutine until an event is queued.
type Hooks struct {
	mu       sync.Mutex
	current  endpoint
	client   *http.Client
	queue    []queuedEvent
	draining bool
}

func FromEnv() *Hooks {
	h := &Hooks{}
	port, _ := strconv.Atoi(os.Getenv("SALCARA_PLUGIN_HOOK_PORT"))
	_ = h.Configure(port, os.Getenv("SALCARA_PLUGIN_HOOK_TOKEN"))
	return h
}

// Configure renews an existing desktop lease or atomically replaces a stale
// desktop connection. Tokens are never returned to a renderer or written to disk.
func (h *Hooks) Configure(port int, token string) error {
	if port < 1 || port > 65535 || len(token) < 24 || len(token) > 256 || !validToken(token) {
		return errors.New("invalid plugin endpoint")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.current.port != port || h.current.token != token {
		h.current.generation++
		h.queue = nil
	}
	h.current.port, h.current.token, h.current.expires = port, token, time.Now().Add(LeaseDuration)
	if h.client == nil {
		h.client = &http.Client{Timeout: 3 * time.Second,
			Transport:     &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 300 * time.Millisecond}).DialContext, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return nil
}

func validToken(token string) bool {
	for _, c := range token {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}
func validAnnotation(name, value string) bool {
	for _, c := range name {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c) {
			continue
		}
		return false
	}
	for _, c := range value {
		if c < 32 || c == 127 {
			return false
		}
	}
	return true
}

// Disconnect cannot detach a newer desktop process with a different token.
func (h *Hooks) Disconnect(token string) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(h.current.token)) != 1 {
		return false
	}
	h.current = endpoint{generation: h.current.generation + 1}
	h.queue = nil
	return true
}
func (h *Hooks) snapshot() (endpoint, *http.Client, bool) {
	if h == nil {
		return endpoint{}, nil, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.current, h.client, h.current.port > 0 && time.Now().Before(h.current.expires)
}
func (h *Hooks) Enabled() bool { _, _, ok := h.snapshot(); return ok }

// Only prompt content is mutable. In particular a plugin cannot modify a model,
// tool/resource budget, previous-response identity or transport mode after the
// caller's account/model/resource policy has already been checked.
var promptFields = [...]string{"instructions", "input", "messages", "system"}

func mergePrompt(original, candidate json.RawMessage) json.RawMessage {
	var before, after map[string]json.RawMessage
	if json.Unmarshal(original, &before) != nil || before == nil || json.Unmarshal(candidate, &after) != nil || after == nil {
		return original
	}
	for _, key := range promptFields {
		if v, ok := after[key]; ok {
			if !validPromptField(key, v) {
				return original
			}
			before[key] = v
		} else {
			delete(before, key)
		}
	}
	merged, err := json.Marshal(before)
	if err != nil || len(merged) > MaxPayloadBytes {
		return original
	}
	return merged
}

func validPromptField(key string, raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	switch value.(type) {
	case nil:
		return true
	case string:
		return key != "messages"
	case []any:
		return key != "instructions"
	default:
		return false
	}
}

// SafeHeaders intentionally excludes API credentials, cookies, account metadata
// and transport headers. Plugins may add their own namespaced annotation headers.
func SafeHeaders(input map[string]string) map[string]string {
	output := map[string]string{}
	for name, value := range input {
		name = http.CanonicalHeaderKey(name)
		if strings.HasPrefix(strings.ToLower(name), "x-salcara-plugin-") && len(name) <= 80 && len(value) <= 256 && validAnnotation(name, value) && len(output) < 16 {
			output[name] = value
		}
	}
	return output
}

type RequestPayload struct {
	Tool    string            `json:"tool"`
	Body    json.RawMessage   `json:"body"`
	Headers map[string]string `json:"headers"`
}
type ResponsePayload struct {
	Tool           string          `json:"tool"`
	RequestBody    json.RawMessage `json:"requestBody"`
	ResponseStatus int             `json:"responseStatus"`
	ResponseBody   json.RawMessage `json:"responseBody"`
}
type HookResult struct {
	Body    json.RawMessage   `json:"body"`
	Headers map[string]string `json:"headers,omitempty"`
	Status  int               `json:"status,omitempty"`
}

func (h *Hooks) RunRequestHooks(ctx context.Context, tool string, body json.RawMessage, headers map[string]string) (json.RawMessage, map[string]string, error) {
	if !h.Enabled() || len(body) > MaxPayloadBytes || !json.Valid(body) {
		return body, headers, nil
	}
	result, err := h.call(ctx, "/plugin/hook/request", RequestPayload{Tool: tool, Body: body, Headers: SafeHeaders(headers)})
	if err != nil {
		return body, headers, nil
	}
	output := body
	if len(result.Body) > 0 && !bytes.Equal(bytes.TrimSpace(result.Body), []byte("null")) {
		output = mergePrompt(body, result.Body)
	}
	outputHeaders := make(map[string]string, len(headers)+len(result.Headers))
	for k, v := range headers {
		outputHeaders[k] = v
	}
	for k, v := range SafeHeaders(result.Headers) {
		outputHeaders[k] = v
	}
	return output, outputHeaders, nil
}
func (h *Hooks) RunResponseHooks(ctx context.Context, tool string, request json.RawMessage, status int, body json.RawMessage) (int, json.RawMessage, error) {
	if !h.Enabled() || len(request)+len(body) > MaxPayloadBytes || !json.Valid(body) {
		return status, body, nil
	}
	result, err := h.call(ctx, "/plugin/hook/response", ResponsePayload{Tool: tool, RequestBody: request, ResponseStatus: status, ResponseBody: body})
	if err != nil || !json.Valid(result.Body) || bytes.Equal(bytes.TrimSpace(result.Body), []byte("null")) {
		return status, body, nil
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(result.Body, &object) != nil || object == nil {
		return status, body, nil
	}
	if result.Status >= 200 && result.Status <= 599 {
		status = result.Status
	}
	return status, result.Body, nil
}

// FireEvent enqueues an observation without delaying token delivery or spawning
// an unbounded goroutine per event. Overflow drops observations, never user data.
func (h *Hooks) FireEvent(_ context.Context, event string, payload any) {
	endpoint, _, ok := h.snapshot()
	if !ok {
		return
	}
	body, err := json.Marshal(map[string]any{"event": event, "payload": payload})
	if err != nil || len(body) > maxEventBytes {
		return
	}
	h.mu.Lock()
	if endpoint.generation != h.current.generation || len(h.queue) >= maxQueuedEvents {
		h.mu.Unlock()
		return
	}
	h.queue = append(h.queue, queuedEvent{endpoint: endpoint, body: body})
	if h.draining {
		h.mu.Unlock()
		return
	}
	h.draining = true
	h.mu.Unlock()
	go h.drain()
}
func (h *Hooks) drain() {
	for {
		h.mu.Lock()
		if len(h.queue) == 0 {
			h.draining = false
			h.mu.Unlock()
			return
		}
		item := h.queue[0]
		h.queue[0] = queuedEvent{}
		h.queue = h.queue[1:]
		current, client := h.current, h.client
		h.mu.Unlock()
		if item.endpoint.generation != current.generation || !time.Now().Before(current.expires) {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _ = post(ctx, client, current, "/plugin/event", item.body)
		cancel()
	}
}
func (h *Hooks) call(ctx context.Context, path string, payload any) (*HookResult, error) {
	endpoint, client, ok := h.snapshot()
	if !ok {
		return nil, errors.New("plugin lease unavailable")
	}
	body, err := json.Marshal(payload)
	if err != nil || len(body) > MaxPayloadBytes {
		return nil, errors.New("plugin payload exceeds limit")
	}
	result, err := post(ctx, client, endpoint, path, body)
	if err != nil {
		return nil, err
	}
	current, _, ok := h.snapshot()
	if !ok || current.generation != endpoint.generation {
		return nil, errors.New("plugin lease changed")
	}
	return result, nil
}
func post(ctx context.Context, client *http.Client, endpoint endpoint, path string, body []byte) (*HookResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d%s", endpoint.port, path), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Plugin-Token", endpoint.token)
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("plugin hook unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxPayloadBytes+1))
	if err != nil || len(raw) > MaxPayloadBytes {
		return nil, errors.New("plugin result exceeds limit")
	}
	var result HookResult
	if json.Unmarshal(raw, &result) != nil {
		return nil, errors.New("invalid plugin result")
	}
	return &result, nil
}
