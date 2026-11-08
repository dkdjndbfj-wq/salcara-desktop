package console

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/config"
)

type apiReadinessClient struct {
	ID                string               `json:"id"`
	Name              string               `json:"name"`
	AccountName       string               `json:"accountName,omitempty"`
	AccountID         string               `json:"accountId,omitempty"`
	CredentialRef     string               `json:"credentialRef,omitempty"`
	State             string               `json:"state"`
	KeyAuthentication string               `json:"keyAuthentication"`
	CredentialState   string               `json:"credentialState,omitempty"`
	Members           []apiReadinessMember `json:"members,omitempty"`
}
type apiReadinessMember struct {
	ID              string `json:"id"`
	Name            string `json:"name,omitempty"`
	State           string `json:"state"`
	CredentialState string `json:"credentialState,omitempty"`
	CredentialRef   string `json:"credentialRef,omitempty"`
}
type apiReadiness struct {
	CheckedAt           int64                `json:"checkedAt"`
	Configured          bool                 `json:"configured"`
	Serving             bool                 `json:"serving"`
	RequiresStart       bool                 `json:"requiresStart"`
	HandlerAvailable    bool                 `json:"handlerAvailable"`
	ReadyClients        int                  `json:"readyClients"`
	UpstreamVerified    bool                 `json:"upstreamVerified"`
	ProviderEntitlement string               `json:"providerEntitlement"`
	Routes              []string             `json:"routes"`
	Clients             []apiReadinessClient `json:"clients"`
}

func (s *Server) codexAPIReadinessRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/local/codex-api/readiness", s.handleCodexAPIReadiness)
	m.HandleFunc("POST /api/local/codex-api/key-check", s.handleCodexAPIKeyCheck)
}

// This is a local configuration snapshot, never a network/entitlement test.
// Source refresh, inference and account mutation are deliberately absent.
func (s *Server) apiClientReadiness(c config.Config, client config.NamedAPIClient) apiReadinessClient {
	out := apiReadinessClient{ID: client.ID, Name: client.Name, AccountID: client.AccountID, KeyAuthentication: "not_checked", State: "configured"}
	if client.Disabled {
		out.State = "client_disabled"
		return out
	}
	if config.ValidateAPIClient(client) != nil {
		out.State = "client_configuration_invalid"
		return out
	}
	if len(client.AccountIDs) > 0 {
		out.AccountName = "文字账号池"
		ready := 0
		for _, id := range client.AccountIDs {
			// Use a fixed-account view for each explicit member; this is a
			// cache-only diagnostic and must not acquire a routing lease.
			memberClient := client
			memberClient.AccountID, memberClient.AccountIDs = id, nil
			memberClient.PoolStrategy, memberClient.PoolAccountID = "", ""
			member := s.apiClientReadiness(c, memberClient)
			out.Members = append(out.Members, apiReadinessMember{ID: id, Name: member.AccountName, State: member.State, CredentialState: member.CredentialState, CredentialRef: member.CredentialRef})
			if member.State == "account_missing" || member.State == "account_configuration_invalid" {
				out.State = "pool_configuration_invalid"
			}
			if member.State == "configured" && (client.PoolStrategy != config.PoolStrategySingleAccount || id == client.PoolAccountID) {
				ready++
			}
		}
		if out.State == "configured" && ready == 0 {
			out.State = "pool_members_unavailable"
		}
		return out
	}
	a, ok := c.LocalAccount(client.AccountID)
	if !ok {
		out.State = "account_missing"
		return out
	}
	out.AccountName = a.Name
	if config.ValidateLocalAccount(&a) != nil {
		out.State = "account_configuration_invalid"
		return out
	}
	if a.SourceType == "official_account" {
		out.CredentialRef = a.CredentialRef
		if a.CredentialRef == "" || s.d.OfficialGateway == nil || s.d.CodexAccounts == nil {
			out.State = "official_executor_unavailable"
			return out
		}
		availability, err := s.d.CodexAccounts.ExecutionAvailability(a.CredentialRef, a.CredentialEpoch)
		if err != nil {
			out.State = "credential_binding_unavailable"
			return out
		}
		out.CredentialState = readinessCredentialState(availability)
		if !availability.AccessUsable {
			switch out.CredentialState {
			case "save_pending":
				out.State = "credential_save_pending"
			case "login_required":
				out.State = "credential_login_required"
			case "refresh_failed":
				out.State = "credential_refresh_failed"
			default:
				out.State = "credential_refresh_required"
			}
		}
	}
	return out
}

