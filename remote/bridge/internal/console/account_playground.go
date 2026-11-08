package console

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/gateway"
	"salcara/bridge/internal/usageledger"
)

type accountTestTarget struct {
	AccountID  string `json:"accountId,omitempty"`
	OfficialID string `json:"officialId,omitempty"`
}

type accountTestMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// The browser submits a saved identity, never a credential or arbitrary URL.
// Vault-only accounts can be tested without creating a new API card or binding.
func (s *Server) accountTestSource(target accountTestTarget) (config.LocalAccount, int) {
	if (target.AccountID == "") == (target.OfficialID == "") {
		return config.LocalAccount{}, http.StatusBadRequest
	}
	c := s.d.Store.Get()
	var a config.LocalAccount
	if target.AccountID != "" {
		var ok bool
		a, ok = c.LocalAccount(target.AccountID)
		if !ok {
			return a, http.StatusNotFound
		}
	} else {
		if !config.ValidAccountID(target.OfficialID) || s.d.CodexAccounts == nil {
			return a, http.StatusNotFound
		}
		row, err := s.d.CodexAccounts.Get(target.OfficialID)
		if err != nil {
			return a, http.StatusNotFound
		}
		if row.Mode == "chatgpt_web_session" {
			return a, http.StatusUnprocessableEntity
		}
		// Preserve saved account policy if a formerly vault-only row was linked.
		for _, candidate := range c.LocalAccounts {
			if candidate.CredentialRef == row.ID {
				return config.LocalAccount{}, http.StatusConflict
			}
		}
		a = config.LocalAccount{ID: "official-" + row.ID, Name: row.Name, Kind: "api", SourceType: "official_account", ProviderID: "openai", CredentialRef: row.ID, CredentialEpoch: row.IdentityEpoch, BaseURL: "https://chatgpt.com/backend-api/codex", Key: "protected-" + row.ID, Wire: "responses", AuthMode: "bearer"}
	}
	if config.ValidateLocalAccount(&a) != nil || a.SourceType == "oauth" {
		return a, http.StatusUnprocessableEntity
	}
	if a.SourceType == "official_account" {
		if s.d.CodexAccounts == nil || s.d.OfficialGateway == nil {
			return a, http.StatusServiceUnavailable
		}
		row, err := s.d.CodexAccounts.Get(a.CredentialRef)
		if err != nil || row.IdentityEpoch != a.CredentialEpoch || row.Mode == "chatgpt_web_session" {
			return a, http.StatusConflict
		}
	}
	return a, 0
}

func (s *Server) accountTestRevision(a config.LocalAccount) string {
	mac := hmac.New(sha256.New, []byte(s.token))
	_, _ = mac.Write([]byte("account-test-v1\x00" + a.ID + "\x00" + config.AccountFingerprint(a)))
	return hex.EncodeToString(mac.Sum(nil))
}

func accountTestSourceError(w http.ResponseWriter, status int) {
	message := map[int]string{400: "请选择一个 API 或账号", 404: "账号已移除，请刷新列表", 409: "账号或绑定已变化，请重新打开测试窗口", 422: "此来源不能发送模型请求；网页会话仅支持额度查询", 503: "官方账号执行器尚未就绪"}[status]
	writeErr(w, status, message)
}

