package console

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"salcara/bridge/internal/config"
)

var errCodexAPIRuntimeChanged = errors.New("Codex API 授权已变化")
var errCodexAPIClientExists = errors.New("API 客户端已存在")

func (s *Server) codexAPIRoutes(m *http.ServeMux) {
	s.apiNetworkRoutes(m)
	s.codexAPIReadinessRoutes(m)
	s.codexAPIImageRoutes(m)
	s.managedAPILaunchRoutes(m)
	s.codexAPIProfileRoutes(m)
	m.HandleFunc("GET /api/local/codex-api", s.handleCodexAPIStatus)
	m.HandleFunc("POST /api/local/codex-api/enabled", s.handleCodexAPIEnabled)
	m.HandleFunc("POST /api/local/codex-api/clients", s.handleCodexAPIClient)
	m.HandleFunc("POST /api/local/codex-api/clients/rotate", s.handleCodexAPIClientRotate)
	m.HandleFunc("POST /api/local/codex-api/clients/enabled", s.handleCodexAPIClientEnabled)
	m.HandleFunc("GET /api/local/codex-api/requests", s.handleCodexAPIRequests)
	m.HandleFunc("POST /api/local/codex-api/clients/revoke", s.handleCodexAPIRevoke)
	m.HandleFunc("POST /api/local/codex-api/members", s.handleCodexAPIMember)
}

// handleCodexAPIClientRotate is deliberately a narrow operation: a key reset
// must not replace current routing or permissions with a stale UI snapshot.
// Store.Update commits the new digest and authorization epoch atomically; an
// unsuccessful save leaves the old key and all client settings unchanged.
func (s *Server) handleCodexAPIClientRotate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if readReverseJSON(r, &in, 4096) != nil || !config.ValidAccountID(in.ID) {
		writeErr(w, http.StatusBadRequest, "请求格式有误")
		return
	}
	key := "sk-salcara-client-" + config.RandomToken(32)
	digest := config.APIClientDigest(key)
	errMissing := errors.New("client missing")
	err := s.d.Store.Update(func(c *config.Config) error {
		for i := range c.CodexAPI.Clients {
			if c.CodexAPI.Clients[i].ID == in.ID {
				// In particular, never re-enable a concurrently disabled client
				// or re-create one that has already been revoked.
				c.CodexAPI.Clients[i].KeyDigest = digest
				return nil
			}
		}
		return errMissing
	})
	if errors.Is(err, errMissing) {
		writeErr(w, http.StatusNotFound, "客户端已被移除，请刷新后重试")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "密钥保存失败，原密钥未更换")
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": in.ID, "key": key})
}
func (s *Server) handleCodexAPIStatus(w http.ResponseWriter, r *http.Request) {
	c := s.d.Store.Get()
	clients := []map[string]any{}
	for _, row := range c.CodexAPI.Clients {
		clients = append(clients, map[string]any{"id": row.ID, "revision": apiClientRevision(c, row), "name": row.Name, "accountId": row.AccountID, "accountIds": row.AccountIDs, "poolStrategy": row.PoolStrategy, "poolAccountId": row.PoolAccountID, "imageAccountIds": row.ImageAccountIDs, "disabled": row.Disabled, "modelAllowlist": row.ModelAllowlist, "modelDenylist": row.ModelDenylist, "modelAllowPrefixes": row.ModelAllowPrefixes, "modelDenyPrefixes": row.ModelDenyPrefixes, "modelAliases": row.ModelAliases, "modelPrefix": row.ModelPrefix, "maxConcurrency": row.MaxConcurrency})
	}
	writeJSON(w, map[string]any{"enabled": c.CodexAPI.Enabled, "running": c.CodexAPIActive(), "requiresStart": c.CodexAPI.Enabled && !c.CodexAPIActive(), "baseUrl": fmt.Sprintf("http://127.0.0.1:%d/codex-api/v1", s.d.Port), "clients": clients, "statistics": s.apiService.Snapshot(), "summary": s.readAPIServiceSummary(c)})
}
func (s *Server) handleCodexAPIEnabled(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if readReverseJSON(r, &in, 4096) != nil {
		writeErr(w, 400, "请求格式有误")
		return
	}
	// Serialise explicit starts/stops with profile transactions. Client/source
	// updates remain independently revocable and are fenced again before arm.
	if !s.codexProfileMu.TryLock() {
		writeErr(w, http.StatusConflict, "Codex 配置正在更新")
		return
	}
	defer s.codexProfileMu.Unlock()
	if in.Enabled && s.apiServiceClosed() {
		writeErr(w, http.StatusServiceUnavailable, "API 监听服务已关闭")
		return
	}
	if !in.Enabled {
		// Stop the captured runtime authorization before touching the config
		// file. A failed save must not roll a live service back into existence.
		before := s.d.Store.Get()
		retiredEpoch, ok := s.d.Store.RetireCodexAPIServiceRuntime(before.CodexAPI.AuthEpoch)
		if !ok {
			writeErr(w, http.StatusConflict, "API 服务授权已变化，请刷新后重试")
			return
		}
		err := s.d.Store.Update(func(c *config.Config) error {
			if c.CodexAPI.AuthEpoch != retiredEpoch || c.CodexAPI.Enabled || c.CodexAPI.RuntimeLease != "" {
				return errCodexAPIRuntimeChanged
			}
			// The runtime retirement already set the persisted fields. This
			// transaction only commits that inert state to disk.
			return nil
		})
		if errors.Is(err, errCodexAPIRuntimeChanged) {
			writeErr(w, http.StatusConflict, "API 服务已由较新的操作重新启动，请刷新后重试")
			return
		}
		profileErr := s.restoreStoppedCodexProfile(r.Context(), retiredEpoch)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "API 服务已在本次进程停止，但停用状态未保存；修复配置保存后再试")
			return
		}
		if profileErr != nil {
			writeErr(w, http.StatusConflict, "API 服务已停止；"+profileErr.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.CodexAPI.Enabled = true
		if strings.TrimSpace(c.GatewayRuntimeKey) != "" {
			// Arm only the current in-memory process lease. RuntimeLease is
			// excluded from JSON, so this explicit action is required again
			// after a restart or lease rotation.
			c.CodexAPI.RuntimeLease = c.GatewayRuntimeKey
		} else {
			c.CodexAPI.RuntimeLease = ""
		}
		return nil
	}); err != nil {
		writeErr(w, 500, "API 服务状态保存失败")
		return
	}
	if err := s.syncAPIListeners(); err != nil {
		current := s.d.Store.Get()
		_, _ = s.d.Store.RetireCodexAPIServiceRuntime(current.CodexAPI.AuthEpoch)
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
func (s *Server) handleCodexAPIClient(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID                 string            `json:"id,omitempty"`
		Name               string            `json:"name"`
		AccountID          string            `json:"accountId"`
		ImageAccountIDs    []string          `json:"imageAccountIds,omitempty"`
		AccountIDs         []string          `json:"accountIds,omitempty"`
		PoolStrategy       string            `json:"poolStrategy,omitempty"`
		PoolAccountID      string            `json:"poolAccountId,omitempty"`
		Disabled           bool              `json:"disabled,omitempty"`
		ModelAllowlist     []string          `json:"modelAllowlist,omitempty"`
		ModelDenylist      []string          `json:"modelDenylist,omitempty"`
		ModelAllowPrefixes []string          `json:"modelAllowPrefixes,omitempty"`
		ModelDenyPrefixes  []string          `json:"modelDenyPrefixes,omitempty"`
		ModelAliases       map[string]string `json:"modelAliases,omitempty"`
		ModelPrefix        string            `json:"modelPrefix,omitempty"`
		MaxConcurrency     int               `json:"maxConcurrency,omitempty"`
		Rotate             bool              `json:"rotate,omitempty"`
		OnlyIfEmpty        bool              `json:"onlyIfEmpty,omitempty"`
		SuppressKey        bool              `json:"suppressKey,omitempty"`
	}
	if readReverseJSON(r, &in, 256<<10) != nil || in.OnlyIfEmpty && (in.ID != "" || in.Rotate) || in.SuppressKey && !in.OnlyIfEmpty {
		writeErr(w, 400, "请求格式有误")
		return
	}
	key := ""
	id := in.ID
	if id == "" {
		id = config.NewUUID()
		key = "sk-salcara-client-" + config.RandomToken(32)
	} else if in.Rotate {
		key = "sk-salcara-client-" + config.RandomToken(32)
	}
	var saved config.NamedAPIClient
	if in.ID != "" && in.Disabled {
		before := s.d.Store.Get()
		old, exists := before.APIClient(id)
		if !exists {
			writeErr(w, http.StatusBadRequest, "客户端已被移除")
			return
		}
		if _, ok := before.LocalAccount(in.AccountID); !ok {
			writeErr(w, http.StatusBadRequest, "API 账号不存在")
			return
		}
		if err := validateClientImageMembers(before, in.ImageAccountIDs); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		if err := validateClientTextMembers(before, in.AccountIDs); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		saved = config.NamedAPIClient{ID: id, Name: in.Name, AccountID: in.AccountID, ImageAccountIDs: in.ImageAccountIDs, KeyDigest: old.KeyDigest, Disabled: true, ModelAllowlist: in.ModelAllowlist, ModelDenylist: in.ModelDenylist, ModelAllowPrefixes: in.ModelAllowPrefixes, ModelDenyPrefixes: in.ModelDenyPrefixes, ModelAliases: in.ModelAliases, ModelPrefix: in.ModelPrefix, MaxConcurrency: in.MaxConcurrency}
		saved.AccountIDs, saved.PoolStrategy, saved.PoolAccountID = in.AccountIDs, in.PoolStrategy, in.PoolAccountID
		if key != "" {
			saved.KeyDigest = config.APIClientDigest(key)
		}
		if err := config.ValidateAPIClient(saved); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		// Disable the exact client snapshot first. If the file write below
		// fails, Store.Update rolls back only to this disabled snapshot, so
		// the old key stays inert for the remainder of the process.
		retired, ok := s.d.Store.RetireCodexAPIClientRuntime(old)
		if !ok {
			writeErr(w, http.StatusConflict, "客户端授权已变化，请刷新后重试")
			return
		}
		err := s.d.Store.Update(func(c *config.Config) error {
			current, exists := c.APIClient(id)
			if !exists || !reflect.DeepEqual(current, retired) {
				return errCodexAPIRuntimeChanged
			}
			if _, ok := c.LocalAccount(in.AccountID); !ok {
				return errCodexAPIRuntimeChanged
			}
			if err := validateClientImageMembers(*c, in.ImageAccountIDs); err != nil {
				return errCodexAPIRuntimeChanged
			}
			if err := validateClientTextMembers(*c, in.AccountIDs); err != nil {
				return errCodexAPIRuntimeChanged
			}
			for i := range c.CodexAPI.Clients {
				if c.CodexAPI.Clients[i].ID == id {
					c.CodexAPI.Clients[i] = saved
					break
				}
			}
			return nil
		})
		if errors.Is(err, errCodexAPIRuntimeChanged) {
			writeErr(w, http.StatusConflict, "客户端已由较新的操作更新，请刷新后重试")
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "客户端已在本次进程停用，但配置更改未保存；修复配置保存后再试")
			return
		}
		writeJSON(w, map[string]any{"ok": true, "id": saved.ID, "key": key})
		return
	}
	err := s.d.Store.Update(func(c *config.Config) error {
		// The simple first-use flow must not create duplicate defaults when
		// two local console windows submit before either receives a response.
		if in.OnlyIfEmpty && len(c.CodexAPI.Clients) != 0 {
			return errCodexAPIClientExists
		}
		old, exists := c.APIClient(id)
		if in.ID != "" && !exists {
			return errors.New("客户端已被移除")
		}
		if !exists && len(c.CodexAPI.Clients) >= 128 {
			return errors.New("API 客户端过多")
		}
		if _, ok := c.LocalAccount(in.AccountID); !ok {
			return errors.New("API 账号不存在")
		}
		if err := validateClientImageMembers(*c, in.ImageAccountIDs); err != nil {
			return err
		}
		if err := validateClientTextMembers(*c, in.AccountIDs); err != nil {
			return err
		}
		saved = config.NamedAPIClient{ID: id, Name: in.Name, AccountID: in.AccountID, ImageAccountIDs: in.ImageAccountIDs, KeyDigest: old.KeyDigest, Disabled: in.Disabled, ModelAllowlist: in.ModelAllowlist, ModelDenylist: in.ModelDenylist, ModelAllowPrefixes: in.ModelAllowPrefixes, ModelDenyPrefixes: in.ModelDenyPrefixes, ModelAliases: in.ModelAliases, ModelPrefix: in.ModelPrefix, MaxConcurrency: in.MaxConcurrency}
		saved.AccountIDs, saved.PoolStrategy, saved.PoolAccountID = in.AccountIDs, in.PoolStrategy, in.PoolAccountID
		if key != "" {
			saved.KeyDigest = config.APIClientDigest(key)
		}
		if err := config.ValidateAPIClient(saved); err != nil {
			return err
		}
		if !exists {
			c.CodexAPI.Clients = append(c.CodexAPI.Clients, saved)
		} else {
			for i := range c.CodexAPI.Clients {
				if c.CodexAPI.Clients[i].ID == id {
					c.CodexAPI.Clients[i] = saved
					break
				}
			}
		}
		return nil
	})
	if errors.Is(err, errCodexAPIClientExists) {
		writeErr(w, http.StatusConflict, "已生成密钥，请刷新后管理现有密钥")
		return
	}
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// Only creation/rotation returns the new random key, and only to the
	// authenticated local console. No digest or upstream credentials exposed.
	// The simple managed-launch setup has no manual-key step. Keep its random
	// digest for compatibility, but never send that unused secret to the UI.
	if in.SuppressKey {
		writeJSON(w, map[string]any{"ok": true, "id": saved.ID})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": saved.ID, "key": key})
}
func validateClientImageMembers(c config.Config, ids []string) error {
	for _, id := range ids {
		if _, ok := c.LocalAccount(id); !ok {
			return errors.New("图片池账号不存在，请刷新账号列表")
		}
	}
	return nil
}
func validateClientTextMembers(c config.Config, ids []string) error {
	for _, id := range ids {
		if _, ok := c.LocalAccount(id); !ok {
			return errors.New("文字池账号不存在，请刷新账号列表")
		}
	}
	return nil
}

