// Package gateway adapts upstream protocols locally using the same MIT-licensed
// CLIProxyAPI translator family used by Cockpit. It does not create accounts,
// change histories, discover secrets, or send credentials to the remote Hub.
package gateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"salcara/bridge/internal/config"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	_ "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/builtin"
	log "github.com/sirupsen/logrus"
)

const maxBody = 64 << 20

type Handler struct {
	Key           func() string
	Source        func(string) (config.LocalAccount, bool)
	Client        *http.Client
	RefreshModels func(context.Context, string, config.LocalAccount) ([]string, error)
}

func New(key func() string, source func(string) (config.LocalAccount, bool)) *Handler {
	// Translators must not log request bodies, private prompts or upstream keys.
	log.SetOutput(io.Discard)
	return &Handler{Key: key, Source: source, Client: &http.Client{
		Transport:     &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 20 * time.Second, ResponseHeaderTimeout: 120 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func fail(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": message, "type": "salcara_gateway_error"}})
}

func wire(a config.LocalAccount) (translator.Format, string) {
	protocol := a.Protocol
	if protocol == "" {
		protocol = config.NativeProtocol(a.Kind)
	}
	switch protocol {
	case "chat":
		return translator.FormatOpenAI, "/chat/completions"
	case "anthropic":
		return translator.FormatClaude, "/messages"
	default:
		return translator.FormatCodex, "/responses"
	}
}

func clientFormat(kind string) translator.Format {
	if kind == "codex" {
		return translator.FormatOpenAIResponse
	}
	return translator.FormatClaude
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if recover() != nil {
			if w.Header().Get("Content-Type") == "text/event-stream" {
				streamError(w)
			} else {
				fail(w, 502, "接口转换失败；没有切换模型或创建新会话")
			}
		}
	}()
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() || r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		fail(w, 403, "仅允许本机 AI 工具访问")
		return
	}
	key := h.Key()
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if r.Header.Get("Authorization") == "" {
		provided = r.Header.Get("x-api-key")
	}
	if key == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(key)) != 1 {
		fail(w, 401, "本地协议网关凭据无效")
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/gateway/"), "/", 2)
	if len(parts) != 2 || (parts[0] != "codex" && parts[0] != "claude" && parts[0] != "claude-desktop" && parts[0] != "remote-codex" && parts[0] != "remote-claude") {
		fail(w, 404, "无效的工具路由")
		return
	}
	path := parts[1]
	modelLimit, modelAfter := 1000, ""
	if r.URL.RawQuery != "" {
		if parts[0] != "claude-desktop" || path != "v1/models" || r.Method != http.MethodGet {
			fail(w, 404, "无效的工具路由")
			return
		}
		var err error
		modelLimit, modelAfter, err = desktopModelsQuery(r.URL.RawQuery)
		if err != nil {
			fail(w, 400, "模型分页参数无效")
			return
		}
	}
	a, valid := h.Source(parts[0])
	if !valid {
		fail(w, 409, "原工具 API 配置已失效，请在工具卡片重新应用；不会使用其他 Key 代替")
		return
	}
	// Snapshot the applied connection before choosing a per-request protocol.
	// Slow discovery/body reads must not send with a removed or switched Key.
	accountID, fingerprint := a.ID, config.AccountFingerprint(a)
	stillApplied := func() bool {
		current, ok := h.Source(parts[0])
		currentKey := h.Key()
		return ok && current.ID == accountID && config.AccountFingerprint(current) == fingerprint &&
			currentKey != "" && subtle.ConstantTimeCompare([]byte(currentKey), []byte(key)) == 1
	}
	if parts[0] == "claude-desktop" && len(a.Models) == 0 && h.RefreshModels != nil && (path == "v1/models" || path == "v1/messages" || path == "v1/messages/count_tokens") {
		models, err := h.RefreshModels(r.Context(), parts[0], a)
		if err != nil {
			fail(w, 502, "模型目录读取失败，请在 Agent 中刷新模型")
			return
		}
		a.Models = models
	}
	// The socket re-validates the live source on every frame itself.
	if a.Kind == "codex" && path == "v1/responses" && r.Method == http.MethodGet && websocket.IsWebSocketUpgrade(r) {
		h.serveWebSocket(w, r, a)
		return
	}
	if !stillApplied() {
		fail(w, 409, "API 配置已在请求期间变更，请重试；没有使用旧 Key 发送请求")
		return
	}
	if path == "v1/models" && r.Method == http.MethodGet {
		if parts[0] == "claude-desktop" {
			models, labels := config.ClaudeDesktopCatalog(a)
			start := 0
			if modelAfter != "" {
				found := false
				for i, model := range models {
					if model == modelAfter {
						start, found = i+1, true
						break
					}
				}
				if !found {
					fail(w, 400, "模型分页游标不在当前 Key 的目录中")
					return
				}
			}
			end := min(start+modelLimit, len(models))
			page := models[start:end]
			data := []map[string]string{}
			for _, model := range page {
				data = append(data, map[string]string{"id": model, "type": "model", "display_name": labels[model], "created_at": "1970-01-01T00:00:00Z"})
			}
			first, last := "", ""
			if len(page) > 0 {
				first, last = page[0], page[len(page)-1]
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "has_more": end < len(models), "first_id": first, "last_id": last})
			return
		}
		models := append([]string{}, a.Models...)
		if a.Model != "" {
			models = append(models, a.Model)
		}
		seen, data := map[string]bool{}, []map[string]string{}
		for _, m := range models {
			if !seen[m] {
				seen[m] = true
				data = append(data, map[string]string{"id": m, "object": "model", "owned_by": "configured-upstream"})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
		return
	}
	if r.Method != http.MethodPost || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		fail(w, 405, "需要 JSON POST")
		return
	}
	if path == "v1/responses/compact" {
		fail(w, 501, "当前转换模式不支持上游远程压缩，请使用工具的本地压缩或支持 Responses 的服务商")
		return
	}
	expected := "v1/messages"
	if a.Kind == "codex" {
		expected = "v1/responses"
	}
	count := a.Kind == "claude" && path == "v1/messages/count_tokens"
	if path != expected && !count {
		fail(w, 404, "这个路由不支持请求的功能")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody || !json.Valid(body) {
		fail(w, 400, "请求不是有效 JSON 或内容超过本地网关限制")
		return
	}
	var input struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Previous string `json:"previous_response_id"`
	}
	if json.Unmarshal(body, &input) != nil || input.Model == "" {
		fail(w, 400, "请求缺少模型 ID")
		return
	}
	clientModel, clientBody := input.Model, body
	known := input.Model == a.Model
	if parts[0] == "claude-desktop" {
		actual, ok := config.ResolveClaudeDesktopModel(a, input.Model)
		if !ok {
			fail(w, 400, "模型不在当前 Key 的目录中，请刷新模型")
			return
		}
		known, input.Model = true, actual
		if actual != clientModel {
			body = replaceModelJSON(body, clientModel, actual)
		}
	}
	for _, m := range a.Models {
		known = known || m == input.Model
	}
	if !known {
		fail(w, 400, "模型不在此 Key 的配置目录中；请先在工具卡片选择或读取目录。不会退回其他模型")
		return
	}
	// The tool may switch models mid-thread (Codex model picker, phone model chip):
	// pick the upstream wire per request from the model family / provider type.
	if p := config.InferProtocol(input.Model, a.Wire); p != "" {
		a.Protocol = p
	}
	from, suffix := wire(a)
	if parts[0] == "claude-desktop" && config.NeedsAdapter(a) {
		body = stripDesktopOnlyOptions(body)
	}
	to := clientFormat(a.Kind)
	if count {
		if from != translator.FormatClaude {
			// Conservative local estimate, explicitly marked as such. It is never
			// a billable usage figure or a claim about the provider's tokenizer.
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Salcara-Token-Count", "local-estimate")
			_ = json.NewEncoder(w).Encode(map[string]int{"input_tokens": (len(body)+2)/3 + 256})
			return
		}
		suffix = "/messages/count_tokens"
	}
	if config.NeedsAdapter(a) && input.Previous != "" {
		fail(w, 400, "跨协议模式不支持只传 previous_response_id；请让工具携带完整原会话上下文")
		return
	}
	upstreamStream := input.Stream || (!count && config.NeedsAdapter(a) && (from == translator.FormatClaude || from == translator.FormatCodex))
	converted := body
	if !count && config.NeedsAdapter(a) {
		if !translator.HasRequestTransformer(to, from) {
			fail(w, 501, "没有此接口组合的转换器")
			return
		}
		converted = translator.TranslateRequest(to, from, input.Model, body, upstreamStream)
		if !json.Valid(converted) {
			fail(w, 502, "接口转换生成了无效请求")
			return
		}
	}
	_, root, err := config.APIBase(a.BaseURL)
	if err != nil {
		fail(w, 400, "上游地址无效")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Hour)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, root+suffix, bytes.NewReader(converted))
	if err != nil {
		fail(w, 400, "无法构造上游请求")
		return
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if upstreamStream {
		request.Header.Set("Accept", "text/event-stream")
	}
	if a.AuthMode == "api-key" {
		request.Header.Set("x-api-key", a.Key)
	} else {
		request.Header.Set("Authorization", "Bearer "+a.Key)
	}
	if from == translator.FormatClaude {
		request.Header.Set("anthropic-version", "2023-06-01")
	}
	if !stillApplied() {
		fail(w, 409, "API 配置已在请求期间变更，请重试；没有使用旧 Key 发送请求")
		return
	}
	response, err := h.Client.Do(request)
	if err != nil {
		fail(w, 502, "上游连接失败，请检查地址、网络和接口协议")
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		fail(w, response.StatusCode, fmt.Sprintf("上游返回 HTTP %d，请检查这个 Key 的模型权限和接口；未更换模型", response.StatusCode))
		return
	}
	if upstreamStream && !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		fail(w, 502, "上游没有返回所需的流式接口")
		return
	}
	if input.Stream {
		if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
			fail(w, 502, "上游没有返回所需的流式接口")
			return
		}
		h.stream(w, ctx, response.Body, from, to, clientModel, input.Model, clientBody, converted, a, config.NeedsAdapter(a))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil || len(raw) > maxBody || bytes.Contains(raw, []byte(a.Key)) {
		fail(w, 502, "上游响应无效或读取失败")
		return
	}
	if upstreamStream && !hasTerminalSSE(raw) {
		fail(w, 502, "上游流未完成，不能把截断的响应当作成功结果")
		return
	}
	if upstreamStream && from == translator.FormatCodex && to == translator.FormatClaude {
		for _, line := range bytes.Split(raw, []byte("\n")) {
			data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if bytes.HasPrefix(line, []byte("data:")) && terminalData(data) {
				raw = append([]byte{}, data...)
				break
			}
		}
	}
	if !count && config.NeedsAdapter(a) {
		var state any
		raw = translator.TranslateNonStream(ctx, from, to, clientModel, clientBody, converted, raw, &state)
	}
	if clientModel != input.Model {
		raw = replaceModelJSON(raw, input.Model, clientModel)
	}
	if !json.Valid(raw) {
		fail(w, 502, "上游响应无法转换为所需格式")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// Only native model discovery needs query parameters. Never pass arbitrary
// query fields upstream or let a cursor create access to an unlisted model.
func desktopModelsQuery(raw string) (int, string, error) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return 0, "", err
	}
	limit, after := 1000, ""
	for name, entries := range values {
		if len(entries) != 1 {
			return 0, "", fmt.Errorf("duplicate model pagination field")
		}
		switch name {
		case "limit":
			value := entries[0]
			if len(value) == 0 || len(value) > 4 {
				return 0, "", fmt.Errorf("invalid model limit")
			}
			for _, digit := range value {
				if digit < '0' || digit > '9' {
					return 0, "", fmt.Errorf("invalid model limit")
				}
			}
			limit, err = strconv.Atoi(value)
			if err != nil || limit < 1 || limit > 1000 {
				return 0, "", fmt.Errorf("invalid model limit")
			}
		case "after_id":
			after = entries[0]
			if !config.ValidCatalogModel(after) {
				return 0, "", fmt.Errorf("invalid model cursor")
			}
		default:
			return 0, "", fmt.Errorf("unknown model pagination field")
		}
	}
	return limit, after, nil
}