func (s *Server) handleAccountTestPrepare(w http.ResponseWriter, r *http.Request) {
	var target accountTestTarget
	if r.URL.RawQuery != "" || readReverseJSON(r, &target, 2048) != nil {
		writeErr(w, 400, "测试目标无效")
		return
	}
	a, status := s.accountTestSource(target)
	if status != 0 {
		accountTestSourceError(w, status)
		return
	}
	models, seen := []string{}, map[string]bool{}
	for _, model := range append([]string{a.Model}, a.Models...) {
		if config.ValidCatalogModel(model) && config.ModelAllowed(a, model) && !seen[model] && !strings.Contains(model, a.Key) {
			seen[model] = true
			models = append(models, model)
		}
		if len(models) == 500 {
			break
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{"name": a.Name, "revision": s.accountTestRevision(a), "models": models, "sourceType": a.SourceType})
}

func validAccountTestMessages(messages []accountTestMessage) bool {
	if len(messages) < 1 || len(messages) > 21 || len(messages)%2 != 1 {
		return false
	}
	total := 0
	for i, m := range messages {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		if m.Role != role || strings.TrimSpace(m.Content) == "" || len(m.Content) > 16384 || strings.ContainsRune(m.Content, '\x00') {
			return false
		}
		total += len(m.Content)
	}
	return total <= 64<<10
}

func (s *Server) acquireAccountTest(a config.LocalAccount) (func(), bool) {
	identity, valid := gateway.CredentialBudgetIdentity(a)
	if !valid {
		return nil, false
	}
	s.accountTestMu.Lock()
	defer s.accountTestMu.Unlock()
	if s.accountTests == nil {
		s.accountTests = map[string]bool{}
	}
	if s.accountTests[identity] || len(s.accountTests) >= 4 {
		return nil, false
	}
	s.accountTests[identity] = true
	return func() {
		s.accountTestMu.Lock()
		delete(s.accountTests, identity)
		s.accountTestMu.Unlock()
	}, true
}

func (s *Server) handleAccountTestSend(w http.ResponseWriter, r *http.Request) {
	if !s.admitUpdateWork(w) {
		return
	}
	var in struct {
		AccountID  string               `json:"accountId,omitempty"`
		OfficialID string               `json:"officialId,omitempty"`
		Revision   string               `json:"revision"`
		Model      string               `json:"model"`
		Messages   []accountTestMessage `json:"messages"`
	}
	if r.URL.RawQuery != "" || readReverseJSON(r, &in, 96<<10) != nil || !validAccountTestMessages(in.Messages) || !config.ValidCatalogModel(in.Model) || len(in.Revision) != 64 {
		writeErr(w, 400, "请填写有效模型和测试消息；最多保留 10 轮对话，单条不超过 16 KiB")
		return
	}
	target := accountTestTarget{AccountID: in.AccountID, OfficialID: in.OfficialID}
	a, status := s.accountTestSource(target)
	if status != 0 {
		accountTestSourceError(w, status)
		return
	}
	if !hmac.Equal([]byte(in.Revision), []byte(s.accountTestRevision(a))) {
		accountTestSourceError(w, 409)
		return
	}
	if !config.ModelAllowed(a, in.Model) || strings.Contains(in.Model, a.Key) {
		writeErr(w, 403, "当前账号的模型规则不允许这个模型")
		return
	}
	release, acquired := s.acquireAccountTest(a)
	if !acquired {
		writeErr(w, 429, "这个凭据正在测试，或同时测试的账号过多；请等本次结束")
		return
	}
	defer release()
	// Tests are short, text-only, explicit user requests. They never retry or
	// save conversation bodies. Abort on window close propagates to the upstream.
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	current := func() bool {
		latest, status := s.accountTestSource(target)
		return status == 0 && hmac.Equal([]byte(in.Revision), []byte(s.accountTestRevision(latest)))
	}
	dispatch := a
	dispatch.Kind, dispatch.Target, dispatch.Model = "codex", "codex", in.Model
	if dispatch.Protocol == "" {
		dispatch.Protocol = config.NativeProtocol(dispatch.Kind)
	}
	key := config.RandomToken(24)
	h := gateway.New(func() string { return key }, func(string) (config.LocalAccount, bool) { return dispatch, current() })
	h.ChatCompletions, h.Official = true, s.d.OfficialGateway
	// A diagnostic may intentionally try an unsupported/custom model. Its
	// failure must not cool the shared production credential or change routing
	// health for Codex, DeepSeek or phone workers. Admission below still uses
	// the common credential budget and respects existing production cooldowns.
	if s.accountTestHTTP != nil {
		h.Client = s.accountTestHTTP
	}
	defer h.Client.CloseIdleConnections()
	// Shared admission counts this test against the same credential budget as
	// normal API traffic, without allowing the pool to select a substitute.
	h.BeforeDispatch = func(ctx context.Context, _ config.LocalAccount, _ []byte) (func(), int) {
		if ctx.Err() != nil || !current() {
			return nil, 409
		}
		release, err := s.accountPool.AdmitFixedModel(dispatch, s.d.Store.Get().LocalAccounts, in.Model)
		if err != nil {
			return nil, 429
		}
		return release, 0
	}
	var result usageledger.Observation
	h.Observe = func(o usageledger.Observation) {
		o.Route = "account-test/chat"
		result = o
		s.observeUsage(o)
	}
	if s.usageLedger != nil {
		h.BeginUsage = s.usageLedger.BeginAccountUsage
	}
	// Use the common text contract. The installed cross-protocol adapter cannot
	// preserve Chat token-limit options, so injecting one would reject valid
	// Responses/Anthropic accounts before contacting them. Duration and response
	// size remain bounded by this endpoint and the browser.
	body, _ := json.Marshal(map[string]any{"model": in.Model, "messages": in.Messages, "stream": true})
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1/gateway/codex/v1/chat/completions", bytes.NewReader(body))
	request.RemoteAddr = "127.0.0.1:1"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+key)
	w.Header().Set("Cache-Control", "no-store")
	writer := &accountTestWriter{ResponseWriter: w, cancel: cancel}
	h.ServeHTTP(writer, request)
	if strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") && r.Context().Err() == nil {
		state := result.State
		if writer.limited {
			state = "incomplete"
		}
		// Only this final server event confirms completion; HTTP 200 or [DONE]
		// alone is not proof that the selected provider completed the response.
		data, _ := json.Marshal(map[string]any{"state": state, "httpStatus": result.HTTPStatus, "upstreamHttpStatus": result.UpstreamHTTPStatus, "durationMs": result.DurationMS, "firstByteMs": result.FirstByteMS, "tokens": result.Tokens, "failureStage": result.FailureStage, "limited": writer.limited})
		_, _ = w.Write(append(append([]byte("\nevent: diagnostic\ndata: "), data...), []byte("\n\n")...))
		_ = http.NewResponseController(w).Flush()
	}
}

type accountTestWriter struct {
	http.ResponseWriter
	cancel  context.CancelFunc
	written int
	limited bool
}

func (w *accountTestWriter) Write(data []byte) (int, error) {
	if w.written+len(data) > 1<<20 {
		w.limited = true
		w.cancel()
		return 0, io.ErrShortWrite
	}
	w.written += len(data)
	return w.ResponseWriter.Write(data)
}

func (w *accountTestWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }
