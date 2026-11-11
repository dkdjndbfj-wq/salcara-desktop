package console

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"salcara/bridge/internal/config"
)

type botBindingInput struct {
	Type      string `json:"type"`
	Version   int    `json:"version"`
	HubURL    string `json:"hubUrl"`
	GroupID   string `json:"groupId"`
	Invite    string `json:"invite"`
	Signature string `json:"signature"`
}

func validBotBindingInput(in botBindingInput) bool {
	if in.Type != "salcara-hub-bot-binding" || in.Version != 1 || len(in.GroupID) < 1 || len(in.GroupID) > 128 || strings.TrimSpace(in.GroupID) != in.GroupID || len(in.Invite) != 64 || len(in.Signature) != 64 {
		return false
	}
	for _, value := range []string{in.GroupID, in.Invite, in.Signature} {
		for _, c := range value {
			if c == ' ' || c < 0x21 || c > 0x7e {
				return false
			}
		}
	}
	for _, value := range []string{in.Invite, in.Signature} {
		for _, c := range value {
			if !((c >= 'a' && c <= 'f') || (c >= '0' && c <= '9')) {
				return false
			}
		}
	}
	u, err := url.Parse(in.HubURL)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && strings.TrimRight(u.Path, "/") == "/salcara-hub"
}

func (s *Server) handleBotBind(w http.ResponseWriter, r *http.Request) {
	var in botBindingInput
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(&in) != nil || dec.Decode(new(any)) != io.EOF || !validBotBindingInput(in) {
		writeErr(w, http.StatusBadRequest, "Bot 绑定包无效")
		return
	}
	u, _ := url.Parse(in.HubURL)
	root := u.Scheme + "://" + u.Host
	s.remoteModeMu.Lock()
	defer s.remoteModeMu.Unlock()
	if r.Context().Err() != nil {
		writeErr(w, http.StatusConflict, "Bot 导入已取消，原连接已保留")
		return
	}
	if err := s.d.Store.Update(func(c *config.Config) error {
		if err := r.Context().Err(); err != nil {
			return err
		}
		c.ConnectRemoteWithBotBinding(root, in.HubURL, in.GroupID, in.Invite, in.Signature)
		return nil
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "无法保存 Bot 绑定")
		return
	}
	s.finishRemoteModeSwitch()
	// Import persists a local invitation. The background Hub registration and
	// authenticated command stream determine whether this computer is online.
	writeJSON(w, map[string]any{"ok": true, "hubUrl": config.NormalizeHubURL(in.HubURL), "groupId": in.GroupID, "stage": "saved", "registered": false})
}
