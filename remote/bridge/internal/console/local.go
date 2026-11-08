package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/gateway"
	"salcara/bridge/internal/launcher"
	"salcara/bridge/internal/toolcfg"
)

func (s *Server) localRoutes(m *http.ServeMux) {
	s.officialAccountRoutes(m)
	s.codexAPIRoutes(m)
	s.claudeDesktop3PRoutes(m)
	m.HandleFunc("GET /api/local/accounts", s.handleLocalAccounts)
	m.HandleFunc("POST /api/local/accounts", s.handleLocalSave)
	m.HandleFunc("POST /api/local/accounts/delete", s.handleLocalDelete)
	m.HandleFunc("POST /api/local/accounts/select", s.handleLocalSelect)
	m.HandleFunc("POST /api/local/accounts/pool", s.handleLocalPool)
	m.HandleFunc("POST /api/local/accounts/model-rules", s.handleLocalModelRules)
	m.HandleFunc("POST /api/local/accounts/speed", s.handleLocalAccountSpeed)
	m.HandleFunc("POST /api/local/accounts/tags", s.handleAccountTags)
	m.HandleFunc("POST /api/local/accounts/test/prepare", s.handleAccountTestPrepare)
	m.HandleFunc("POST /api/local/accounts/test/send", s.handleAccountTestSend)
	m.HandleFunc("POST /api/local/accounts/daily-refresh", s.handleAccountDailyRefresh)
	m.HandleFunc("POST /api/local/accounts/daily-usage", s.handleAccountDailyUsage)
	m.HandleFunc("POST /api/local/accounts/remote-support", s.handleAccountRemoteSupport)
	m.HandleFunc("POST /api/local/models", s.handleLocalModels)
	m.HandleFunc("POST /api/local/launch", launchProgressEndpoint(s.handleLocalLaunch))
	m.HandleFunc("POST /api/local/switch/preview", s.handleLocalSwitchPreview)
	m.HandleFunc("POST /api/local/switch", launchProgressEndpoint(s.handleLocalSwitch))
	m.HandleFunc("POST /api/local/default", s.handleLocalDefault)
	m.HandleFunc("POST /api/local/tools", s.handleLocalToolPaths)
	m.HandleFunc("POST /api/local/bind", s.handleLocalBind)
	m.HandleFunc("POST /api/local/resume", launchProgressEndpoint(s.handleLocalResume))
	m.HandleFunc("GET /api/local/desktop-companion", s.handleDesktopCompanion)
	m.HandleFunc("POST /api/local/desktop-companion/install", s.handleInstallDesktopCompanion)
	m.HandleFunc("GET /api/local/desktop-companion/uninstall", s.handlePreviewUninstallDesktopCompanion)
	m.HandleFunc("POST /api/local/desktop-companion/uninstall", s.handleUninstallDesktopCompanion)
	m.HandleFunc("GET /api/local/desktop-companion/control", s.handleDesktopControl)
	m.HandleFunc("POST /api/local/desktop-companion/disconnect", s.handleDisconnectDesktop)
}

type localSwitchRequest struct {
	ID                     string  `json:"id"`
	Target                 string  `json:"target"`
	Workspace              string  `json:"workspace"`
	Confirmed              bool    `json:"confirmed"`
	KeepModel              bool    `json:"keepModel"`
	ApplyOnly              bool    `json:"applyOnly,omitempty"`
	ExpectedSourceRevision *string `json:"expectedSourceRevision,omitempty"`
}

func (s *Server) handleLocalSwitchPreview(w http.ResponseWriter, r *http.Request) {
	var in localSwitchRequest
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	c := s.d.Store.Get()
	a, ok := c.LocalAccount(in.ID)
	if !ok {
		writeErr(w, 404, "请先添加一个 API 账号")
		return
	}
	ctx, cancel := ctxTimeout(r, 20*time.Second)
	defer cancel()
	a = c.APIForTool(a, in.Target)
	if in.KeepModel {
		var err error
		a, err = launcher.PreserveCurrentToolModel(a, in.Target)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	a.GatewayLaunchLease = config.RandomToken(32) // preview never publishes this lease
	info, err := s.d.Local.PreviewSwitch(ctx, c.ToolConnection(a, in.Target, s.d.Port), in.Target, in.Workspace, c.LocalToolPaths)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "info": info})
}

