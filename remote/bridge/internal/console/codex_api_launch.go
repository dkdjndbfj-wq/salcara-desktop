package console

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/launcher"
)

type managedLaunchPreview struct {
	ClientID, Target, StateRevision string
	Models                          []string
	Expires                         time.Time
}
type managedLaunchRequest struct {
	ClientID        string `json:"clientId"`
	Target          string `json:"target"`
	Revision        string `json:"revision"`
	Confirmed       bool   `json:"confirmed"`
	CatalogOverride bool   `json:"catalogOverride"`
	Workspace       string `json:"workspace,omitempty"`
	AllowModeChange bool   `json:"allowModeChange,omitempty"`
}

func (s *Server) managedAPILaunchRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /api/local/codex-api/launch/preview", s.handleManagedAPILaunchPreview)
	m.HandleFunc("POST /api/local/codex-api/launch", launchProgressEndpoint(s.handleManagedAPILaunch))
}

func managedClientMembers(client config.NamedAPIClient) []string {
	if len(client.AccountIDs) > 0 {
		return append([]string(nil), client.AccountIDs...)
	}
	return []string{client.AccountID}
}

func (s *Server) managedLaunchState(c config.Config, id, target string) (config.NamedAPIClient, string, error) {
	bad := errors.New("本地 API 或账号配置已改变，请重新打开")
	client, ok := c.APIClient(id)
	if !ok || client.Disabled || config.ValidateAPIClient(client) != nil || !launcher.ValidTarget(target) || !c.CodexAPIActive() || c.GatewayRuntimeKey == "" || c.CodexAPI.ImagesMode == "images_only" {
		return client, "", bad
	}
	members := []config.LocalAccount{}
	for _, member := range managedClientMembers(client) {
		a, exists := c.LocalAccount(member)
		if !exists || config.ValidateLocalAccount(&a) != nil || s.managedAPILoop(c, a.BaseURL) {
			return client, "", bad
		}
		members = append(members, a)
	}
	if s.apiClientReadiness(c, client).State != "configured" {
		return client, "", errors.New("客户端尚无就绪账号，请先检查账号状态")
	}
	selected, _ := c.SelectedToolAccount(target)
	binding := c.ToolAPIApplied[config.ToolFamily(target)]
	data, _ := json.Marshal(struct {
		Client                                                                    config.NamedAPIClient
		Members                                                                   []config.LocalAccount
		ClientEpoch, ServiceEpoch, Target, Selected, Model, Protocol, Path, Lease string
		Catalog                                                                   bool
		Binding                                                                   config.AppliedAPI
	}{
		client, members, client.AuthEpoch, c.CodexAPI.AuthEpoch, target, selected.ID, c.ToolModels[target], c.ToolProtocols[target], c.LocalToolPaths[target], binding.LaunchLease, c.ToolCatalogOverride[target], binding})
	mac := hmac.New(sha256.New, []byte(c.GatewayRuntimeKey))
	mac.Write([]byte("salcara-managed-preview-state-v1\x00"))
	mac.Write(data)
	return client, hex.EncodeToString(mac.Sum(nil)), nil
}

// Never route a managed pool back into this Bridge's own service or gateway.
// The managed profile itself is not added as an upstream account.
func (s *Server) managedAPILoop(c config.Config, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return true
	}
	host := strings.ToLower(u.Hostname())
	ip := net.ParseIP(host)
	local := host == "localhost" || ip != nil && ip.IsLoopback() || host == strings.ToLower(c.CodexAPI.Network.ClientHost)
	if !local {
		return false
	}
	port, _ := strconv.Atoi(u.Port())
	if port == s.d.Port {
		return true
	}
	for _, p := range c.CodexAPI.Network.Ports {
		if port == p {
			return true
		}
	}
	return false
}

