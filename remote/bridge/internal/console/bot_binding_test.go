package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
)

func syntheticBotBinding(group string) botBindingInput {
	return botBindingInput{Type: "salcara-hub-bot-binding", Version: 1, HubURL: "https://bot.example.test/salcara-hub", GroupID: group, Invite: strings.Repeat("a", 64), Signature: strings.Repeat("b", 64)}
}

func TestBotImportCanceledBeforeCommitDoesNotSwitchOrSave(t *testing.T) {
	s, _ := newTestServer(t)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.ConnectRemote("https://original.example.test", "https://original.example.test/salcara-hub")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, sequence := s.d.Store.Get(), s.remoteModeSeq
	body, _ := json.Marshal(syntheticBotBinding("group-a"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/remote/bot-bind", strings.NewReader(string(body))).WithContext(ctx)
	w := httptest.NewRecorder()
	s.handleBotBind(w, req)
	if w.Code == http.StatusOK || !reflect.DeepEqual(before, s.d.Store.Get()) || s.remoteModeSeq != sequence {
		t.Fatal("canceled Bot import saved or activated a connection")
	}
}

func TestBotImportRepeatPreservesExactGroupIdentityAndOnlyAcknowledgesSave(t *testing.T) {
	s, h := newTestServer(t)
	identities := map[string]string{}
	secrets := map[string]string{}
	for _, group := range []string{"group-a", "group-b", "group-a"} {
		w := localRequest(s, h, http.MethodPost, "/api/remote/bot-bind", syntheticBotBinding(group))
		if w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body)
		}
		var ack struct {
			OK         bool   `json:"ok"`
			Stage      string `json:"stage"`
			Registered bool   `json:"registered"`
		}
		if json.Unmarshal(w.Body.Bytes(), &ack) != nil || !ack.OK || ack.Stage != "saved" || ack.Registered {
			t.Fatal("local import incorrectly confirmed remote enrollment", w.Body)
		}
		cfg := s.d.Store.Get()
		if prior := identities[group]; prior != "" && (prior != cfg.DeviceID || secrets[group] != cfg.DeviceSecret) {
			t.Fatal("repeat import replaced the existing device")
		}
		identities[group], secrets[group] = cfg.DeviceID, cfg.DeviceSecret
	}
	if identities["group-a"] == identities["group-b"] || len(s.d.Store.Get().RemoteConnections) != 2 {
		t.Fatal("distinct Bot groups shared or duplicated a device")
	}
}

func TestBotImportRejectsMalformedPackageWithoutChangingConfiguration(t *testing.T) {
	for _, value := range []botBindingInput{
		func() botBindingInput {
			v := syntheticBotBinding("group")
			v.HubURL = "http://bot.example.test/salcara-hub"
			return v
		}(),
		func() botBindingInput {
			v := syntheticBotBinding("group")
			v.HubURL = "https://user@bot.example.test/salcara-hub"
			return v
		}(),
		func() botBindingInput { v := syntheticBotBinding("group"); v.HubURL += "?key=not-real"; return v }(),
		func() botBindingInput {
			v := syntheticBotBinding("group")
			v.Signature = strings.Repeat("A", 64)
			return v
		}(),
		func() botBindingInput { v := syntheticBotBinding("group"); v.Version = 2; return v }(),
	} {
		s, h := newTestServer(t)
		before := s.d.Store.Get()
		if w := localRequest(s, h, http.MethodPost, "/api/remote/bot-bind", value); w.Code != http.StatusBadRequest || !reflect.DeepEqual(before, s.d.Store.Get()) {
			t.Fatal("invalid Bot package changed configuration")
		}
	}
}