func readinessCredentialState(a codexaccounts.ExecutionAvailability) string {
	if a.PendingSave {
		return "save_pending"
	}
	if a.NeedsLogin {
		return "login_required"
	}
	if a.RequiresRefresh {
		if !a.RefreshEligible {
			return "refresh_failed"
		}
		return "refresh_required"
	}
	if a.RefreshFailed {
		return "refresh_failed"
	}
	if a.RefreshRecommended {
		return "refresh_recommended"
	}
	return "ready"
}
func (s *Server) readAPIReadiness(c config.Config) apiReadiness {
	out := apiReadiness{CheckedAt: time.Now().UnixMilli(), Configured: c.CodexAPI.Enabled, Serving: c.CodexAPIActive() && s.apiService != nil, RequiresStart: c.CodexAPI.Enabled && !c.CodexAPIActive(), HandlerAvailable: s.apiService != nil, ProviderEntitlement: "not_checked", Routes: []string{"GET /v1/models", "GET /backend-api/codex/models"}, Clients: []apiReadinessClient{}}
	if c.CodexAPI.ImagesMode != "images_only" {
		out.Routes = append(out.Routes, "POST /v1/responses", "GET /v1/responses (WebSocket compatibility)", "POST /v1/responses/compact", "POST /v1/chat/completions")
		out.Routes = append(out.Routes, "POST /backend-api/codex/responses", "GET /backend-api/codex/responses (WebSocket compatibility)", "POST /backend-api/codex/responses/compact")
	}
	if config.CodexAPIImagesEnabled(c.CodexAPI.ImagesMode) {
		out.Routes = append(out.Routes, "POST /v1/images/generations", "POST /v1/images/edits")
	}
	for _, client := range c.CodexAPI.Clients {
		row := s.apiClientReadiness(c, client)
		if row.State == "configured" {
			out.ReadyClients++
		}
		out.Clients = append(out.Clients, row)
	}
	return out
}
func (s *Server) handleCodexAPIReadiness(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		writeErr(w, 400, "本机就绪检查不接收查询参数")
		return
	}
	writeJSON(w, s.readAPIReadiness(s.d.Store.Get()))
}
func (s *Server) handleCodexAPIKeyCheck(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key   string `json:"key"`
		Model string `json:"model,omitempty"`
	}
	if r.URL.RawQuery != "" || readReverseJSON(r, &in, 4096) != nil || in.Key == "" || len(in.Key) > 256 || strings.ContainsAny(in.Key, "\r\n\x00") || len(in.Model) > 200 || strings.ContainsAny(in.Model, "\r\n\x00") {
		writeErr(w, 400, "请填写客户端 Key，可选填写模型 ID")
		return
	}
	c := s.d.Store.Get()
	digest := config.APIClientDigest(in.Key)
	var client config.NamedAPIClient
	found := false
	for _, row := range c.CodexAPI.Clients {
		if subtle.ConstantTimeCompare([]byte(digest), []byte(row.KeyDigest)) == 1 {
			client, found = row, true
			break
		}
	}
	authenticated := found && !client.Disabled && config.ValidateAPIClient(client) == nil
	state := "client_key_invalid"
	credentialState := ""
	if found {
		readiness := s.apiClientReadiness(c, client)
		state, credentialState = readiness.State, readiness.CredentialState
	}
	var modelAllowed *bool
	if authenticated && in.Model != "" {
		model, resolved := config.ResolveAPIClientModel(client, in.Model)
		allowed := resolved && clientTextModelPolicyAllowed(c, client, model)
		modelAllowed = &allowed
	}
	// A matching key/configured source does not prove that an upstream will grant
	// this model or that a token is currently accepted. Actual requests recheck.
	writeJSON(w, struct {
		Authenticated       bool   `json:"authenticated"`
		State               string `json:"state"`
		CredentialState     string `json:"credentialState,omitempty"`
		Serving             bool   `json:"serving"`
		ModelPolicyAllowed  *bool  `json:"modelPolicyAllowed"`
		UpstreamVerified    bool   `json:"upstreamVerified"`
		ProviderEntitlement string `json:"providerEntitlement"`
	}{authenticated, state, credentialState, c.CodexAPIActive() && s.apiService != nil, modelAllowed, false, "not_checked"})
}

func clientTextModelPolicyAllowed(c config.Config, client config.NamedAPIClient, model string) bool {
	ids := client.AccountIDs
	if len(ids) == 0 {
		ids = []string{client.AccountID}
	}
	allowed := false
	for _, id := range ids {
		a, ok := c.LocalAccount(id)
		if !ok {
			return false
		}
		if client.PoolStrategy == config.PoolStrategySingleAccount && len(client.AccountIDs) > 0 && id != client.PoolAccountID {
			continue
		}
		if !config.ModelAllowed(a, model) {
			continue
		}
		if len(client.AccountIDs) == 0 || a.SourceType == "official_account" && a.CredentialRef != "" || a.Model == model || len(a.Models) == 0 || containsString(a.Models, model) {
			allowed = true
		}
	}
	return allowed
}
