package console

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/hubclient"
	"salcara/bridge/internal/launcher"
)

func TestRemoteConnectionManageExactIdentityAndPersistence(t *testing.T) {
	s, h := remoteModeFixture(t)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.ConnectRemoteWithBotBinding("https://bot.example.test", "https://bot.example.test/salcara-hub", "second-group", "second-invite", "second-signature")
		c.ActivateRemoteMode("none")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := s.d.Store.Get()
	p := before.RemoteConnections[0]
	request := map[string]any{"action": "rename", "mode": "bot", "hubUrl": p.HubURL + "/v1/", "groupId": p.BotGroupID, "expectedRevision": remoteConnectionRevision(p), "name": "  家里的电脑  "}
	w := localRequest(s, h, "POST", "/api/remote/connections/manage", request)
	if w.Code != 200 {
		t.Fatalf("rename: %d %s", w.Code, w.Body)
	}
	changed := s.d.Store.Get()
	if changed.RemoteConnections[0].Name != "家里的电脑" || !reflect.DeepEqual(changed.RemoteConnections[1:], before.RemoteConnections[1:]) {
		t.Fatal("rename changed another identity")
	}
	reopened, err := config.Open(s.d.Store.Path())
	if err != nil || reopened.Get().RemoteConnections[0].Name != "家里的电脑" {
		t.Fatal("rename not durable", err)
	}
	request["action"], request["name"] = "remove", ""
	w = localRequest(s, h, "POST", "/api/remote/connections/manage", request)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "connection_changed") {
		t.Fatal("stale edit allowed", w.Body)
	}
	request["expectedRevision"] = remoteConnectionRevision(changed.RemoteConnections[0])
	w = localRequest(s, h, "POST", "/api/remote/connections/manage", request)
	if w.Code != 200 {
		t.Fatal("remove failed", w.Body)
	}
	after := s.d.Store.Get()
	if !reflect.DeepEqual(after.RemoteConnections, changed.RemoteConnections[1:]) || after.DeviceID != before.DeviceID || after.DirectPhoneHash != before.DirectPhoneHash || !reflect.DeepEqual(after.RemoteAPI, before.RemoteAPI) {
		t.Fatal("remove changed unrelated pairing or API")
	}
	w = localRequest(s, h, "POST", "/api/remote/mode", map[string]any{"mode": "bot", "hubUrl": p.HubURL, "groupId": p.BotGroupID})
	if w.Code != 409 {
		t.Fatal("removed group can still be activated")
	}
	w = localRequest(s, h, "POST", "/api/remote/connections/manage", request)
	if w.Code != 404 {
		t.Fatal("missing connection not reported")
	}
}