func terminalData(data []byte) bool {
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return true
	}
	var event struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(data, &event)
	return event.Type == "response.completed" || event.Type == "response.incomplete" || event.Type == "message_stop"
}

func hasTerminalSSE(raw []byte) bool {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("data:")) && terminalData(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))) {
			return true
		}
	}
	return false
}

func streamError(w http.ResponseWriter) {
	message := `{"type":"error","error":{"type":"upstream_stream_error","message":"上游流中断或返回错误；本轮未完成，没有自动重试或切换模型"}}`
	_, _ = io.WriteString(w, "event: error\ndata: "+message+"\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func (h *Handler) stream(w http.ResponseWriter, ctx context.Context, reader io.Reader, from, to translator.Format, model, upstreamModel string, original, request []byte, a config.LocalAccount, adapt bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	var state any
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	terminal := false
	write := func(chunk []byte) bool {
		if len(chunk) == 0 {
			return true
		}
		if bytes.Contains(chunk, []byte(a.Key)) {
			return false
		}
		if model != upstreamModel {
			chunk = replaceSSEModel(chunk, upstreamModel, model)
		}
		if hasTerminalSSE(chunk) {
			terminal = true
		}
		if _, err := w.Write(chunk); err != nil {
			return false
		}
		if !bytes.HasSuffix(chunk, []byte("\n\n")) {
			_, _ = io.WriteString(w, "\n\n")
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return true
	}
	var eventName string
	var dataLines [][]byte
	frameSize, failed := 0, false
	dispatch := func() bool {
		if len(dataLines) == 0 {
			eventName = ""
			frameSize = 0
			return true
		}
		data := bytes.Join(dataLines, []byte("\n"))
		dataLines = nil
		if bytes.Contains(data, []byte(a.Key)) {
			failed = true
			return false
		}
		var event map[string]any
		if json.Unmarshal(data, &event) == nil && (event["error"] != nil || event["type"] == "error") {
			failed = true
			return false
		}
		if !adapt {
			// Preserve named SSE events; Claude clients rely on event: message_*.
			frame := []byte{}
			if eventName != "" {
				frame = append(frame, []byte("event: "+eventName+"\n")...)
			}
			for _, line := range bytes.Split(data, []byte("\n")) {
				frame = append(frame, []byte("data: ")...)
				frame = append(frame, line...)
				frame = append(frame, '\n')
			}
			frame = append(frame, '\n')
			eventName, frameSize = "", 0
			return write(frame)
		}
		// Translators accept one data: payload at a time, not the event label.
		line := append([]byte("data: "), data...)
		for _, chunk := range translator.TranslateStream(ctx, from, to, model, original, request, line, &state) {
			if !write(chunk) {
				failed = true
				return false
			}
		}
		eventName, frameSize = "", 0
		return true
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			if !dispatch() {
				break
			}
			continue
		}
		frameSize += len(line)
		if frameSize > 16<<20 {
			failed = true
			break
		}
		if bytes.HasPrefix(line, []byte("event:")) {
			eventName = strings.TrimSpace(string(line[6:]))
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			value := bytes.TrimPrefix(line, []byte("data:"))
			value = bytes.TrimPrefix(value, []byte(" "))
			dataLines = append(dataLines, append([]byte{}, value...))
		}
	}
	if !failed && scanner.Err() == nil {
		_ = dispatch()
	}
	if (failed || !terminal || scanner.Err() != nil) && ctx.Err() == nil {
		// Never manufacture a completed event for a truncated upstream response.
		streamError(w)
	}
}
