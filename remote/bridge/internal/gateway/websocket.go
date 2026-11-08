package gateway

// The original built-in OpenAI provider cannot be renamed without hiding old
// sessions, and current Codex attempts WebSocket transport first. Keep that
// provider ID and terminate the socket locally; upstream remains ordinary SSE.
// Context exists ONLY inside this authenticated connection, never in Hub storage.
import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/usageledger"

	"github.com/gorilla/websocket"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type wsHistory struct {
	id            string
	request       map[string]any
	input         []any
	clientModel   string
	upstreamModel string
	credential    string
}

func wsInputs(value any) ([]any, bool) {
	if value == nil {
		return []any{}, true
	}
	if list, ok := value.([]any); ok {
		return list, true
	}
	if text, ok := value.(string); ok {
		return []any{map[string]any{"role": "user", "content": text}}, true
	}
	return nil, false
}

func wsError(conn *websocket.Conn, code, message string) {
	_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_ = conn.WriteJSON(map[string]any{"type": "error", "status": 400, "error": map[string]any{"type": "invalid_request_error", "code": code, "message": message}})
}

func (h *Handler) serveWebSocket(w http.ResponseWriter, r *http.Request, account config.LocalAccount, target, key string, currentAuthorization func() bool) {
	if !currentAuthorization() {
		fail(w, 409, "API 授权已变更，请重新连接")
		return
	}
	up := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(maxBody)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	frames := make(chan []byte, 1)
	go func() {
		defer close(frames)
		defer cancel()
		for {
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Hour))
			kind, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if kind != websocket.TextMessage {
				return
			}
			select {
			case frames <- raw:
			case <-ctx.Done():
				return
			default:
				// Never block the sole socket reader: it must detect disconnects
				// and cancel the upstream even while an inference is running.
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "pipelining is not supported"), time.Now().Add(time.Second))
				_ = conn.Close()
				return
			}
		}
	}()
	var last wsHistory
	identity := config.AccountFingerprint(account)
	for raw := range frames {
		currentKey := h.gatewayKey(target)
		if !currentAuthorization() || !h.accountStillCurrent(target, account) || config.AccountFingerprint(account) != identity || currentKey == "" || subtle.ConstantTimeCompare([]byte(currentKey), []byte(key)) != 1 {
			wsError(conn, "configuration_changed", "API 配置已更改，请在原工具重新连接；未使用其他 Key")
			return
		}
		in, valid := webSocketEnvelope(raw)
		if !valid || in["type"] != "response.create" {
			wsError(conn, "invalid_event", "仅支持 response.create JSON 消息")
			continue
		}
		if value, present := in["generate"]; present {
			if _, ok := value.(bool); !ok {
				wsError(conn, "invalid_generate", "generate 必须是布尔值")
				continue
			}
		}
		if value, present := in["stream"]; present && value != true {
			wsError(conn, "invalid_stream", "WebSocket 仅支持流式响应")
			continue
		}
		if value := in["background"]; value != nil && value != false {
			wsError(conn, "unsupported_background", "本机 WebSocket 不支持后台任务")
			continue
		}
		if in["conversation"] != nil {
			wsError(conn, "unsupported_conversation", "请通过本连接的 previous_response_id 或完整 input 续接会话")
			continue
		}
		if in["stream_id"] != nil && in["stream_id"] != "" {
			wsError(conn, "unsupported_stream_id", "本地网关仅支持单会话顺序响应，请分别建立连接")
			continue
		}
		input, ok := wsInputs(in["input"])
		if !ok {
			wsError(conn, "invalid_input", "input 必须是数组或文本")
			continue
		}
		if value, present := in["previous_response_id"]; present && value != nil {
			if _, ok := value.(string); !ok {
				wsError(conn, "invalid_previous_response_id", "previous_response_id 必须是文本或 null")
				continue
			}
		}
		previousCredential := ""
		if previous, _ := in["previous_response_id"].(string); previous != "" {
			if last.id != previous {
				wsError(conn, "previous_response_not_found", "连接内的原上下文已失效，请重传完整原会话，不能丢弃历史")
				continue
			}
			input = append(append([]any{}, last.input...), input...)
			previousCredential = last.credential
			// Codex's incremental socket requests may omit unchanged tools/model.
			for key, value := range last.request {
				if _, present := in[key]; !present && key != "input" && key != "instructions" && key != "model" {
					in[key] = value
				}
			}
			if _, present := in["model"]; !present {
				// Resolve the original client name once again under the current
				// policy; an upstream name may itself shadow another alias.
				in["model"] = last.clientModel
			}
		}
		clientModel, _ := in["model"].(string)
		model := clientModel
		if h.WebSocketModel != nil {
			var allowed bool
			model, allowed = h.WebSocketModel(clientModel)
			if !allowed {
				wsError(conn, "invalid_model", "模型不符合本客户端和账号的权限，未自动更换模型")
				continue
			}
		}
		known := model != "" && model == account.Model
		for _, m := range account.Models {
			known = known || (model != "" && model == m)
		}
		if account.CredentialRef != "" && account.SourceType == "official_account" && config.ValidCatalogModel(model) {
			known = true // Native picker metadata is not an account entitlement check.
		}
		warmupModelKnown := known && config.ModelAllowed(account, model)
		if in["generate"] == false && h.WebSocketModelKnown != nil {
			warmupModelKnown = h.WebSocketModelKnown(model)
		}
		if (h.BeforeDispatchAccount == nil || in["generate"] == false && h.WebSocketModelKnown == nil) && !warmupModelKnown {
			wsError(conn, "invalid_model", "模型未在此 Key 的卡片配置中，未自动更换模型")
			continue
		}
		warmup := in["generate"] == false
		delete(in, "type")
		delete(in, "stream_id")
		delete(in, "previous_response_id")
		delete(in, "generate")
		delete(in, "background")
		in["model"], in["input"], in["stream"] = model, input, true
		body, err := json.Marshal(in)
		if err != nil || len(body) > maxBody {
			wsError(conn, "context_too_large", "原上下文超过本地限制，未截断历史")
			continue
		}
		if h.NormalizeRequest != nil {
			var status int
			body, status = h.NormalizeRequest(body)
			if status != 0 {
				wsError(conn, "invalid_tool_model", "请求的模型命名空间或工具模型权限无效")
				continue
			}
		}
		if warmup {
			var random [16]byte
			if _, err := rand.Read(random[:]); err != nil {
				return
			}
			id := "resp_salcara_warmup_" + hex.EncodeToString(random[:])
			response := map[string]any{"id": id, "object": "response", "status": "completed", "model": model, "output": []any{}, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}}
			_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if conn.WriteJSON(map[string]any{"type": "response.completed", "response": response}) != nil {
				return
			}
			last = wsHistory{id: id, request: in, input: input, clientModel: clientModel, upstreamModel: model}
			continue // local preparation only: zero upstream inference or billing.
		}
		frameAccount := account
		frameCurrent := func() bool {
			currentKey := h.gatewayKey(target)
			// The loop header already verified the exact socket account for this
			// turn. Do not re-read Source between validation and the pinned inner
			// call; that would expose old context to a replacement credential.
			return currentAuthorization() && currentKey != "" && subtle.ConstantTimeCompare([]byte(currentKey), []byte(key)) == 1
		}
		frameRelease := func() {}
		usedAccountHook := false
		if h.BeforeDispatchAccount != nil {
			selected, selectedCurrent, release, status := h.BeforeDispatchAccount(ctx, account, body)
			if release != nil {
				frameRelease = release
			}
			if status != 0 {
				frameRelease()
				wsError(conn, "execution_not_allowed", "当前请求的执行限制不允许继续")
				continue
			}
			if selected.ID == "" || selectedCurrent == nil {
				frameRelease()
				wsError(conn, "configuration_changed", "API 授权已变更，请重新连接")
				continue
			}
			frameAccount, usedAccountHook = selected, true
			frameCurrent = func() bool {
				currentKey := h.gatewayKey(target)
				return currentAuthorization() && selectedCurrent() && currentKey != "" && subtle.ConstantTimeCompare([]byte(currentKey), []byte(key)) == 1
			}
		}
		if !frameCurrent() {
			frameRelease()
			wsError(conn, "configuration_changed", "API 授权已变更，请重新连接")
			continue
		}
		if previousCredential != "" && !replayableTextBody(body) {
			selectedCredential, valid := CredentialBudgetIdentity(frameAccount)
			if !valid || selectedCredential != previousCredential {
				frameRelease()
				wsError(conn, "previous_account_unavailable", "原会话包含账号专属资源，原账号暂不可用，请稍后重试；未转交其他账号")
				continue
			}
		}
		known = model != "" && model == frameAccount.Model
		for _, candidate := range frameAccount.Models {
			known = known || model != "" && model == candidate
		}
		if frameAccount.CredentialRef != "" && frameAccount.SourceType == "official_account" && config.ValidCatalogModel(model) {
			known = true
		}
		if !known || !config.ModelAllowed(frameAccount, model) {
			frameRelease()
			wsError(conn, "invalid_model", "模型不符合本次图片账号的权限，未自动更换模型")
			continue
		}
		request := r.Clone(cliproxyexecutor.WithDownstreamWebsocket(ctx))
		request.Method = http.MethodPost
		request.Header = r.Header.Clone()
		request.Header.Del("Connection")
		request.Header.Del("Upgrade")
		request.Header.Set("Content-Type", "application/json")
		request.Body = io.NopCloser(bytes.NewReader(body))
		request.ContentLength = int64(len(body))
		writer := &wsWriter{conn: conn, header: make(http.Header)}
		last = wsHistory{} // a failed turn must not reuse a stale partial chain.
		// A switch between validation and forwarding must not put old context on
		// a newly selected Key. Pin the already verified socket account for this call.
		pinned := *h
		completedCredential := ""
		observeAttempt := pinned.AttemptObserver
		pinned.AttemptObserver = func(selected config.LocalAccount, o usageledger.Observation) {
			completedCredential, _ = CredentialBudgetIdentity(selected)
			if observeAttempt != nil {
				observeAttempt(selected, o)
			}
		}
		pinned.Source = func(requestTarget string) (config.LocalAccount, bool) {
			return frameAccount, requestTarget == target && frameCurrent()
		}
		// The outer WebSocket selected and pinned the account. Keep a health-only
		// pool reference when the caller supplied one; Accounts remains nil, so
		// the inner translation cannot select a second account for this frame.
		pinned.Accounts = nil
		pinned.PoolGroup = nil
		pinned.PoolEnabled = nil
		pinned.NormalizeRequest = nil
		pinned.BeforeDispatchAccount = nil
		if usedAccountHook {
			// The account hook already owns all per-turn resources. Do not run the
			// legacy image-only hook again in the inner HTTP translation call.
			pinned.BeforeDispatch = nil
		}
		pinned.InitialAttemptRelease = frameRelease
		pinned.ServeHTTP(writer, request)
		// The translated socket request is always stream=true. ServeHTTP has
		// drained the upstream and flushed every SSE event before returning, so
		// release the selected member before the final local [DONE] flush and
		// the next turn. A provider response.completed frame can already have
		// reached the client; that frame is not a local resource-release fence.
		frameRelease()
		finishErr := writer.finish()
		if finishErr != nil {
			return
		}
		if writer.completed != nil {
			id, _ := writer.completed["id"].(string)
			output, _ := writer.completed["output"].([]any)
			history := append(append([]any{}, input...), output...)
			encoded, _ := json.Marshal(history)
			if id != "" && len(encoded) <= maxBody {
				last = wsHistory{id: id, request: in, input: history, clientModel: clientModel, upstreamModel: model, credential: completedCredential}
			} else {
				last = wsHistory{}
			}
		}
	}
}

// Converts the existing validated HTTP SSE writer into one JSON event/frame.
type wsWriter struct {
	conn      *websocket.Conn
	header    http.Header
	status    int
	buffer    []byte
	err       error
	completed map[string]any
	failed    bool
}

func (w *wsWriter) Header() http.Header { return w.header }
func (w *wsWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *wsWriter) Write(raw []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if len(w.buffer)+len(raw) > 16<<20 {
		w.err = errors.New("websocket frame limit")
		return 0, w.err
	}
	w.buffer = append(w.buffer, raw...)
	return len(raw), nil
}
func (w *wsWriter) Flush() {
	if w.header.Get("Content-Type") != "text/event-stream" {
		return
	}
	for w.err == nil {
		end := bytes.Index(w.buffer, []byte("\n\n"))
		if end < 0 {
			break
		}
		frame := w.buffer[:end]
		w.buffer = w.buffer[end+2:]
		var data [][]byte
		for _, line := range bytes.Split(frame, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) {
				data = append(data, bytes.TrimSpace(line[5:]))
			}
		}
		payload := bytes.Join(data, []byte("\n"))
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var event map[string]any
		if json.Unmarshal(payload, &event) != nil {
			w.err = errors.New("invalid websocket event")
			break
		}
		_ = w.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		w.err = w.conn.WriteMessage(websocket.TextMessage, payload)
		response, _ := event["response"].(map[string]any)
		if event["error"] != nil || event["type"] == "error" || event["type"] == "response.failed" || event["type"] == "response.incomplete" || response["status"] == "failed" || response["status"] == "incomplete" {
			w.failed, w.completed = true, nil
		}
		if event["type"] == "response.completed" && !w.failed {
			w.completed, _ = event["response"].(map[string]any)
		}
	}
}
func (w *wsWriter) finish() error {
	w.Flush()
	if w.err != nil {
		return w.err
	}
	if w.header.Get("Content-Type") != "text/event-stream" {
		var result map[string]any
		_ = json.Unmarshal(w.buffer, &result)
		if result["error"] == nil {
			result = map[string]any{"error": map[string]string{"message": "本地网关响应异常"}}
		}
		result["type"], result["status"] = "error", w.status
		_ = w.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		return w.conn.WriteJSON(result)
	}
	return nil
}