func (s *Server) handleLocalSwitch(w http.ResponseWriter, r *http.Request) {
	if !s.admitUpdateWork(w) {
		return
	}
	var in localSwitchRequest
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if in.ApplyOnly {
		launcher.LaunchApplyPlan(r.Context())
	} else {
		launcher.LaunchPlan(r.Context(), in.Target, false, false)
	}
	if !in.Confirmed {
		writeErr(w, 400, "请先确认任务已结束、内容已保存，再切换原工具")
		return
	}
	c := s.d.Store.Get()
	a, ok := c.LocalAccount(in.ID)
	if !ok {
		writeErr(w, 404, "账号不存在")
		return
	}
	if err := checkLocalSourceRevision(c, a, in.ExpectedSourceRevision); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	ctx, cancel := ctxTimeout(r, 55*time.Second)
	defer cancel()
	release, err := s.beginLocalToolChange(ctx, in.Target)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	defer release()
	launcher.LaunchPhase(ctx, "catalog", "读取可用模型")
	var catalogErr error
	a, catalogErr = s.refreshLaunchCatalog(ctx, a)
	if catalogErr != nil {
		writeErr(w, http.StatusConflict, catalogErr.Error())
		return
	}
	launcher.LaunchPhase(ctx, "selection", "读取当前工具配置")
	vaultAccount := a
	refreshedConfig := s.d.Store.Get()
	if !unchangedSelectionAfterCatalog(refreshedConfig, c, a, in.Target) {
		writeErr(w, http.StatusConflict, "读取模型期间工具选择已改变，请重新打开")
		return
	}
	// The catalog transaction may clear a no-longer-available saved model.
	// Preserve its internally verified result without accepting later edits.
	authorizationSourceRevision := c.AccountSourceRevision(vaultAccount)
	if in.ExpectedSourceRevision != nil {
		current, exists := refreshedConfig.LocalAccount(a.ID)
		if !exists || checkLocalSourceRevision(refreshedConfig, current, &authorizationSourceRevision) != nil {
			writeErr(w, http.StatusConflict, errLocalSourceChanged.Error())
			return
		}
	}
	c = refreshedConfig
	a = c.APIForTool(a, in.Target)
	if in.KeepModel {
		var err error
		a, err = launcher.PreserveCurrentToolModel(a, in.Target)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	a = launchCatalogDefault(a)
	a.GatewayLaunchLease = config.RandomToken(32)
	processKey := c.GatewayRuntimeKey
	launchConfig := c
	authorize := func() error {
		if s.d.Hub != nil {
			if err := s.d.Hub.CheckUpdateAdmission(); err != nil {
				return err
			}
		}
		return s.d.Store.Update(func(c *config.Config) error {
			current, exists := c.LocalAccount(a.ID)
			if in.ExpectedSourceRevision != nil && (!exists || checkLocalSourceRevision(*c, current, &authorizationSourceRevision) != nil) {
				return errLocalSourceChanged
			}
			if !exists || !sameLaunchAccount(current, vaultAccount) || c.GatewayRuntimeKey != processKey || !sameLaunchSelection(*c, launchConfig, in.Target) {
				return errors.New("启动期间账号已变更，请重新应用")
			}
			provider := ""
			if a.Kind == "codex" {
				b, _ := os.ReadFile(toolcfg.CodexConfigPath())
				provider, _, _, _ = toolcfg.InspectCodexTOML(string(b))
				if provider == "" {
					provider = "openai"
				}
			}
			if in.KeepModel {
				if c.ToolModels == nil {
					c.ToolModels = map[string]string{}
				}
				if c.ToolProtocols == nil {
					c.ToolProtocols = map[string]string{}
				}
				c.ToolModels[in.Target], c.ToolProtocols[in.Target] = a.Model, a.Protocol
			}
			c.RecordAppliedAPI(in.Target, a, provider)
			return nil
		})
	}
	configure := s.d.Local.SwitchAuthorized
	if in.ApplyOnly {
		configure = s.d.Local.ApplyAuthorized
	}
	result, err := configure(ctx, c.ToolConnection(a, in.Target, s.d.Port), in.Target, in.Workspace, c.LocalToolPaths, authorize)
	if err != nil {
		s.d.Store.RevokeAppliedLaunchLease(in.Target, a.GatewayLaunchLease)
		if errors.Is(err, errLocalSourceChanged) {
			writeErr(w, http.StatusConflict, errLocalSourceChanged.Error())
			return
		}
		writeErr(w, 400, err.Error())
		return
	}
	launcher.LaunchPhase(ctx, "record", "保存本次启动状态")
	err = s.d.Store.Update(func(c *config.Config) error {
		current, exists := c.LocalAccount(a.ID)
		binding, applied := c.AppliedToolBinding(in.Target)
		if !exists || !sameLaunchAccount(current, vaultAccount) || !applied || binding.LaunchLease != a.GatewayLaunchLease || c.GatewayRuntimeKey != processKey {
			return errors.New("启动期间账号或授权已变更，请重新应用")
		}
		if a.Kind == "codex" {
			c.ActiveCodexAccount = a.ID
		} else if a.Kind == "claude" {
			c.ActiveClaudeAccount = a.ID
		}
		for i, x := range c.LocalAccounts {
			if x.ID == a.ID {
				c.LocalAccounts[i].LastUsedAt = time.Now().UnixMilli()
				c.LocalAccounts[i].Target = in.Target
			}
		}
		return nil
	})
	if err != nil {
		s.d.Store.RevokeAppliedLaunchLease(in.Target, a.GatewayLaunchLease)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		message := "工具已启动，本次启动状态保存失败："
		if in.ApplyOnly {
			message = "配置已写入，本次应用状态保存失败："
		}
		writeJSON(w, map[string]any{"error": message + err.Error(), "started": !in.ApplyOnly, "result": result})
		return
	}
	if !in.ApplyOnly && in.Target == "codex-desktop" {
		if marker, ok := s.d.Companion.(interface{ MarkManagedLaunch() }); ok {
			marker.MarkManagedLaunch()
		}
	}
	s.log.Printf("local switch: target=%s account=%s pid=%d", in.Target, a.ID, result.PID)
	writeJSON(w, map[string]any{"ok": true, "result": result})
}

func maskedAccount(a config.LocalAccount) map[string]any {
	sourceType := a.SourceType
	if sourceType == "" {
		sourceType = "api_key"
	}
	return map[string]any{"id": a.ID, "name": a.Name, "kind": a.Kind, "sourceType": sourceType, "providerId": a.ProviderID, "providerName": a.ProviderName, "poolGroup": a.PoolGroup, "poolEnabled": a.PoolEnabled, "poolStrategy": a.PoolStrategy, "poolBackup": a.PoolBackup, "priority": a.Priority, "weight": a.Weight, "maxConcurrency": a.MaxConcurrency, "quotaReservePercent": a.QuotaReservePercent, "baseUrl": a.BaseURL, "keyMasked": mask(a.Key), "hasKey": a.Key != "", "model": a.Model, "models": a.Models, "modelsManual": a.ModelsManual, "modelAllowlist": a.ModelAllowlist, "modelDenylist": a.ModelDenylist, "remoteHubUrls": append([]string(nil), a.RemoteHubURLs...), "remoteHubBindingsSet": a.RemoteHubBindingsSet, "authMode": a.AuthMode, "workspace": a.Workspace, "target": a.Target, "createdAt": a.CreatedAt, "lastUsedAt": a.LastUsedAt, "wire": a.Wire,
		// These are the operator-entered editor fields. The authenticated local
		// editor needs their values to round-trip an account; native credentials
		// remain excluded and the account key retains its masked representation.
		"extraHeaders": a.ExtraHeaders, "capabilities": a.Capabilities, "systemPromptPrefix": a.SystemPromptPrefix, "fallbackModel": a.FallbackModel, "maxRpm": a.MaxRPM, "maxTpm": a.MaxTPM, "speedMode": config.AccountSpeedMode(a.SpeedMode)}
}

// handleLocalPool changes only the optional local account-pool membership.
// It deliberately does not restart an Agent or alter the account currently
// applied to a desktop tool. Existing streams keep their pinned credential;
// the pool is consulted only for a new request/session.
func (s *Server) handleLocalPool(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID                  string  `json:"id"`
		Enabled             bool    `json:"enabled"`
		Group               string  `json:"group"`
		Strategy            *string `json:"strategy,omitempty"`
		Priority            *int    `json:"priority,omitempty"`
		Weight              *int    `json:"weight,omitempty"`
		Backup              *bool   `json:"backup,omitempty"`
		MaxConcurrency      *int    `json:"maxConcurrency,omitempty"`
		QuotaReservePercent *int    `json:"quotaReservePercent,omitempty"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !config.ValidAccountID(in.ID) {
		writeErr(w, 400, "无效的账号编号")
		return
	}
	err := s.d.Store.Update(func(c *config.Config) error {
		for i := range c.LocalAccounts {
			if c.LocalAccounts[i].ID != in.ID {
				continue
			}
			a := c.LocalAccounts[i]
			if in.Strategy != nil {
				a.PoolStrategy = strings.TrimSpace(*in.Strategy)
			}
			if in.Priority != nil {
				a.Priority = *in.Priority
			}
			if in.Weight != nil {
				a.Weight = *in.Weight
			}
			if in.Backup != nil {
				a.PoolBackup = *in.Backup
			}
			if in.MaxConcurrency != nil {
				a.MaxConcurrency = *in.MaxConcurrency
			}
			if in.QuotaReservePercent != nil {
				a.QuotaReservePercent = *in.QuotaReservePercent
			}
			if in.Enabled {
				a.PoolEnabled = true
				if group := strings.TrimSpace(in.Group); group != "" {
					a.PoolGroup = group
				}
				if a.PoolGroup == "" {
					return errors.New("启用账号池前需要填写账号池名称")
				}
			} else {
				a.PoolEnabled = false
			}
			if err := config.ValidateLocalAccount(&a); err != nil {
				return err
			}
			c.LocalAccounts[i] = a
			return nil
		}
		return errors.New("账号不存在")
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if a, ok := s.d.Store.Get().LocalAccount(in.ID); ok {
		writeJSON(w, map[string]any{"ok": true, "account": maskedAccount(a)})
		return
	}
	writeErr(w, 404, "账号不存在")
}

func (s *Server) handleLocalAccounts(w http.ResponseWriter, r *http.Request) {
	c := s.d.Store.Get()
	official := map[string]officialAccountDisplay{}
	officialTags := map[string]accountTagDisplay{}
	for _, row := range s.officialAccountDisplays() {
		official[row.ID] = row
		officialTags[row.ID] = accountTagsDisplay(c, "official:"+row.ID, row.IdentityEpoch, "")
	}
	poolStates := map[string]gateway.PoolAccountState{}
	if s.accountPool != nil {
		for _, state := range s.accountPool.Snapshot(c.LocalAccounts) {
			poolStates[state.AccountID] = state
		}
	}
	accounts := []map[string]any{}
	for _, a := range c.LocalAccounts {
		entry := maskedAccount(a)
		// Process-bound HMAC, not the raw credential fingerprint. Account-card
		// launch confirmation can detect same-ID replacements without exposing
		// a key or a reusable offline guess oracle. An unarmed runtime is empty.
		entry["sourceRevision"] = c.AccountSourceRevision(a)
		if a.CredentialRef == "" {
			entry["tagMetadata"] = accountTagsDisplay(c, "local:"+a.ID, 0, c.AccountSourceRevision(a))
		}
		entry["remoteSupport"] = accountSavedRemoteSupport(c, a)
		quotaOnly := false
		if a.CredentialRef != "" && s.d.CodexAccounts != nil {
			if info, ok := official[a.CredentialRef]; ok {
				entry["officialAccount"] = info
				// The protected account owns its display preference. A rotation or
				// metadata save must not leave a stale copied card name on reads.
				if a.CredentialEpoch == info.IdentityEpoch {
					entry["name"] = info.Name
				}
				quotaOnly = info.Mode == "chatgpt_web_session"
			}
		}
		entry["modelCount"] = len(a.Models)
		entry["poolState"] = map[string]any{"inflight": 0, "failures": 0, "coolingDown": false}
		if state, ok := poolStates[a.ID]; ok {
			entry["poolState"] = state
		}
		// These are diagnostics for the card and remote selector, not an
		// authorization decision. They reveal no key or prompt data.
		codex := c.APIForTool(a, "codex")
		claude := c.APIForTool(a, "claude")
		entry["supportsCodex"] = a.Kind == "api" || a.Kind == "codex"
		entry["supportsClaude"] = a.Kind == "api" || a.Kind == "claude"
		entry["codexNeedsGateway"] = c.GatewayRuntimeKey != "" || config.NeedsGateway(codex)
		entry["claudeNeedsGateway"] = c.GatewayRuntimeKey != "" || config.NeedsGateway(claude)
		entry["mixedModels"] = config.MixedModels(a)
		entry["apiService"] = localAPIServiceMembership(a.ID, c.CodexAPI.Clients, quotaOnly)
		accounts = append(accounts, entry)
	}
	tools, pending := s.d.Local.InventorySnapshot(c.LocalToolPaths)
	native, nativePending := s.nativeConnectionSnapshot()
	hubState := "not_logged_in"
	if s.d.Hub != nil {
		status := s.d.Hub.Status()
		hubState = status.State
		if c.RemoteDeviceOnly && hubState == "connected" && (status.Pairing == nil || !status.Pairing.Paired) {
			hubState = "unpaired"
		}
	}
	bindings := toolBindings(c, tools, hubState, native)
	poolState := []gateway.PoolAccountState{}
	if s.accountPool != nil {
		poolState = s.accountPool.Snapshot(c.LocalAccounts)
	}
	writeJSON(w, map[string]any{"accounts": accounts, "officialAccountTags": officialTags, "remoteStations": remoteConnectionChoices(c), "poolState": poolState, "activeCodexAccount": c.ActiveCodexAccount, "activeClaudeAccount": c.ActiveClaudeAccount, "tools": tools, "discoveryPending": pending, "nativePending": nativePending, "toolPaths": c.LocalToolPaths, "bindings": bindings, "desktopControl": native.Active, "nativeConnection": native})
}

type localAPIClientMembership struct {
	ClientID   string `json:"clientId"`
	ClientName string `json:"clientName"`
	Text       bool   `json:"text"`
	Image      bool   `json:"image"`
	Primary    bool   `json:"primary"`
	Disabled   bool   `json:"disabled"`
	Active     bool   `json:"active"`
}

type localAPIServiceMembershipDTO struct {
	Member                bool                       `json:"member"`
	ActiveClientCount     int                        `json:"activeClientCount"`
	ConfiguredClientCount int                        `json:"configuredClientCount"`
	Clients               []localAPIClientMembership `json:"clients"`
}

// localAPIServiceMembership mirrors the execution layer's independent text and
// image member sets: only an empty explicit set falls back to AccountID. A
// compatibility anchor remains manageable even when neither set authorizes it.
// Active means membership in an enabled client, not a running service or proof
// of credential/model/quota availability. Known quota-only web sessions cannot
// be execution members, including stale associations left in older configs.
func localAPIServiceMembership(accountID string, clients []config.NamedAPIClient, quotaOnly bool) localAPIServiceMembershipDTO {
	out := localAPIServiceMembershipDTO{Clients: []localAPIClientMembership{}}
	for _, client := range clients {
		primary := client.AccountID == accountID
		text := containsString(client.AccountIDs, accountID)
		if len(client.AccountIDs) == 0 {
			text = primary
		}
		image := containsString(client.ImageAccountIDs, accountID)
		if len(client.ImageAccountIDs) == 0 {
			image = primary
		}
		if !primary && !text && !image {
			continue
		}
		configured := text || image
		active := configured && !client.Disabled && !quotaOnly
		out.Clients = append(out.Clients, localAPIClientMembership{
			ClientID: client.ID, ClientName: client.Name, Text: text, Image: image,
			Primary: primary, Disabled: client.Disabled, Active: active,
		})
		if configured {
			out.ConfiguredClientCount++
		}
		if active {
			out.ActiveClientCount++
		}
	}
	out.Member = out.ActiveClientCount > 0
	return out
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// detachAPIAccount fences stale named-client routes when a local account is
// deleted. Explicit pool members are filtered; a deleted primary account
// leaves its client disabled until the user deliberately rebinds it.
func detachAPIAccount(c *config.Config, accountID string) {
	for i := range c.CodexAPI.Clients {
		client := &c.CodexAPI.Clients[i]
		if client.AccountID == accountID {
			client.Disabled = true
		}
		client.AccountIDs = removeString(client.AccountIDs, accountID)
		client.ImageAccountIDs = removeString(client.ImageAccountIDs, accountID)
		if client.PoolAccountID == accountID {
			client.PoolAccountID = ""
			client.PoolStrategy = ""
		}
	}
}

func removeString(values []string, removed string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != removed {
			out = append(out, value)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *Server) handleLocalSave(w http.ResponseWriter, r *http.Request) {
	var a config.LocalAccount
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(raw) == 0 || len(raw) > 1<<20 || json.Unmarshal(raw, &a) != nil {
		writeErr(w, 400, "请求格式有误")
		return
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		writeErr(w, 400, "请求格式有误")
		return
	}
	hasField := func(name string) bool { _, ok := fields[name]; return ok }
	var sourceRevision *string
	if hasField("sourceRevision") && json.Unmarshal(fields["sourceRevision"], &sourceRevision) != nil {
		writeErr(w, 400, "账号版本格式有误")
		return
	}
	newAccount := a.ID == ""
	directoryChanged := false
	var committed config.Config
	if newAccount {
		a.ID = config.NewUUID()
	}
	s.modelProbeMu.Lock()
	committed, err = s.d.Store.UpdateWithSnapshot(func(c *config.Config) error {
		index := -1
		for i, old := range c.LocalAccounts {
			if old.ID == a.ID {
				index = i
				break
			}
		}
		if index < 0 && !newAccount {
			return errors.New("这个账号已被移除，请刷新列表")
		}
		if index >= 0 {
			old := c.LocalAccounts[index]
			if hasField("sourceRevision") {
				if sourceRevision == nil {
					return errLocalSourceChanged
				}
				if err := checkLocalSourceRevision(*c, old, sourceRevision); err != nil {
					return err
				}
			}
			if !hasField("name") {
				a.Name = old.Name
			}
			if strings.TrimSpace(a.Key) == "" {
				a.Key = old.Key
			}
			a.CreatedAt, a.LastUsedAt = old.CreatedAt, old.LastUsedAt
			if a.Target == "" {
				a.Target = old.Target
			}
			if !hasField("models") {
				a.Models = old.Models
			}
			if !hasField("modelsManual") {
				a.ModelsManual = old.ModelsManual || hasField("models") && strings.Join(a.Models, "\x00") != strings.Join(old.Models, "\x00")
			}
			// Older desktop UI versions do not send the Cockpit-style source,
			// pool, or model-rule fields. Preserve those fields on an edit instead
			// of silently disabling a pool member or clearing its routing policy.
			if !hasField("sourceType") {
				a.SourceType = old.SourceType
			}
			if !hasField("credentialRef") {
				a.CredentialRef, a.CredentialEpoch = old.CredentialRef, old.CredentialEpoch
			}
			if !hasField("providerId") {
				a.ProviderID = old.ProviderID
			}
			if !hasField("providerName") {
				a.ProviderName = old.ProviderName
			}
			if !hasField("poolGroup") {
				a.PoolGroup = old.PoolGroup
			}
			if !hasField("poolEnabled") {
				a.PoolEnabled = old.PoolEnabled
			}
			if !hasField("poolStrategy") {
				a.PoolStrategy = old.PoolStrategy
			}
			if !hasField("poolBackup") {
				a.PoolBackup = old.PoolBackup
			}
			if !hasField("priority") {
				a.Priority = old.Priority
			}
			if !hasField("weight") {
				a.Weight = old.Weight
			}
			if !hasField("maxConcurrency") {
				a.MaxConcurrency = old.MaxConcurrency
			}
			if !hasField("quotaReservePercent") {
				a.QuotaReservePercent = old.QuotaReservePercent
			}
			if !hasField("modelAllowlist") {
				a.ModelAllowlist = old.ModelAllowlist
			}
			if !hasField("modelDenylist") {
				a.ModelDenylist = old.ModelDenylist
			}
			if !hasField("remoteHubBindingsSet") {
				a.RemoteHubBindingsSet = old.RemoteHubBindingsSet
			}
			if !hasField("remoteHubUrls") {
				a.RemoteHubURLs = old.RemoteHubURLs
			}
			// An older editor cannot erase advanced routing options it does not
			// send. Explicit empty values still reset them in the current editor.
			if !hasField("extraHeaders") {
				a.ExtraHeaders = old.ExtraHeaders
			}
			if !hasField("speedMode") {
				a.SpeedMode = old.SpeedMode
			}
			if !hasField("capabilities") {
				a.Capabilities = old.Capabilities
			}
			if !hasField("systemPromptPrefix") {
				a.SystemPromptPrefix = old.SystemPromptPrefix
			}
			if !hasField("fallbackModel") {
				a.FallbackModel = old.FallbackModel
			}
			if !hasField("maxRpm") {
				a.MaxRPM = old.MaxRPM
			}
			if !hasField("maxTpm") {
				a.MaxTPM = old.MaxTPM
			}
		} else {
			if len(c.LocalAccounts) >= 256 {
				return errors.New("本地最多保存 256 个 API 账号")
			}
			a.CreatedAt, a.LastUsedAt = time.Now().UnixMilli(), 0
			if !hasField("remoteHubBindingsSet") {
				// Older clients still get the expected first-use behavior: a new
				// API joins the Hub currently selected on this computer only.
				a.RemoteHubBindingsSet = true
				current := config.NormalizeHubURL(c.EffectiveHubURL())
				for _, station := range c.RemoteConnections {
					if current != "" && config.NormalizeHubURL(station.HubURL) == current && station.DeviceID != "" && station.DeviceSecret != "" {
						a.RemoteHubURLs = []string{station.HubURL}
						break
					}
				}
			}
		}
		if !a.RemoteHubBindingsSet {
			a.RemoteHubURLs = nil
		} else {
			var bindingErr error
			a.RemoteHubURLs, bindingErr = config.CanonicalRemoteHubBindings(*c, a.RemoteHubURLs)
			if bindingErr != nil {
				return bindingErr
			}
		}
		if err := applyDefaultAPIAccountName(&a); err != nil {
			return err
		}
		if err := config.ValidateLocalAccount(&a); err != nil {
			return err
		}
		a.Models = cleanModels(a.Models)
		if index >= 0 {
			old := c.LocalAccounts[index]
			directoryChanged = modelDirectoryIdentity(old) != modelDirectoryIdentity(a)
			if directoryChanged {
				// A new key/source/policy cannot inherit a directory fetched for
				// the previous connection.  The user can refresh it immediately
				// from the same editor; until then Agent must ask for a model.
				a.Models = nil
				a.ModelsManual = false
				a.Model = ""
			}
			if hasField("models") {
				// An explicit empty directory is a deliberate revocation of the
				// previously discovered routes, not the same as an omitted field.
				// Never leave the old default selected after the user clears it.
				if len(a.Models) == 0 || a.Model != "" && !containsLocalModel(a.Models, a.Model) {
					a.Model = ""
				}
			}
		}
		if index >= 0 {
			c.LocalAccounts[index] = a
		} else {
			c.LocalAccounts = append(c.LocalAccounts, a)
		}
		if (a.Kind == "codex" || a.Kind == "api") && c.ActiveCodexAccount == "" {
			c.ActiveCodexAccount = a.ID
		}
		if (a.Kind == "claude" || a.Kind == "api") && c.ActiveClaudeAccount == "" {
			c.ActiveClaudeAccount = a.ID
		}
		invalidateAccountModelSelections(c, a, directoryChanged || hasField("models") && len(a.Models) == 0)
		return nil
	})
	if err == nil {
		delete(s.modelProbeSeq, a.ID)
	}
	s.modelProbeMu.Unlock()
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errLocalSourceChanged) {
			status = http.StatusConflict
		}
		writeErr(w, status, err.Error())
		return
	}
	if s.d.Hub != nil {
		s.d.Hub.InvalidateModelCatalog(a.ID)
	}
	saved, _ := committed.LocalAccount(a.ID)
	entry := maskedAccount(saved)
	entry["sourceRevision"] = committed.AccountSourceRevision(saved)
	entry["remoteSupport"] = accountSavedRemoteSupport(committed, saved)
	writeJSON(w, map[string]any{"ok": true, "account": entry})
}

func cleanModels(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m != "" && len(m) <= 200 && !strings.ContainsAny(m, "\r\n\x00") && !seen[m] {
			seen[m] = true
			out = append(out, m)
			if len(out) == 1000 {
				break
			}
		}
	}
	return out
}

func containsLocalModel(models []string, wanted string) bool {
	for _, model := range models {
		if model == wanted {
			return true
		}
	}
	return false
}

// modelDirectoryIdentity excludes the selected default model and timestamps:
// a directory fetched for a different credential, endpoint, wire, source, or
// model policy must never be reused after an account edit.
func modelDirectoryIdentity(a config.LocalAccount) string {
	wire := a.Wire
	if wire == "auto" {
		wire = ""
	}
	return strings.Join([]string{
		a.Kind, a.SourceType, a.ProviderID, a.CredentialRef, fmt.Sprint(a.CredentialEpoch),
		a.BaseURL, a.Key, a.AuthMode, wire, a.Protocol,
		strings.Join(a.ModelAllowlist, "\x1f"), strings.Join(a.ModelDenylist, "\x1f"),
	}, "\x00")
}

func invalidateAccountModelSelections(c *config.Config, a config.LocalAccount, clearAll bool) {
	for target, id := range c.ToolAPISelections {
		if id != a.ID {
			continue
		}
		model := c.ToolModels[target]
		if model == "" && !clearAll {
			continue
		}
		if clearAll || !config.ModelAllowed(a, model) || len(a.Models) > 0 && !containsLocalModel(a.Models, model) {
			delete(c.ToolModels, target)
			delete(c.ToolProtocols, target)
		}
	}
	for family, choice := range c.RemoteAPI {
		if choice.AccountID != a.ID || choice.Model == "" {
			continue
		}
		if clearAll || !config.ModelAllowed(a, choice.Model) || len(a.Models) > 0 && !containsLocalModel(a.Models, choice.Model) {
			choice.Model = ""
			c.RemoteAPI[family] = choice
		}
	}
}

func readLocalID(w http.ResponseWriter, r *http.Request) (string, bool) {
	var in struct {
		ID string `json:"id"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return "", false
	}
	if !config.ValidAccountID(in.ID) {
		writeErr(w, 400, "无效的账号编号")
		return "", false
	}
	return in.ID, true
}