func TestRemoteConnectionManageGuardsActiveAndPending(t *testing.T) {
	for _, reason := range []string{"active", "revoke", "enrollment"} {
		t.Run(reason, func(t *testing.T) {
			s, h := remoteModeFixture(t)
			if err := s.d.Store.Update(func(c *config.Config) error {
				p := c.RemoteConnections[1]
				if reason != "active" {
					c.ActivateRemoteMode("none")
				}
				if reason == "revoke" {
					c.PendingPhoneRevokes = []config.PendingPhoneRevoke{{HubURL: p.HubURL, DeviceID: p.DeviceID, BindingID: "pending", PhoneHash: "synthetic"}}
				}
				if reason == "enrollment" {
					c.RemoteEnrollmentAttempts = []config.RemoteEnrollmentAttempt{{Connection: p, RequestID: "pending"}}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := s.d.Store.Get()
			p := before.RemoteConnections[1]
			w := localRequest(s, h, "POST", "/api/remote/connections/manage", map[string]any{"action": "remove", "mode": "hub", "hubUrl": p.HubURL, "expectedRevision": remoteConnectionRevision(p)})
			if w.Code != 409 || !reflect.DeepEqual(s.d.Store.Get(), before) {
				t.Fatal("protected identity changed", w.Body)
			}
		})
	}
}

func TestRemoteConnectionManageDormantCurrentDoesNotDisturbDirect(t *testing.T) {
	s, h := remoteModeFixture(t)
	if err := s.d.Store.Update(func(c *config.Config) error { c.ActivateRemoteMode("direct"); return nil }); err != nil {
		t.Fatal(err)
	}
	before := s.d.Store.Get()
	p := before.RemoteConnections[1]
	w := localRequest(s, h, "POST", "/api/remote/connections/manage", map[string]any{"action": "remove", "mode": "hub", "hubUrl": p.HubURL, "expectedRevision": remoteConnectionRevision(p)})
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	after := s.d.Store.Get()
	if after.EffectiveRemoteMode() != "direct" || after.DeviceID != before.DeviceID || after.DirectPhoneHash != before.DirectPhoneHash || after.RemoteConnectionEpoch != before.RemoteConnectionEpoch {
		t.Fatal("removing dormant Hub disrupted Direct")
	}
	w = localRequest(s, h, "POST", "/api/remote/mode", map[string]any{"mode": "hub"})
	if w.Code != 409 {
		t.Fatal("removed last Hub can still be activated")
	}
	public, _ := json.Marshal(remoteConnectionChoices(after))
	if strings.Contains(string(public), p.DeviceSecret) {
		t.Fatal("saved credentials leaked")
	}
}

func remoteSettingsFixture(t *testing.T) (*Server, http.Handler) {
	s, h := remoteModeFixture(t)
	s.d.Store.SetRuntimeGatewayKey("synthetic-process")
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.ActivateRemoteMode("direct")
		c.LocalAccounts = []config.LocalAccount{{ID: "direct-account", Name: "Direct source", BaseURL: "https://direct.example.test/v1", Key: "synthetic-key", Wire: "responses"}, {ID: "hub-account", Name: "Hub source", BaseURL: "https://hub.example.test/v1", Key: "synthetic-key", Wire: "responses"}}
		c.DirectAPI = map[string]config.RemoteAPI{"codex": {AccountID: "direct-account"}}
		c.RemoteAPI = map[string]config.RemoteAPI{"codex": {AccountID: "hub-account"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.d.Hub = hubclient.New(hubclient.Options{Store: s.d.Store, DiscoverTools: func(context.Context, map[string]string) []launcher.Tool {
		return []launcher.Tool{{ID: "codex", Kind: "codex", Name: "Codex", Available: true}}
	}})
	return s, h
}

func TestRemoteAgentSettingsDirectScope(t *testing.T) {
	s, h := remoteSettingsFixture(t)
	before := s.d.Store.Get()
	w := localRequest(s, h, "GET", "/api/remote/agents", nil)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	var status struct {
		Agents []struct {
			ID  string `json:"id"`
			API struct {
				AccountID string `json:"accountId"`
			} `json:"api"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, agent := range status.Agents {
		if agent.ID == "codex" {
			found = true
			if agent.API.AccountID != hubclient.APIHandle(before, "direct-account") {
				t.Fatal("Direct view read Hub selection", w.Body)
			}
		}
	}
	if !found {
		t.Fatal("missing codex status")
	}
	w = localRequest(s, h, "POST", "/api/remote/agents/api", map[string]any{"agent": "codex", "accountId": "", "expectedSettingsScope": remoteAgentSettingsScope(before)})
	if w.Code != 200 {
		t.Fatal("clear Direct selection", w.Body)
	}
	after := s.d.Store.Get()
	if after.DirectAPI["codex"].AccountID != "" || !reflect.DeepEqual(before.RemoteAPI, after.RemoteAPI) || !reflect.DeepEqual(before.ToolAPISelections, after.ToolAPISelections) {
		t.Fatal("Direct edit crossed into another API scope")
	}
}

func TestRemoteAgentSettingsLeaseAndUnpaired(t *testing.T) {
	for _, change := range []string{"mode", "pairing"} {
		t.Run(change, func(t *testing.T) {
			s, h := remoteSettingsFixture(t)
			ctx, err := s.remoteAgentSettingsContext(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := s.d.Store.Update(func(c *config.Config) error {
				if change == "mode" {
					c.ActivateRemoteMode("hub")
				} else {
					c.DirectPhoneHash = ""
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := s.d.Store.Get()
			_, err = s.d.Hub.Dispatch(ctx, map[string]any{"type": "agents.api.set", "agent": "codex", "accountId": ""})
			if err == nil || !reflect.DeepEqual(before, s.d.Store.Get()) {
				t.Fatal("stale Direct settings lease allowed write")
			}
			if change == "pairing" {
				for _, method := range []string{"GET", "POST"} {
					path := "/api/remote/agents"
					if method == "POST" {
						path += "/api"
					}
					w := localRequest(s, h, method, path, map[string]any{"agent": "codex", "accountId": ""})
					if w.Code != 409 {
						t.Fatal("unpaired Direct fell back to Hub", w.Body)
					}
				}
			}
		})
	}
}

func TestRemoteAgentSettingsHubContextCannotWriteAfterModeChanges(t *testing.T) {
	s, _ := remoteSettingsFixture(t)
	if err := s.d.Store.Update(func(c *config.Config) error { c.ActivateRemoteMode("hub"); return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, err := s.remoteAgentSettingsContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.d.Store.Update(func(c *config.Config) error { c.ActivateRemoteMode("direct"); return nil }); err != nil {
		t.Fatal(err)
	}
	before := s.d.Store.Get()
	for _, typ := range []string{"agents.status", "agents.api.set"} {
		_, err := s.d.Hub.Dispatch(ctx, map[string]any{"type": typ, "agent": "codex", "accountId": ""})
		if err == nil || !reflect.DeepEqual(before, s.d.Store.Get()) {
			t.Fatal("old Hub settings context crossed into Direct", typ)
		}
	}
}

func TestRemoteAgentSettingsRejectsOldFormBeforeDispatch(t *testing.T) {
	for _, change := range []string{"pairing", "pairing-ABA", "mode", "missing"} {
		t.Run(change, func(t *testing.T) {
			s, h := remoteSettingsFixture(t)
			w := localRequest(s, h, "GET", "/api/remote/agents", nil)
			var catalog struct {
				SettingsScope string `json:"settingsScope"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &catalog) != nil || catalog.SettingsScope == "" {
				t.Fatal("missing form scope", w.Body)
			}
			if change == "pairing-ABA" {
				original := s.d.Store.Get().DirectPhoneHash
				for _, hash := range []string{"", original} {
					if err := s.d.Store.Update(func(c *config.Config) error { c.DirectPhoneHash = hash; return nil }); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := s.d.Store.Update(func(c *config.Config) error {
				if change == "pairing" {
					c.DirectPhoneHash = "replacement-phone-hash"
				} else if change == "mode" {
					c.ActivateRemoteMode("hub")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := s.d.Store.Get()
			body := map[string]any{"agent": "codex", "accountId": "", "expectedSettingsScope": catalog.SettingsScope}
			if change == "missing" {
				delete(body, "expectedSettingsScope")
			}
			w = localRequest(s, h, "POST", "/api/remote/agents/api", body)
			if w.Code != 409 || !reflect.DeepEqual(before, s.d.Store.Get()) {
				t.Fatal("old form changed new connection", w.Body)
			}
		})
	}
}