func (s *Server) managedLaunchModels(ctx context.Context, c config.Config, client config.NamedAPIClient, target string) (config.Config, []string, error) {
	seen := map[string]bool{}
	for _, id := range managedClientMembers(client) {
		if client.PoolStrategy == config.PoolStrategySingleAccount && len(client.AccountIDs) > 0 && id != client.PoolAccountID {
			continue
		}
		a, _ := c.LocalAccount(id)
		_, revision, err := s.managedLaunchState(c, client.ID, target)
		if err != nil || ctx.Err() != nil {
			return config.Config{}, nil, errors.New("读取期间配置已改变，请重新打开")
		}
		guard := func(now config.Config) error {
			_, current, err := s.managedLaunchState(now, client.ID, target)
			if err != nil || current != revision || ctx.Err() != nil {
				return errors.New("读取期间配置已改变，请重新打开")
			}
			return nil
		}
		var models []string
		if a.CredentialRef != "" || a.SourceType == "official_account" {
			if s.apiService.ModelCatalog == nil {
				return config.Config{}, nil, errors.New("暂时无法读取账号模型目录")
			}
			catalog, err := s.apiService.ModelCatalog(ctx, a, true)
			if err != nil {
				return config.Config{}, nil, fmt.Errorf("读取账号模型目录失败：%w", err)
			}
			// The native reader may publish only these picker hints. It cannot
			// change a client epoch, member, selected API or tool profile here.
			expected := c.Clone()
			for i := range expected.LocalAccounts {
				if expected.LocalAccounts[i].ID == id {
					expected.LocalAccounts[i].Models = append([]string(nil), catalog.Models...)
				}
			}
			_, expectedRevision, expectedErr := s.managedLaunchState(expected, client.ID, target)
			latest := s.d.Store.Get()
			_, latestRevision, latestErr := s.managedLaunchState(latest, client.ID, target)
			if expectedErr != nil || latestErr != nil || ctx.Err() != nil || latestRevision != expectedRevision && latestRevision != revision {
				return config.Config{}, nil, errors.New("读取期间配置已改变，请重新打开")
			}
			c = latest
			models = catalog.Models
		} else {
			var refreshed config.Config
			a, refreshed, err = s.refreshLaunchCatalogChecked(ctx, a, guard)
			if err != nil {
				return config.Config{}, nil, err
			}
			c = refreshed
			models = append(append([]string(nil), a.Models...), a.Model)
		}
		client, _ = c.APIClient(client.ID)
		for _, model := range models {
			if !config.ValidCatalogModel(model) || !config.ModelAllowed(a, model) || !config.APIClientModelAllowed(client, model) {
				continue
			}
			if _, shadowed := client.ModelAliases[model]; !shadowed {
				if visible, ok := config.APIClientVisibleModel(client, model); ok {
					seen[visible] = true
				}
			}
		}
		for alias, actual := range client.ModelAliases {
			if config.ModelAllowed(a, actual) && config.APIClientModelAllowed(client, actual) {
				if visible, ok := config.APIClientVisibleModel(client, alias); ok {
					seen[visible] = true
				}
			}
		}
	}
	if len(seen) == 0 || len(seen) > 1000 {
		return config.Config{}, nil, errors.New("没有可用的模型目录，请先刷新 API 模型")
	}
	out := make([]string, 0, len(seen))
	for model := range seen {
		out = append(out, model)
	}
	sort.Strings(out)
	return c, out, nil
}

func (s *Server) handleManagedAPILaunchPreview(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ClientID string `json:"clientId"`
		Target   string `json:"target"`
	}
	if readReverseJSON(r, &in, 4096) != nil || !config.ValidAccountID(in.ClientID) || !launcher.ValidTarget(in.Target) {
		writeErr(w, 400, "请求格式有误")
		return
	}
	if !s.managedLaunchMu.TryLock() {
		writeErr(w, 409, "正在准备打开工具，请稍后重试")
		return
	}
	defer s.managedLaunchMu.Unlock()
	c := s.d.Store.Get()
	client, state, err := s.managedLaunchState(c, in.ClientID, in.Target)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	c, models, err := s.managedLaunchModels(ctx, c, client, in.Target)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	client, state, err = s.managedLaunchState(c, in.ClientID, in.Target)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	modeChange := false
	if in.Target == "claude-desktop" {
		status := s.d.ClaudeDesktop.Status(ctx, c.LocalToolPaths)
		if !status.Available || !status.Supported || status.Managed {
			writeErr(w, 409, "Claude Desktop 尚不支持本机配置，请检查安装与组织策略")
			return
		}
		modeChange = status.RequiresModeChange
	} else {
		available := false
		for _, tool := range s.d.Local.Tools(ctx, c.LocalToolPaths) {
			if tool.ID == in.Target && tool.Available {
				available = true
			}
		}
		if !available {
			writeErr(w, 409, "未找到所选工具，请先安装或设置程序路径")
			return
		}
	}
	_, latest, err := s.managedLaunchState(s.d.Store.Get(), in.ClientID, in.Target)
	if err != nil || latest != state || ctx.Err() != nil {
		writeErr(w, 409, "读取期间配置已改变，请重新打开")
		return
	}
	if s.managedLaunchPreviews == nil {
		s.managedLaunchPreviews = map[string]managedLaunchPreview{}
	}
	now := time.Now()
	for key, p := range s.managedLaunchPreviews {
		if !p.Expires.After(now) || p.Target == in.Target {
			delete(s.managedLaunchPreviews, key)
		}
	}
	if len(s.managedLaunchPreviews) >= 8 {
		writeErr(w, 409, "打开请求过多，请稍后重试")
		return
	}
	revision := "mla-v1-" + config.RandomToken(32)
	s.managedLaunchPreviews[revision] = managedLaunchPreview{in.ClientID, in.Target, state, models, now.Add(2 * time.Minute)}
	writeJSON(w, map[string]any{"clientId": in.ClientID, "target": in.Target, "revision": revision, "clientName": client.Name, "models": models, "catalogOverride": true, "requiresModeChange": modeChange, "ready": true})
}

func (s *Server) handleManagedAPILaunch(w http.ResponseWriter, r *http.Request) {
	var in managedLaunchRequest
	if readReverseJSON(r, &in, 8192) != nil || !in.Confirmed || !launcher.ValidTarget(in.Target) || !config.ValidAccountID(in.ClientID) || len(in.Revision) != 71 {
		writeErr(w, 400, "请确认任务已结束后再打开")
		return
	}
	launcher.LaunchPlan(r.Context(), in.Target, true, false)
	if !s.managedLaunchMu.TryLock() {
		writeErr(w, 409, "正在打开工具，请勿重复提交")
		return
	}
	defer s.managedLaunchMu.Unlock()
	p, ok := s.managedLaunchPreviews[in.Revision]
	if !ok || p.ClientID != in.ClientID || p.Target != in.Target || !p.Expires.After(time.Now()) {
		writeErr(w, 409, "打开确认已失效，请重新打开")
		return
	}
	// One confirmation is consumed before any side effect; an unknown response
	// must not restart the native application a second time.
	delete(s.managedLaunchPreviews, in.Revision)
	c := s.d.Store.Get()
	client, state, err := s.managedLaunchState(c, in.ClientID, in.Target)
	if err != nil || state != p.StateRevision {
		writeErr(w, 409, "账号或服务已改变，请重新打开")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 55*time.Second)
	defer cancel()
	launcher.LaunchPhase(ctx, "remote", "检查远程任务")
	if s.d.Hub != nil {
		release, err := s.d.Hub.BeginLocalToolChange(ctx)
		if err != nil {
			writeErr(w, 409, err.Error())
			return
		}
		defer release()
	} else if m := s.manager(); m != nil {
		for _, agent := range m.Agents() {
			active, known := agent.(interface{ ActiveRemoteTurns() bool })
			if !known || active.ActiveRemoteTurns() {
				writeErr(w, 409, "远程任务尚未结束，请稍后重试")
				return
			}
		}
	}
	launcher.LaunchPhase(ctx, "service", "检查本机 API 服务")
	release, err := s.apiService.BeginManagedLaunch()
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	defer release()
	lease := config.RandomToken(32)
	prospective := c.Clone()
	prospective.RecordManagedAPI(in.Target, client, lease, in.CatalogOverride, p.Models)
	key := prospective.ManagedAPICredential(in.Target)
	if key == "" {
		writeErr(w, 409, "本机授权尚未就绪")
		return
	}
	connection := config.LocalAccount{ID: "managed-local-client", Name: "本地 API · " + client.Name, Kind: strings.Split(in.Target, "-")[0], SourceType: "api_key", BaseURL: fmt.Sprintf("http://127.0.0.1:%d/codex-api/managed/%s", s.d.Port, in.Target), Key: key, AuthMode: "bearer", Models: append([]string(nil), p.Models...), Model: p.Models[0], Wire: config.NativeProtocol(strings.Split(in.Target, "-")[0]), CatalogOverride: in.CatalogOverride}
	connection.Protocol = connection.Wire
	authorize := func() error {
		err := s.d.Store.Update(func(now *config.Config) error {
			current, revision, err := s.managedLaunchState(*now, in.ClientID, in.Target)
			if err != nil || revision != state || ctx.Err() != nil {
				return errors.New("本机启动来源已改变")
			}
			now.RecordManagedAPI(in.Target, current, lease, in.CatalogOverride, p.Models)
			return nil
		})
		if err == nil {
			s.apiService.AdmitManagedLaunch(key)
		}
		return err
	}
	var result launcher.Result
	if in.Target == "claude-desktop" {
		connection.Models, connection.DesktopModelLabels = config.ClaudeDesktopCatalog(connection)
		connection.Model = ""
		result, err = s.d.ClaudeDesktop.SwitchAuthorized(ctx, connection, c.LocalToolPaths, in.AllowModeChange, authorize)
	} else {
		result, err = s.d.Local.SwitchAuthorized(ctx, connection, in.Target, in.Workspace, c.LocalToolPaths, authorize)
	}
	if err != nil {
		s.d.Store.RevokeAppliedLaunchLease(in.Target, lease)
		writeErr(w, 409, err.Error())
		return
	}
	launcher.LaunchPhase(ctx, "record", "保存本次启动状态")
	if s.d.Store.Get().ManagedAPICredential(in.Target) != key {
		s.d.Store.RevokeAppliedLaunchLease(in.Target, lease)
		w.WriteHeader(http.StatusConflict)
		writeJSON(w, map[string]any{"error": "工具已启动，但本机连接授权已改变", "started": true, "result": result})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "result": result})
}