func (s *Server) handleLocalDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := readLocalID(w, r)
	if !ok {
		return
	}
	s.modelProbeMu.Lock()
	err := s.d.Store.Update(func(c *config.Config) error {
		found := false
		for i, a := range c.LocalAccounts {
			if a.ID == id {
				c.LocalAccounts = append(c.LocalAccounts[:i], c.LocalAccounts[i+1:]...)
				found = true
				break
			}
		}
		if !found {
			return errors.New("账号不存在")
		}
		for target, selected := range c.ToolAPISelections {
			if selected == id {
				c.ToolAPISelections[target] = ""
				delete(c.ToolModels, target)
				delete(c.ToolProtocols, target)
			}
		}
		if c.ActiveCodexAccount == id {
			c.ActiveCodexAccount = ""
		}
		if c.ActiveClaudeAccount == id {
			c.ActiveClaudeAccount = ""
		}
		detachAPIAccount(c, id)
		for _, a := range c.LocalAccounts {
			if a.Kind == "codex" && c.ActiveCodexAccount == "" {
				c.ActiveCodexAccount = a.ID
			}
			if a.Kind == "claude" && c.ActiveClaudeAccount == "" {
				c.ActiveClaudeAccount = a.ID
			}
		}
		return nil
	})
	if err == nil {
		delete(s.modelProbeSeq, id)
	}
	s.modelProbeMu.Unlock()
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if s.d.Hub != nil {
		s.d.Hub.InvalidateModelCatalog(id)
	}
	writeJSON(w, map[string]any{"ok": true, "message": "已从列表移除账号。正在运行的工具、会话目录和配置备份不会被删除；如需撤销凭据，请在服务商后台撤销 Key。"})
}