var errCodexAPIMemberChanged = errors.New("API 服务成员已变化")

// handleCodexAPIMember performs a small, compare-and-swap style membership
// edit for an account card. It never changes the client's primary AccountID;
// moving a primary account out requires an explicit client edit so a card
// action cannot silently strand a key. mode may be text, image, or both.
func (s *Server) handleCodexAPIMember(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ClientID               string  `json:"clientId"`
		AccountID              string  `json:"accountId"`
		Mode                   string  `json:"mode"`
		Enabled                bool    `json:"enabled"`
		ExpectedSourceRevision *string `json:"expectedSourceRevision,omitempty"`
		ExpectedClientRevision *string `json:"expectedClientRevision,omitempty"`
	}
	if readReverseJSON(r, &in, 4096) != nil || !config.ValidAccountID(in.ClientID) || !config.ValidAccountID(in.AccountID) {
		writeErr(w, 400, "请求格式有误")
		return
	}
	if in.Mode != "text" && in.Mode != "image" && in.Mode != "both" {
		writeErr(w, 400, "成员类型无效")
		return
	}
	before := s.d.Store.Get()
	client, exists := before.APIClient(in.ClientID)
	if !exists {
		if in.ExpectedSourceRevision != nil || in.ExpectedClientRevision != nil {
			writeErr(w, http.StatusConflict, "API 服务成员已变化，请重新打开")
			return
		}
		writeErr(w, http.StatusNotFound, "API 客户端不存在")
		return
	}
	account, exists := before.LocalAccount(in.AccountID)
	if !exists {
		if in.ExpectedSourceRevision != nil || in.ExpectedClientRevision != nil {
			writeErr(w, http.StatusConflict, "API 服务成员已变化，请重新打开")
			return
		}
		writeErr(w, http.StatusBadRequest, "API 账号不存在，请刷新账号列表")
		return
	}
	if checkAPIMemberRevisions(before, account, client, in.ExpectedSourceRevision, in.ExpectedClientRevision) != nil {
		writeErr(w, http.StatusConflict, "API 服务成员已变化，请重新打开")
		return
	}
	if !in.Enabled && client.AccountID == in.AccountID {
		writeErr(w, http.StatusConflict, "这是客户端主账号，请先在客户端配置中更换主账号")
		return
	}
	// Empty pools execute against the primary account. A small card toggle
	// must not silently replace that grant, or restore it by removing the last
	// explicit member. These transitions require the full client editor.
	if (in.Mode == "text" || in.Mode == "both") &&
		(apiMemberToggleNeedsEditor(client.AccountIDs, client.AccountID, in.AccountID, in.Enabled) ||
			!in.Enabled && client.PoolStrategy == config.PoolStrategySingleAccount && client.PoolAccountID == in.AccountID) ||
		(in.Mode == "image" || in.Mode == "both") && apiMemberToggleNeedsEditor(client.ImageAccountIDs, client.AccountID, in.AccountID, in.Enabled) {
		writeErr(w, http.StatusConflict, "此操作会改变主账号回退或固定账号，请在客户端设置中确认成员")
		return
	}
	next := client
	if in.Mode == "text" || in.Mode == "both" {
		next.AccountIDs = toggleMember(next.AccountIDs, in.AccountID, in.Enabled)
	}
	if in.Mode == "image" || in.Mode == "both" {
		next.ImageAccountIDs = toggleMember(next.ImageAccountIDs, in.AccountID, in.Enabled)
	}
	if err := config.ValidateAPIClient(next); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	committed, err := s.d.Store.UpdateWithSnapshot(func(c *config.Config) error {
		current, ok := c.APIClient(in.ClientID)
		if !ok || current.AuthEpoch != client.AuthEpoch || !reflect.DeepEqual(current, client) {
			return errCodexAPIMemberChanged
		}
		currentAccount, ok := c.LocalAccount(in.AccountID)
		if !ok || checkAPIMemberRevisions(*c, currentAccount, current, in.ExpectedSourceRevision, in.ExpectedClientRevision) != nil {
			return errCodexAPIMemberChanged
		}
		for i := range c.CodexAPI.Clients {
			if c.CodexAPI.Clients[i].ID == in.ClientID {
				c.CodexAPI.Clients[i] = next
				return nil
			}
		}
		return errCodexAPIMemberChanged
	})
	if errors.Is(err, errCodexAPIMemberChanged) {
		writeErr(w, http.StatusConflict, "API 服务成员已由较新的操作更新，请刷新后重试")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "API 服务成员保存失败")
		return
	}
	saved, _ := committed.APIClient(next.ID)
	writeJSON(w, map[string]any{"ok": true, "clientId": next.ID, "accountId": in.AccountID, "enabled": in.Enabled, "mode": in.Mode, "clientRevision": apiClientRevision(committed, saved)})
}

func apiMemberToggleNeedsEditor(values []string, primary, id string, enabled bool) bool {
	return enabled && len(values) == 0 && id != primary ||
		!enabled && len(values) == 1 && values[0] == id
}

func toggleMember(values []string, id string, enabled bool) []string {
	out := make([]string, 0, len(values)+1)
	seen := false
	for _, value := range values {
		if value == id {
			if enabled && !seen {
				out = append(out, value)
				seen = true
			}
			continue
		}
		out = append(out, value)
	}
	if enabled && !seen {
		out = append(out, id)
	}
	return out
}
func (s *Server) handleCodexAPIRevoke(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if readReverseJSON(r, &in, 4096) != nil || !config.ValidAccountID(in.ID) {
		writeErr(w, 400, "请求格式有误")
		return
	}
	id := in.ID
	before := s.d.Store.Get()
	expected, exists := before.APIClient(id)
	if !exists {
		writeErr(w, http.StatusBadRequest, "客户端已被移除")
		return
	}
	retired, ok := s.d.Store.RetireCodexAPIClientRuntime(expected)
	if !ok {
		writeErr(w, http.StatusConflict, "客户端授权已变化，请刷新后重试")
		return
	}
	err := s.d.Store.Update(func(c *config.Config) error {
		current, exists := c.APIClient(id)
		if !exists || !reflect.DeepEqual(current, retired) {
			return errCodexAPIRuntimeChanged
		}
		out := c.CodexAPI.Clients[:0]
		for _, row := range c.CodexAPI.Clients {
			if row.ID != id {
				out = append(out, row)
			}
		}
		c.CodexAPI.Clients = out
		return nil
	})
	if errors.Is(err, errCodexAPIRuntimeChanged) {
		writeErr(w, http.StatusConflict, "客户端已由较新的操作更新，请刷新后重试")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "客户端已在本次进程撤销，但移除状态未保存；修复配置保存后再试")
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
