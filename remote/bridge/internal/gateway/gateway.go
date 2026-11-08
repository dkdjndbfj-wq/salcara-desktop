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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"salcara/bridge/internal/apierror"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/pluginhook"
	"salcara/bridge/internal/usageledger"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	_ "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/builtin"
	log "github.com/sirupsen/logrus"
)

const maxBody = 64 << 20

type Handler struct {
	// Plugins is optional and only applies to authenticated local tool routes.
	Plugins *pluginhook.Hooks
	// ChatCompletions is explicitly enabled only by the named-client API. Local
	// Codex/Claude tool routes retain their existing Responses/Messages contract.
	ChatCompletions bool
	// WebSocketModel optionally resolves a client model into an upstream model
	// and applies the caller's policy on every socket turn, including warmup.
	WebSocketModel func(string) (string, bool)
	// WebSocketModelKnown optionally checks a resolved model against an explicit
	// per-client member union for local warmup. It is only consulted when no
	// provider request will be made; real turns recheck the selected member.
	WebSocketModelKnown func(string) bool
	// BeforeDispatch optionally reserves caller-specific execution resources.
	// It runs only for real inference, once per HTTP or translated WS turn, and
	// returns a release held through response drain. A nonzero status rejects.
	BeforeDispatch func(context.Context, config.LocalAccount, []byte) (func(), int)
	// BeforeDispatchAccount is the per-WebSocket-turn variant for an explicitly
	// authorized account set. It may replace the socket's default account and
	// must return a current check for that exact identity plus a release held
	// through response drain. Ordinary callers keep BeforeDispatch unchanged.
	BeforeDispatchAccount func(context.Context, config.LocalAccount, []byte) (config.LocalAccount, func() bool, func(), int)
	// NormalizeRequest resolves caller-specific metadata before translation;
	// prompt, input history and arbitrary tool arguments remain untouched.
	NormalizeRequest func([]byte) ([]byte, int)
	// Observe sees only sanitized metadata and genuine upstream usage snapshots.
	// It is called once per authenticated request, never once per SSE event.
	Observe    func(usageledger.Observation)
	BeginUsage func(string, uint64) func()
	Key        func() string
	Source     func(string) (config.LocalAccount, bool)
	// KeyForTarget optionally separates local tools' persistent credentials from
	// remote workers' process leases. When nil, Key retains its legacy behavior.
	KeyForTarget func(string) string
	// AuthorizationRevision optionally fences queued work across a committed
	// route change, even when credentials/settings are later restored verbatim.
	AuthorizationRevision func(string) string
	// Accounts and Pool are optional. When configured, Pool is used only for
	// accounts explicitly marked poolEnabled; legacy Source behavior remains
	// unchanged for manual accounts and for callers that do not provide a pool.
	Accounts  func(string) []config.LocalAccount
	PoolGroup func(string) string
	// PoolEnabled binds pooling to the selected tool route, not merely to the
	// presence of some pool member in the vault. Nil retains standalone callers'
	// historical opt-in through Pool + Accounts.
	PoolEnabled   func(string) bool
	Pool          *AccountPool
	Client        *http.Client
	RefreshModels func(context.Context, string, config.LocalAccount) ([]string, error)
	// Official executes only the explicitly selected protected account.
	Official func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error)
	// RetryAccount is supplied only by an explicitly authorized named text pool.
	// It is never used after output or an ambiguous upstream outcome.
	RetryAccount          func(context.Context, []byte, []string) (config.LocalAccount, func() bool, func(), int)
	InitialAttemptRelease func()
	AttemptObserver       func(config.LocalAccount, usageledger.Observation)
	FirstByteTimeout      time.Duration
	StreamIdleTimeout     time.Duration
	attempt               *attemptControl
}

func New(key func() string, source func(string) (config.LocalAccount, bool)) *Handler {
	// Translators must not log request bodies, private prompts or upstream keys.
	log.SetOutput(io.Discard)
	return &Handler{Key: key, Source: source, Client: &http.Client{
		Transport:     &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 20 * time.Second, ResponseHeaderTimeout: 120 * time.Second},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (h *Handler) gatewayKey(target string) string {
	if h.KeyForTarget != nil {
		return h.KeyForTarget(target)
	}
	if h.Key != nil {
		return h.Key()
	}
	return ""
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
	if config.IsAdditionalTool(kind) {
		return translator.FormatOpenAI
	}
	if kind == "codex" {
		return translator.FormatOpenAIResponse
	}
	return translator.FormatClaude
}

func retryAfter(response *http.Response) time.Duration {
	if response == nil {
		return 0
	}
	return apierror.RetryAfterDuration(response.Header)
}

func requestModel(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var input struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &input) != nil {
		return ""
	}
	return strings.TrimSpace(input.Model)
}

// requestSessionKey keeps a conversation on one upstream account. The key is
// a local routing hint, never sent to the Hub or provider. Explicit headers are
// accepted only from the loopback tool; otherwise common request metadata is
// used conservatively and an empty key disables affinity.
func requestSessionKey(r *http.Request, body []byte) string {
	if value := strings.TrimSpace(r.Header.Get("X-Salcara-Session")); value != "" && len(value) <= 256 {
		return value
	}
	var input struct {
		ConversationID string `json:"conversation_id"`
		SessionID      string `json:"session_id"`
		Previous       string `json:"previous_response_id"`
		User           string `json:"user"`
		Metadata       struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if json.Unmarshal(body, &input) != nil {
		return ""
	}
	for _, value := range []string{input.ConversationID, input.SessionID, input.Metadata.UserID, input.User, input.Previous} {
		value = strings.TrimSpace(value)
		if value != "" && len(value) <= 256 {
			return value
		}
	}
	return ""
}

func (h *Handler) poolActive(target string) bool {
	// Remote pooling needs per-member provenance. Local pooling is unchanged.
	return !strings.HasPrefix(target, "remote-") && !strings.HasPrefix(target, "direct-") && h.Pool != nil && h.Accounts != nil && (h.PoolEnabled == nil || h.PoolEnabled(target))
}

func (h *Handler) poolPolicy(target string) (PoolPolicy, bool) {
	if h.Source != nil {
		if anchor, authorized := h.Source(target); authorized {
			return PoolPolicy{Strategy: anchor.PoolStrategy, SingleAccountID: anchor.ID, Revision: config.AccountFingerprint(anchor)}, true
		}
	}
	// PoolEnabled is set by the production console and means this route requires
	// an explicitly authorized applied source. Nil retains historical standalone
	// tests/embedders that supplied only Pool + Accounts and use legacy policy.
	if h.PoolEnabled != nil {
		return PoolPolicy{}, false
	}
	return PoolPolicy{}, true
}

func (h *Handler) resolveAccount(target string, r *http.Request, body []byte) (config.LocalAccount, func(), bool) {
	if h.poolActive(target) {
		candidates := h.Accounts(target)
		poolConfigured := false
		for _, candidate := range candidates {
			if candidate.PoolEnabled && strings.TrimSpace(candidate.PoolGroup) != "" {
				poolConfigured = true
				break
			}
		}
		if poolConfigured {
			policy, authorized := h.poolPolicy(target)
			if !authorized {
				return config.LocalAccount{}, nil, false
			}
			group := ""
			if h.PoolGroup != nil {
				group = strings.TrimSpace(h.PoolGroup(target))
			}
			// Reading a cached catalog must work while every task slot is occupied
			// or all providers are cooling; it never consumes an inference slot.
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v1/models") {
				account, err := h.Pool.RepresentativeWithPolicy(target, candidates, group, policy)
				return account, func() {}, err == nil
			}
			account, release, err := h.Pool.SelectWithPolicy(target, candidates, group, requestModel(body), requestSessionKey(r, body), policy)
			if err == nil {
				return account, release, true
			}
			// A pool miss must not silently select a different, ungrouped Key. This
			// is the important boundary between Cockpit-style pool selection and the
			// legacy manually applied account.
			return config.LocalAccount{}, nil, false
		}
	}
	if h.Source == nil {
		return config.LocalAccount{}, nil, false
	}
	account, ok := h.Source(target)
	if ok && h.Pool != nil && h.Accounts != nil && !(r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v1/models")) {
		release, err := h.Pool.AdmitFixed(account, h.Accounts(target))
		if err != nil {
			return config.LocalAccount{}, nil, false
		}
		return account, release, true
	}
	return account, func() {}, ok
}

func (h *Handler) accountStillCurrent(target string, selected config.LocalAccount) bool {
	return h.accountStillCurrentFingerprint(target, selected.ID, config.AccountFingerprint(selected))
}

func (h *Handler) accountStillCurrentFingerprint(target, accountID, fingerprint string) bool {
	if h.poolActive(target) {
		group := ""
		if h.PoolGroup != nil {
			group = strings.TrimSpace(h.PoolGroup(target))
		}
		for _, current := range h.Accounts(target) {
			if current.ID == accountID && current.PoolEnabled && (group == "" || current.PoolGroup == group) && poolTargetMatches(current, target) && config.AccountFingerprint(current) == fingerprint {
				return true
			}
		}
		return false
	}
	current, ok := h.Source(target)
	return ok && current.ID == accountID && config.AccountFingerprint(current) == fingerprint
}

func (h *Handler) serveHTTP(w http.ResponseWriter, r *http.Request) {
	var observation *requestObservation
	var releaseUsage func()
	defer func() {
		if releaseUsage != nil {
			releaseUsage()
		}
	}()
	defer func() { observation.finish() }()
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
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/gateway/"), "/", 2)
	if len(parts) != 2 || !validGatewayTarget(parts[0]) {
		fail(w, 404, "无效的工具路由")
		return
	}
	revision := ""
	if h.AuthorizationRevision != nil {
		revision = h.AuthorizationRevision(parts[0])
	}
	currentAuthorization := func() bool {
		return h.AuthorizationRevision == nil || revision != "" && h.AuthorizationRevision(parts[0]) == revision
	}
	key := h.gatewayKey(parts[0])
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if r.Header.Get("Authorization") == "" {
		provided = r.Header.Get("x-api-key")
	}
	if key == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(key)) != 1 {
		fail(w, 401, "本地协议网关凭据无效")
		return
	}
	observer := h.Observe
	var observedAccount config.LocalAccount
	if h.AttemptObserver != nil {
		observer = func(o usageledger.Observation) {
			h.AttemptObserver(observedAccount, o)
			if h.Observe != nil {
				h.Observe(o)
			}
		}
	}
	plugins := h.Plugins
	if strings.HasPrefix(parts[0], "remote-") || strings.HasPrefix(parts[0], "direct-") {
		plugins = nil
	}
	if plugins.Enabled() {
		downstreamObserver := observer
		observer = func(o usageledger.Observation) {
			plugins.FireEvent(context.Background(), "usage", map[string]any{"tool": parts[0], "model": o.Model, "httpStatus": o.HTTPStatus, "durationMs": o.DurationMS, "tokens": o.Tokens, "state": o.State, "usageFinal": o.UsageFinal})
			if downstreamObserver != nil {
				downstreamObserver(o)
			}
		}
	}
	w, observation = beginRequestObservation(w, r, observer, parts[0]+"/"+parts[1])
	if !currentAuthorization() {
		fail(w, 409, "API 授权已变更，请重试")
		return
	}
	path := parts[1]
	var prefetchedBody []byte
	bodyPrefetched := false
	// Account selection needs the requested model/session before the request is
	// forwarded. Read once and reuse below; the 64 MiB cap is the same as the
	// existing gateway body limit.
	if h.Pool != nil && r.Method == http.MethodPost && strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var err error
		prefetchedBody, err = io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		if err != nil || len(prefetchedBody) > maxBody || !json.Valid(prefetchedBody) {
			fail(w, 400, "请求不是有效 JSON 或内容超过本地网关限制")
			return
		}
		bodyPrefetched = true
	}
	modelLimit, modelAfter := 1000, ""
	claudeBeta := false
	if r.URL.RawQuery != "" {
		if claudeBetaQuery(r, parts[0], path) {
			claudeBeta = true
		} else if parts[0] == "claude-desktop" && path == "v1/models" && r.Method == http.MethodGet {
			var err error
			modelLimit, modelAfter, err = desktopModelsQuery(r.URL.RawQuery)
			if err != nil {
				fail(w, 400, "模型分页参数无效")
				return
			}
		} else {
			fail(w, 404, "无效的工具路由")
			return
		}
	}
	if !currentAuthorization() {
		fail(w, 409, "API 授权已变更，请重试")
		return
	}
	a, releaseAccount, valid := h.resolveAccount(parts[0], r, prefetchedBody)
	if !valid {
		fail(w, 409, "没有可用的账号池账号，请检查账号池成员、模型目录或并发状态；不会退回其他未授权 Key")
		return
	}
	defer releaseAccount()
	observedAccount = a
	if h.attempt != nil {
		h.attempt.account = a
	}
	observation.account(a.ID, a.CredentialEpoch)
	if h.BeginUsage != nil && r.Method == http.MethodPost {
		releaseUsage = h.BeginUsage(a.ID, a.CredentialEpoch)
	}
	// Snapshot the applied connection before choosing a per-request protocol.
	// Slow discovery/body reads must not send with a removed or switched Key.
	selectedID, selectedFingerprint := a.ID, config.AccountFingerprint(a)
	stillApplied := func() bool {
		currentKey := h.gatewayKey(parts[0])
		return currentAuthorization() && h.accountStillCurrentFingerprint(parts[0], selectedID, selectedFingerprint) &&
			currentKey != "" && subtle.ConstantTimeCompare([]byte(currentKey), []byte(key)) == 1
	}
	if parts[0] == "claude-desktop" && len(a.Models) == 0 && h.RefreshModels != nil && (path == "v1/models" || path == "v1/messages" || path == "v1/messages/count_tokens") {
		models, err := h.RefreshModels(r.Context(), parts[0], a)
		if err != nil {
			fail(w, 502, "模型目录读取失败，请在 Agent 中刷新模型")
			return
		}
		a.Models = models
		// RefreshModels persists the catalog in the account snapshot. Refresh the
		// captured fingerprint as well; otherwise the same request would compare
		// its pre-discovery fingerprint with the newly persisted catalog and report
		// a false concurrent API switch.
		selectedFingerprint = config.AccountFingerprint(a)
	}
	// The socket re-validates the live source on every frame itself.
	if a.Kind == "codex" && path == "v1/responses" && r.Method == http.MethodGet && websocket.IsWebSocketUpgrade(r) {
		if observation != nil {
			observation.skip = true
		} // Each real socket turn uses the inner HTTP hook.
		if a.SourceType == "oauth" || a.SourceType == "official_account" && (a.CredentialRef == "" || h.Official == nil) {
			fail(w, 501, "官方账号模式使用 HTTP/SSE")
			return
		}
		h.serveWebSocket(w, r, a, parts[0], key, currentAuthorization)
		return
	}
	if !stillApplied() {
		fail(w, 409, "API 配置已在请求期间变更，请重试；没有使用旧 Key 发送请求")
		return
	}
	if path == "v1/models" && r.Method == http.MethodGet {
		observation.state("read_only")
		if parts[0] == "claude-desktop" {
			catalogAccount := a
			pooledCatalog := h.poolCatalog(parts[0])
			usingPooledCatalog := h.poolActive(parts[0]) && h.hasPoolMembers(parts[0])
			if usingPooledCatalog {
				catalogAccount.Models = pooledCatalog
				catalogAccount.Model = ""
			}
			models, labels := config.ClaudeDesktopCatalog(catalogAccount)
			filtered := models[:0]
			for _, model := range models {
				// A pooled catalog has already applied each member's model policy.
				// Keep the selected-account check for the legacy single-account path.
				if usingPooledCatalog || config.ModelAllowed(a, model) {
					filtered = append(filtered, model)
				}
			}
			models = filtered
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
		usingPooledCatalog := h.poolActive(parts[0]) && h.hasPoolMembers(parts[0])
		models := h.poolCatalog(parts[0])
		if !usingPooledCatalog {
			models = append([]string{}, a.Models...)
			if a.Model != "" {
				models = append(models, a.Model)
			}
		}
		seen, data := map[string]bool{}, []map[string]string{}
		for _, m := range models {
			if (usingPooledCatalog || config.ModelAllowed(a, m)) && !seen[m] {
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
	compact := a.Kind == "codex" && path == "v1/responses/compact"
	chat := (config.IsAdditionalTool(a.Kind) || h.ChatCompletions && a.Kind == "codex") && path == "v1/chat/completions"
	expected := "v1/messages"
	if config.IsAdditionalTool(a.Kind) {
		expected = "v1/chat/completions"
	}
	if a.Kind == "codex" {
		expected = "v1/responses"
	}
	count := a.Kind == "claude" && path == "v1/messages/count_tokens"
	if path != expected && !count && !compact && !chat {
		fail(w, 404, "这个路由不支持请求的功能")
		return
	}
	body := prefetchedBody
	var err error
	if !bodyPrefetched {
		body, err = io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	}
	if err != nil || len(body) > maxBody || !json.Valid(body) {
		fail(w, 400, "请求不是有效 JSON 或内容超过本地网关限制")
		return
	}
	pluginHeaders := map[string]string{}
	var input struct {
		Model        string          `json:"model"`
		Stream       bool            `json:"stream"`
		Previous     string          `json:"previous_response_id"`
		Conversation json.RawMessage `json:"conversation"`
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
	if !config.ModelAllowed(a, input.Model) {
		fail(w, 403, "当前 API 账号禁止使用这个模型")
		return
	}
	observation.model(input.Model, a.Key, key)
	for _, m := range a.Models {
		known = known || m == input.Model
	}
	// The native Codex picker may publish its bundled catalog, which is not an
	// entitlement check. Forward an explicit official model unchanged; do not
	// reject it solely because no provider /models endpoint exists.
	if a.CredentialRef != "" && a.SourceType == "official_account" && len(input.Model) <= 200 && !strings.ContainsAny(input.Model, "\r\n\x00") {
		known = true
	}
	if !known {
		fail(w, 400, "模型不在此 Key 的配置目录中；请先在工具卡片选择或读取目录。不会退回其他模型")
		return
	}
	if h.NormalizeRequest != nil {
		var status int
		body, status = h.NormalizeRequest(body)
		if status != 0 {
			fail(w, status, "请求的模型命名空间或工具模型权限无效")
			return
		}
	}
	// The tool may switch models mid-thread (Codex model picker, phone model chip):
	// pick the upstream wire per request from the model family / provider type.
	if p := config.InferProtocol(input.Model, a.Wire); p != "" {
		a.Protocol = p
	}
	from, suffix := wire(a)
	if compact {
		// Compaction output contains opaque provider state. Forward only native
		// Responses requests using the selected API key; no translation can
		// reproduce that state for a different protocol or credential source.
		if a.Kind != "codex" || (a.Protocol != "" && a.Protocol != "responses") || (a.SourceType != "" && a.SourceType != "api_key" && a.SourceType != "reverse_proxy" && !(a.SourceType == "official_account" && a.CredentialRef != "" && h.Official != nil)) {
			fail(w, 501, "远程压缩仅支持 API Key 账号的原生 Responses 接口")
			return
		}
		if input.Stream {
			fail(w, 400, "远程压缩不支持流式请求")
			return
		}
		if input.Previous != "" && len(input.Conversation) != 0 && !bytes.Equal(bytes.TrimSpace(input.Conversation), []byte("null")) {
			fail(w, 400, "previous_response_id 与 conversation 不能同时使用")
			return
		}
		suffix = "/responses/compact"
	}
	if parts[0] == "claude-desktop" && config.NeedsAdapter(a) {
		body = stripDesktopOnlyOptions(body)
	}
	to := clientFormat(a.Kind)
	adapt := config.NeedsAdapter(a)
	if chat {
		to = translator.FormatOpenAI
		adapt = from != to
		if status, message := validateChatRequest(body, from); status != 0 {
			fail(w, status, message)
			return
		}
	}
	if count {
		if from != translator.FormatClaude {
			observation.state("local_estimate")
			// Conservative local estimate, explicitly marked as such. It is never
			// a billable usage figure or a claim about the provider's tokenizer.
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Salcara-Token-Count", "local-estimate")
			_ = json.NewEncoder(w).Encode(map[string]int{"input_tokens": (len(body)+2)/3 + 256})
			return
		}
		suffix = "/messages/count_tokens"
	}
	if adapt && input.Previous != "" {
		fail(w, 400, "跨协议模式不支持只传 previous_response_id；请让工具携带完整原会话上下文")
		return
	}
	if a.CredentialRef != "" && a.SourceType == "official_account" && !compact && (input.Previous != "" || len(input.Conversation) != 0 && !bytes.Equal(bytes.TrimSpace(input.Conversation), []byte("null"))) {
		// The legacy SDK's Codex route is stateless and deletes continuation
		// references. Reject instead of silently discarding the old context.
		fail(w, 400, "官方账号接口需要完整会话上下文，不支持只传 previous_response_id 或 conversation")
		return
	}
	if plugins.Enabled() {
		body, pluginHeaders, _ = plugins.RunRequestHooks(r.Context(), parts[0], body, nil)
		clientBody = replaceModelJSON(body, input.Model, clientModel)
	}
	upstreamStream := input.Stream || (!count && adapt && (from == translator.FormatClaude || from == translator.FormatCodex))
	converted := body
	if !count && adapt {
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
	if !count && !compact {
		var speedErr error
		converted, speedErr = applyAccountSpeed(converted, a)
		if speedErr != nil {
			fail(w, 400, "无法设置请求速度档位")
			return
		}
	}
	_, root, err := config.APIBase(a.BaseURL)
	if err != nil {
		fail(w, 400, "上游地址无效")
		return
	}
	timeout := 2 * time.Hour
	if compact {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
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
		if to == translator.FormatClaude {
			if claudeBeta {
				request.URL.RawQuery = "beta=true"
			}
			// Native Claude uses these feature flags with Messages beta fields.
			// Carry only the protocol header, never arbitrary client headers.
			if beta := r.Header.Get("anthropic-beta"); beta != "" {
				request.Header.Set("anthropic-beta", beta)
			}
		}
	}
	// Inject per-account extra headers declared in the account config.
	// This must run after auth headers are set so it can add supplemental headers
	// without overriding Authorization or Host.
	InjectAccountHeaders(request, a)
	for name, value := range pluginhook.SafeHeaders(pluginHeaders) {
		request.Header.Set(name, value)
	}
	if !stillApplied() {
		fail(w, 409, "API 配置已在请求期间变更，请重试；没有使用旧 Key 发送请求")
		return
	}
	var response *http.Response
	if h.BeforeDispatch != nil {
		release, status := h.BeforeDispatch(ctx, a, body)
		if release != nil {
			defer release()
		}
		if status != 0 {
			if status < 400 || status > 599 {
				status = http.StatusInternalServerError
			}
			fail(w, status, "当前请求的执行限制不允许继续")
			return
		}
		if !stillApplied() {
			fail(w, 409, "API 授权已在等待执行资源期间变更，请重新连接")
			return
		}
	}
	watch := newResponseWatch(ctx, h.FirstByteTimeout, h.StreamIdleTimeout)
	observation.watch = watch
	defer watch.stop()
	ctx = watch.ctx
	request = request.WithContext(ctx)
	if h.attempt != nil {
		h.attempt.safeInput = replayableTextBody(body) && replayableTextBody(converted)
	}
	if a.SourceType == "oauth" || a.SourceType == "official_account" {
		if h.Official == nil || a.CredentialRef == "" || from != translator.FormatCodex {
			fail(w, 501, "此账号尚未连接官方账号执行器")
			return
		}
		observation.dispatch()
		response, err = h.Official(ctx, a, converted, upstreamStream, compact, stillApplied)
	} else {
		observation.dispatch()
		response, err = h.Client.Do(request)
	}
	if err != nil {
		var localAdmission interface{ LocalAdmissionStatus() int }
		if errors.As(err, &localAdmission) {
			observation.dispatched = false
			observation.state("rejected")
		}
		reason := "unknown_result"
		stage := "connect"
		if watch.reason() != "" {
			reason, stage = watch.reason(), "headers"
		}
		status, delay, safe, rejection := executorRejection(err)
		if status != 0 {
			observation.response(status)
			reason, stage = rejection, "upstream"
		}
		observation.failure(stage, reason, delay)
		if h.attempt != nil {
			h.attempt.safeRetry = safe
		}
		if h.Pool != nil && r.Context().Err() == nil && (credentialCooldownError(err) || watch.reason() != "") {
			h.Pool.MarkFailureFor(a.ID, selectedFingerprint, delay)
		}
		if watch.reason() != "" {
			fail(w, 504, "等待上游响应超时，请稍后重试；未重放请求")
			return
		}
		if failExecutorError(w, err) {
			return
		}
		fail(w, 502, "上游连接失败，请检查地址、网络和接口协议")
		return
	}
	defer response.Body.Close()
	response.Body = watch.wrap(response.Body)
	observation.response(response.StatusCode)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		rawError, readErr := io.ReadAll(io.LimitReader(response.Body, maxProviderErrorBytes+1))
		code := ""
		if readErr == nil && len(rawError) <= maxProviderErrorBytes {
			observation.json(rawError)
			code = providerExecutionCode(rawError)
		}
		delay := retryAfter(response)
		safe, reason := responseRejection(response.StatusCode, code)
		observation.failure("upstream", reason, delay)
		if h.attempt != nil {
			h.attempt.safeRetry = safe && readErr == nil
		}
		if h.Pool != nil && credentialCooldownCode(code) {
			h.Pool.MarkFailureFor(a.ID, selectedFingerprint, retryAfter(response))
		}
		if code == "model_capacity" {
			failModelCapacity(w, response.StatusCode)
		} else {
			fail(w, response.StatusCode, fmt.Sprintf("上游返回 HTTP %d，请检查这个 Key 的模型权限和接口；未更换模型", response.StatusCode))
		}
		return
	}
	if upstreamStream && !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		if h.Pool != nil {
			h.Pool.MarkFailureFor(a.ID, selectedFingerprint, 0)
		}
		fail(w, 502, "上游没有返回所需的流式接口")
		return
	}
	if input.Stream {
		if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
			if h.Pool != nil {
				h.Pool.MarkFailureFor(a.ID, selectedFingerprint, 0)
			}
			fail(w, 502, "上游没有返回所需的流式接口")
			return
		}
		w = plugins.ObserveStream(w, ctx, parts[0])
		completed, failureCode := h.stream(w, ctx, response.Body, from, to, clientModel, input.Model, clientBody, converted, a, adapt, observation)
		if failureCode == "model_capacity" {
			observation.failure("upstream", "model_unavailable", 0)
		}
		if watch.reason() != "" {
			stage := "stream"
			if watch.reason() == "headers_timeout" {
				stage = "headers"
			}
			observation.failure(stage, watch.reason(), 0)
			if r.Context().Err() == nil {
				if observation.writer.status == 0 {
					fail(w, 504, "等待上游响应超时；未重放请求")
				} else {
					streamExecutionError(w, "")
				}
			}
		}
		observation.stream(completed)
		if h.Pool != nil {
			if completed {
				h.Pool.MarkSuccessFor(a.ID, selectedFingerprint)
			} else if !observation.writer.writeFailed && r.Context().Err() == nil && (ctx.Err() == nil || watch.reason() != "") && credentialCooldownCode(failureCode) {
				h.Pool.MarkFailureFor(a.ID, selectedFingerprint, 0)
			}
		}
		return
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if len(raw) <= maxBody {
		if upstreamStream {
			observation.sse(raw)
		} else {
			observation.json(raw)
		}
	}
	if err != nil || len(raw) > maxBody || (a.Key != "" && bytes.Contains(raw, []byte(a.Key))) {
		if reason := watch.reason(); reason != "" {
			stage := "stream"
			if reason == "headers_timeout" {
				stage = "headers"
			}
			observation.failure(stage, reason, 0)
			if h.Pool != nil && r.Context().Err() == nil {
				h.Pool.MarkFailureFor(a.ID, selectedFingerprint, 0)
			}
			fail(w, 504, "等待上游响应超时；未重放请求")
			return
		}
		if h.Pool != nil && r.Context().Err() == nil {
			h.Pool.MarkFailureFor(a.ID, selectedFingerprint, 0)
		}
		fail(w, 502, "上游响应无效或读取失败")
		return
	}
	if chat && upstreamStream {
		if code, failed := chatStreamFailure(raw); failed {
			observation.stream(false)
			if h.Pool != nil && credentialCooldownCode(code) {
				h.Pool.MarkFailureFor(a.ID, selectedFingerprint, 0)
			}
			if code == "model_capacity" {
				failModelCapacity(w, http.StatusServiceUnavailable)
			} else {
				fail(w, 502, "上游流返回错误，不能把部分响应当作成功结果")
			}
			return
		}
	}
	if upstreamStream && !hasTerminalSSE(raw) {
		observation.stream(false)
		if h.Pool != nil {
			h.Pool.MarkFailureFor(a.ID, selectedFingerprint, 0)
		}
		fail(w, 502, "上游流未完成，不能把截断的响应当作成功结果")
		return
	}
	if upstreamStream && from == translator.FormatCodex && (to == translator.FormatClaude || to == translator.FormatOpenAI) {
		for _, line := range bytes.Split(raw, []byte("\n")) {
			data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if bytes.HasPrefix(line, []byte("data:")) && terminalData(data) {
				raw = append([]byte{}, data...)
				break
			}
		}
	}
	if !count && adapt {
		var state any
		upstreamRaw := raw
		raw = translator.TranslateNonStream(ctx, from, to, clientModel, clientBody, converted, raw, &state)
		if chat && from == translator.FormatCodex && to == translator.FormatOpenAI {
			raw = repairCodexChatNonStream(upstreamRaw, raw)
		}
	}
	if clientModel != input.Model {
		raw = replaceModelJSON(raw, input.Model, clientModel)
	}
	if !json.Valid(raw) {
		if h.Pool != nil {
			h.Pool.MarkFailureFor(a.ID, selectedFingerprint, 0)
		}
		fail(w, 502, "上游响应无法转换为所需格式")
		return
	}
	if h.Pool != nil {
		h.Pool.MarkSuccessFor(a.ID, selectedFingerprint)
	}
	responseStatus := http.StatusOK
	if plugins.Enabled() {
		responseStatus, raw, _ = plugins.RunResponseHooks(ctx, parts[0], clientBody, responseStatus, raw)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(responseStatus)
	_, _ = w.Write(raw)
}

// poolCatalog returns a stable union of the cached model catalogs for the
// current target's explicitly enabled pool. A single selected account cannot
// accurately advertise a GPT reverse-proxy pool when different upstream
// accounts expose different model sets; advertising the union lets the next
// request be routed to the member that actually supports its chosen model.
func (h *Handler) poolCatalog(target string) []string {
	if !h.poolActive(target) {
		return nil
	}
	accounts := h.Accounts(target)
	group := ""
	if h.PoolGroup != nil {
		group = strings.TrimSpace(h.PoolGroup(target))
	}
	policy, authorized := h.poolPolicy(target)
	if !authorized {
		return nil
	}
	return h.Pool.CatalogWithPolicy(target, accounts, group, policy)
}

func (h *Handler) hasPoolMembers(target string) bool {
	if !h.poolActive(target) {
		return false
	}
	for _, account := range h.Accounts(target) {
		if account.PoolEnabled && strings.TrimSpace(account.PoolGroup) != "" {
			return true
		}
	}
	return false
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
	streamExecutionError(w, "")
}

func streamExecutionError(w http.ResponseWriter, code string) {
	message := `{"type":"error","error":{"type":"upstream_stream_error","message":"上游流中断或返回错误；本轮未完成，没有自动重试或切换模型"}}`
	if code == "model_capacity" {
		message = `{"type":"error","error":{"type":"upstream_stream_error","code":"model_capacity","message":"Selected model is at capacity. Please try a different model."}}`
	}
	_, _ = io.WriteString(w, "event: error\ndata: "+message+"\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func (h *Handler) stream(w http.ResponseWriter, ctx context.Context, reader io.Reader, from, to translator.Format, model, upstreamModel string, original, request []byte, a config.LocalAccount, adapt bool, observation *requestObservation) (bool, string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	var state any
	var refusalState chatRefusalStreamState
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	terminal := false
	chatDone := false
	write := func(chunk []byte) bool {
		if len(chunk) == 0 {
			return true
		}
		if a.Key != "" && bytes.Contains(chunk, []byte(a.Key)) {
			return false
		}
		// The SDK's Chat translators return JSON payloads, while our streaming
		// boundary writes complete SSE frames. Keep the wire contract explicit.
		if adapt && to == translator.FormatOpenAI && json.Valid(chunk) {
			chunk = append(append([]byte("data: "), chunk...), []byte("\n\n")...)
		}
		if model != upstreamModel {
			chunk = replaceSSEModel(chunk, upstreamModel, model)
		}
		if !(adapt && to == translator.FormatOpenAI) && hasTerminalSSE(chunk) {
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
	failureCode := ""
	dispatch := func() bool {
		if len(dataLines) == 0 {
			eventName = ""
			frameSize = 0
			return true
		}
		data := bytes.Join(dataLines, []byte("\n"))
		dataLines = nil
		if adapt && to == translator.FormatOpenAI && chatDone && terminalData(data) {
			eventName, frameSize = "", 0
			return true
		}
		if a.Key != "" && bytes.Contains(data, []byte(a.Key)) {
			failed = true
			return false
		}
		observation.event(data)
		var event map[string]any
		if json.Unmarshal(data, &event) == nil && (event["error"] != nil || event["type"] == "error" || event["type"] == "response.failed" || event["type"] == "response.incomplete") {
			failed = true
			failureCode = providerExecutionCode(data)
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
		if from == translator.FormatCodex && to == translator.FormatOpenAI {
			chunks, valid := translateCodexChatRefusal(data, upstreamModel, &refusalState)
			if !valid {
				failed = true
				return false
			}
			for _, chunk := range chunks {
				if !write(chunk) {
					failed = true
					return false
				}
			}
		}
		// Translators accept one data: payload at a time, not the event label.
		line := append([]byte("data: "), data...)
		for _, chunk := range translator.TranslateStream(ctx, from, to, model, original, request, line, &state) {
			if !write(chunk) {
				failed = true
				return false
			}
		}
		if to == translator.FormatOpenAI && terminalData(data) {
			terminal = true
			chatDone = true
			if !write([]byte("data: [DONE]\n\n")) {
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
		streamExecutionError(w, failureCode)
	}
	return !failed && terminal && scanner.Err() == nil, failureCode
}