func (s *Server) handleLocalSelect(w http.ResponseWriter, r *http.Request) {
	id, ok := readLocalID(w, r)
	if !ok {
		return
	}
	err := s.d.Store.Update(func(c *config.Config) error {
		a, ok := c.LocalAccount(id)
		if !ok {
			return errors.New("账号不存在")
		}
		if a.Kind == "codex" {
			c.ActiveCodexAccount = id
		} else {
			c.ActiveClaudeAccount = id
		}
		return nil
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "message": "默认账号已保存；没有修改系统工具配置"})
}

func (s *Server) handleLocalModels(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	if err := readJSON(r, &raw); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var payload struct {
		config.LocalAccount
		DraftOnly bool `json:"draftOnly"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		writeErr(w, 400, "请求格式有误")
		return
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		writeErr(w, 400, "请求格式有误")
		return
	}
	hasField := func(name string) bool { _, exists := fields[name]; return exists }
	in := payload.LocalAccount
	saved, exists := s.d.Store.Get().LocalAccount(in.ID)
	if exists && (saved.CredentialRef != "" || saved.SourceType == "official_account") {
		if s.d.Hub == nil {
			writeErr(w, 503, "原生 Codex 模型目录不可用，请确认 Codex CLI 已安装并已启动")
			return
		}
		ctx, cancel := ctxTimeout(r, 20*time.Second)
		defer cancel()
		catalog, err := s.d.Hub.OfficialModelCatalog(ctx, saved, true)
		if err != nil {
			writeErr(w, 502, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true, "models": catalog.Models, "modelCapabilities": catalog.ModelCapabilities, "catalogSource": "codex-native-picker-hint", "nativeSource": "codex-native-picker-hint", "catalogIsEntitlement": false, "message": "已读取原生 Codex 模型选择提示，不代表当前官方账号的模型权限；没有发起生成请求"})
		return
	}
	if in.CredentialRef != "" || in.SourceType == "official_account" {
		writeErr(w, 400, "官方模型目录必须使用已保存的官方账号")
		return
	}
	if in.ID != "" && !exists {
		writeErr(w, 404, "账号不存在")
		return
	}
	probeID := in.ID
	var probeSeq uint64
	if exists && !payload.DraftOnly {
		probeSeq = s.beginModelProbe(probeID)
	}
	var probeFingerprint string
	if exists {
		// Keep the exact account fingerprint that supplied this probe.  A model
		// response from an old Key, URL, wire, source, or allow/deny policy must
		// never be committed over a newer card edit.
		probeFingerprint = config.AccountFingerprint(saved)
		if payload.DraftOnly {
			// Missing fields use the saved draft base; explicitly supplied empty
			// lists/headers clear that policy for this read without saving it.
			// Do not unmarshal onto saved maps: encoding/json merges their keys.
			if !hasField("baseUrl") {
				in.BaseURL = saved.BaseURL
			}
			if !hasField("authMode") {
				in.AuthMode = saved.AuthMode
			}
			if !hasField("wire") {
				in.Wire = saved.Wire
			}
			if !hasField("extraHeaders") {
				in.ExtraHeaders = saved.ExtraHeaders
			}
			if !hasField("modelAllowlist") {
				in.ModelAllowlist = saved.ModelAllowlist
			}
			if !hasField("modelDenylist") {
				in.ModelDenylist = saved.ModelDenylist
			}
		}
		if in.Key == "" {
			in.Key = saved.Key
		}
		if in.BaseURL == "" && !payload.DraftOnly {
			in.BaseURL = saved.BaseURL
		}
		if in.Kind == "" {
			in.Kind = saved.Kind
		}
		if in.AuthMode == "" && !payload.DraftOnly {
			in.AuthMode = saved.AuthMode
		}
		// A model probe is part of the account card, so apply the same local
		// visibility policy as the remote picker.  Without copying these rules
		// a desktop refresh could persist a denied model and later overwrite the
		// verified catalogue used by phone/Claude Desktop routes.
		if !payload.DraftOnly {
			in.ModelAllowlist = append([]string(nil), saved.ModelAllowlist...)
			in.ModelDenylist = append([]string(nil), saved.ModelDenylist...)
			in.ExtraHeaders, in.Wire = saved.ExtraHeaders, saved.Wire
		}
	} else {
		in.ID = "probe"
	}
	in.Name = "模型读取"
	if err := config.ValidateLocalAccount(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ctx, cancel := ctxTimeout(r, 20*time.Second)
	defer cancel()
	models, err := launcher.Models(ctx, in)
	if errors.Is(err, launcher.ErrEmptyModelCatalog) {
		models, err = nil, nil
	}
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	models = visibleAccountModels(in, models)
	// Only persist probe results for an unchanged saved provider, not an unsaved edit.
	if exists && !payload.DraftOnly && in.Key == saved.Key && in.BaseURL == saved.BaseURL && in.Kind == saved.Kind && in.AuthMode == saved.AuthMode {
		if err := s.commitModelProbe(saved, probeSeq, probeFingerprint, models, false); err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		if s.d.Hub != nil {
			s.d.Hub.InvalidateModelCatalog(saved.ID)
		}
	}
	if len(models) == 0 {
		writeErr(w, 502, "当前 API 没有可用模型，请检查上游模型分组或本卡模型规则")
		return
	}
	writeJSON(w, map[string]any{"ok": true, "models": models, "message": "已读取模型目录，不代表所有模型均支持所选工具；此操作没有发起生成请求"})
}

func (s *Server) beginModelProbe(id string) uint64 {
	s.modelProbeMu.Lock()
	defer s.modelProbeMu.Unlock()
	if s.modelProbeSeq == nil {
		s.modelProbeSeq = map[string]uint64{}
	}
	s.modelProbeNext++
	s.modelProbeSeq[id] = s.modelProbeNext
	return s.modelProbeNext
}

func (s *Server) commitModelProbe(saved config.LocalAccount, seq uint64, fingerprint string, models []string, manual bool) error {
	_, err := s.commitModelProbeChecked(saved, seq, fingerprint, models, manual, nil)
	return err
}

func (s *Server) commitModelProbeChecked(saved config.LocalAccount, seq uint64, fingerprint string, models []string, manual bool, guard func(config.Config) error) (config.Config, error) {
	s.modelProbeMu.Lock()
	defer s.modelProbeMu.Unlock()
	if seq == 0 || s.modelProbeSeq[saved.ID] != seq {
		return config.Config{}, errors.New("模型读取已过期，请重试")
	}
	defer delete(s.modelProbeSeq, saved.ID)
	return s.d.Store.UpdateWithSnapshot(func(c *config.Config) error {
		if guard != nil {
			if err := guard(*c); err != nil {
				return err
			}
		}
		for i, a := range c.LocalAccounts {
			if a.ID != saved.ID {
				continue
			}
			if config.AccountFingerprint(a) != fingerprint || a.ModelsManual != saved.ModelsManual || strings.Join(a.Models, "\x00") != strings.Join(saved.Models, "\x00") {
				return errors.New("API 在读取期间已更换，请刷新模型后重试")
			}
			a.Models = models
			a.ModelsManual = manual
			if a.Model != "" && !containsLocalModel(models, a.Model) {
				a.Model = ""
			}
			c.LocalAccounts[i] = a
			invalidateAccountModelSelections(c, a, len(models) == 0)
			return nil
		}
		return errors.New("API 在读取期间已移除，请刷新后重试")
	})
}

// visibleAccountModels is deliberately shared by the desktop probe and the
// gateway's lazy Claude Desktop refresh.  Provider /models data is only a
// picker hint, but an account-local deny rule must be reflected before that
// hint is persisted or exposed to another agent family.
func visibleAccountModels(a config.LocalAccount, models []string) []string {
	out := make([]string, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		if seen[model] || !config.ModelAllowed(a, model) {
			continue
		}
		seen[model] = true
		out = append(out, model)
	}
	return out
}

func (s *Server) handleLocalLaunch(w http.ResponseWriter, r *http.Request) {
	if !s.admitUpdateWork(w) {
		return
	}
	var in struct {
		ID        string `json:"id"`
		Target    string `json:"target"`
		Workspace string `json:"workspace"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	launcher.LaunchPlan(r.Context(), in.Target, false, true)
	if !launcher.ValidTarget(in.Target) {
		writeErr(w, 400, "请选择要启动的工具")
		return
	}
	c := s.d.Store.Get()
	a, ok := c.LocalAccount(in.ID)
	if in.ID == "" {
		a, ok = c.SelectedToolAccount(in.Target)
	}
	if !ok {
		writeErr(w, 404, "请先添加并选择一个本地 API 账号")
		return
	}
	ctx, cancel := ctxTimeout(r, 45*time.Second)
	defer cancel()
	launcher.LaunchPhase(ctx, "catalog", "读取可用模型")
	var catalogErr error
	a, catalogErr = s.refreshLaunchCatalog(ctx, a)
	if catalogErr != nil {
		writeErr(w, http.StatusConflict, catalogErr.Error())
		return
	}
	launcher.LaunchPhase(ctx, "selection", "读取当前工具配置")
	vaultAccount := a
	refreshedConfig := s.d.Store.Get()
	if !unchangedSelectionAfterCatalog(refreshedConfig, c, a, in.Target) {
		writeErr(w, http.StatusConflict, "读取模型期间工具选择已改变，请重新打开")
		return
	}
	c = refreshedConfig
	a = c.APIForTool(a, in.Target)
	a = launchCatalogDefault(a)
	if c.GatewayRuntimeKey != "" || config.NeedsGateway(a) {
		writeErr(w, http.StatusConflict, "此账号必须通过工具工作台“应用并重启”；独立实例不会直接写入反代或官方账号凭据")
		return
	}
	result, err := s.d.Local.LaunchAuthorized(ctx, a, in.Target, in.Workspace, c.LocalToolPaths, func() error {
		if s.d.Hub != nil {
			if err := s.d.Hub.CheckUpdateAdmission(); err != nil {
				return err
			}
		}
		currentConfig := s.d.Store.Get()
		current, exists := currentConfig.LocalAccount(a.ID)
		if !exists || !sameLaunchAccount(current, vaultAccount) || currentConfig.GatewayRuntimeKey != c.GatewayRuntimeKey || !sameLaunchSelection(currentConfig, c, in.Target) {
			return errors.New("启动期间 API 已变更，请重新打开")
		}
		return nil
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if in.Target == "codex-desktop" {
		if marker, ok := s.d.Companion.(interface{ MarkManagedLaunch() }); ok {
			marker.MarkManagedLaunch()
		}
	}
	// A launch cannot be undone if saving history fails. Report that independently.
	launcher.LaunchPhase(ctx, "record", "保存本次启动状态")
	err = s.d.Store.Update(func(c *config.Config) error {
		for i, x := range c.LocalAccounts {
			if x.ID == a.ID && sameLaunchAccount(x, vaultAccount) {
				c.LocalAccounts[i].LastUsedAt = time.Now().UnixMilli()
				c.LocalAccounts[i].Target = in.Target
			}
		}
		return nil
	})
	if err != nil {
		message := "工具已启动，最近使用时间保存失败：" + err.Error()
		launcher.LaunchWarning(ctx, "record", message)
		result.Message += " " + message
	}
	s.log.Printf("local launcher: target=%s account=%s pid=%d", in.Target, a.ID, result.PID)
	writeJSON(w, map[string]any{"ok": true, "result": result})
}

func (s *Server) handleLocalDefault(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID     string `json:"id"`
		Kind   string `json:"kind"`
		Action string `json:"action"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ctx, cancel := ctxTimeout(r, 45*time.Second)
	defer cancel()
	var err error
	switch in.Action {
	case "apply":
		c := s.d.Store.Get()
		a, ok := c.LocalAccount(in.ID)
		if !ok {
			writeErr(w, 404, "账号不存在")
			return
		}
		// The legacy default-config action writes the account URL and secret
		// directly into Codex/Claude's native profile.  That is unsafe for
		// reverse-proxy, official/OAuth, pooled, adapted, or credential-ref
		// accounts: another process (or a restart) could then use the upstream
		// without the current Bridge lease.  Those accounts must go through the
		// workbench switch path, which writes a loopback gateway URL and the
		// current process credential and records the applied binding.
		if c.GatewayRuntimeKey != "" || config.NeedsGateway(a) {
			writeErr(w, http.StatusConflict, "此账号必须通过工具工作台“应用并重启”；不会把反代或官方账号凭据直接写入原工具")
			return
		}
		release, admissionErr := s.beginLocalToolChange(ctx, a.Kind)
		if admissionErr != nil {
			writeErr(w, http.StatusConflict, admissionErr.Error())
			return
		}
		defer release()
		err = s.d.Local.ApplyDefault(a)
	case "restore":
		release, admissionErr := s.beginLocalToolChange(ctx, in.Kind)
		if admissionErr != nil {
			writeErr(w, http.StatusConflict, admissionErr.Error())
			return
		}
		defer release()
		target := config.ToolFamily(in.Kind)
		var restoredBinding config.AppliedAPI
		var hadBinding bool
		err = s.d.Local.RestoreDefaultAuthorized(in.Kind, func() error {
			// Read after acquiring the launch lock: a preceding switch may have
			// published a new lease while this restore waited for its turn.
			restoredBinding, hadBinding = s.d.Store.Get().ToolAPIApplied[target]
			if hadBinding {
				s.d.Store.RevokeAppliedLaunchLease(target, restoredBinding.LaunchLease)
				restoredBinding.LaunchLease = ""
			}
			return nil
		})
		if err == nil {
			err = s.d.Store.Update(func(c *config.Config) error {
				if current, ok := c.ToolAPIApplied[target]; hadBinding && ok && reflect.DeepEqual(current, restoredBinding) {
					delete(c.ToolAPIApplied, target)
				}
				return nil
			})
			if err != nil {
				writeErr(w, 400, "原配置已恢复，本次工具连接授权已撤销，但本地应用标记未能保存；请重新打开原工具")
				return
			}
		}
	default:
		writeErr(w, 400, "无效的配置操作")
		return
	}
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "message": "原工具 API 配置已" + map[string]string{"apply": "应用，请重新打开工具", "restore": "恢复；请重新打开原工具，会话和项目数据未改动"}[in.Action]})
}

func (s *Server) handleLocalToolPaths(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Paths map[string]string `json:"paths"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	for target, path := range in.Paths {
		if !launcher.ValidTarget(target) {
			writeErr(w, 400, "无效的工具名称")
			return
		}
		path = strings.Trim(strings.TrimSpace(path), "\"")
		if path != "" {
			if !filepath.IsAbs(path) {
				writeErr(w, 400, "程序路径需要绝对路径，不能填写命令和参数")
				return
			}
			st, err := os.Stat(path)
			if err != nil || (st.IsDir() && !(runtime.GOOS == "darwin" && strings.HasSuffix(strings.ToLower(path), ".app"))) {
				writeErr(w, 400, "程序不存在，请选择可执行文件或 macOS 的 .app")
				return
			}
		}
		in.Paths[target] = path
	}
	err := s.d.Store.Update(func(c *config.Config) error {
		if c.LocalToolPaths == nil {
			c.LocalToolPaths = map[string]string{}
		}
		for k, v := range in.Paths {
			if v == "" {
				delete(c.LocalToolPaths, k)
			} else {
				c.LocalToolPaths[k] = v
			}
		}
		return nil
	})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if s.d.Local != nil {
		s.d.Local.InvalidateInventory()
	}
	writeJSON(w, map[string]bool{"ok": true})
}
