package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"salcara/bridge/internal/config"
)

func remoteModeChoices(c config.Config) map[string]any {
	hub, bot := !c.RemoteDeviceOnly && c.LoggedIn(), false
	for _, saved := range c.RemoteConnections {
		if saved.DeviceID == "" || saved.DeviceSecret == "" {
			continue
		}
		if saved.BotGroupID == "" {
			hub = true
		} else {
			bot = true
		}
	}
	return map[string]any{
		"direct": map[string]bool{"configured": len(c.DirectRelayHosts) != 0 || c.DirectPhoneID != "", "paired": c.DirectPhoneID != "" && c.DirectPhoneHash != ""},
		"hub":    map[string]bool{"configured": hub}, "bot": map[string]bool{"configured": bot},
	}
}

// Caller owns remoteModeMu through publication and old-listener shutdown.
// The Store update is the admission boundary, before sockets are cleaned up.
func (s *Server) finishRemoteModeSwitch() {
	s.remoteModeSeq++
	s.clearQR()
	if !s.d.Store.Get().DirectRemoteEnabled() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.closeDirectRelay(ctx); err != nil {
			s.log.Printf("remote mode: direct listener closed with error: %v", err)
		}
	}
	if s.d.Hub != nil {
		s.d.Hub.Kick()
	}
}

func (s *Server) handleRemoteMode(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Mode   string `json:"mode"`
		HubURL string `json:"hubUrl,omitempty"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	dec.DisallowUnknownFields()
	if dec.Decode(&in) != nil || dec.Decode(new(any)) != io.EOF {
		writeErr(w, 400, "远程方式参数无效")
		return
	}
	if in.Mode != "none" && in.Mode != "direct" && in.Mode != "hub" && in.Mode != "bot" {
		writeErr(w, 400, "不支持的远程方式")
		return
	}
	if (in.Mode == "none" || in.Mode == "direct") && in.HubURL != "" {
		writeErr(w, 400, "此方式不接受中转站地址")
		return
	}
	s.remoteModeMu.Lock()
	defer s.remoteModeMu.Unlock()
	var err error
	if in.Mode == "direct" {
		err = s.ensureDirectRelay()
	} else {
		err = s.d.Store.Update(func(c *config.Config) error {
			if in.Mode == "none" {
				c.ActivateRemoteMode("none")
				return nil
			}
			var chosen *config.RemoteConnection
			for _, saved := range c.RemoteConnections {
				if saved.DeviceID == "" || saved.DeviceSecret == "" || (saved.BotGroupID != "") != (in.Mode == "bot") {
					continue
				}
				if in.HubURL != "" && config.NormalizeHubURL(saved.HubURL) != config.NormalizeHubURL(in.HubURL) {
					continue
				}
				if chosen == nil || config.NormalizeHubURL(saved.HubURL) == config.NormalizeHubURL(c.EffectiveHubURL()) {
					item := saved
					chosen = &item
				}
			}
			if chosen == nil {
				if in.Mode == "hub" && in.HubURL == "" && !c.RemoteDeviceOnly && c.LoggedIn() {
					c.ActivateRemoteMode("hub")
					return nil
				}
				return errors.New("此方式没有已保存的有效连接，请先完成绑定")
			}
			u, parseErr := url.Parse(chosen.HubURL)
			if parseErr != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
				return errors.New("已保存的中转站地址无效")
			}
			return c.RestoreRemoteConnection(u.Scheme+"://"+u.Host, chosen.HubURL, chosen.DeviceID)
		})
	}
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	s.finishRemoteModeSwitch()
	writeJSON(w, map[string]any{"ok": true, "activeMode": s.d.Store.Get().EffectiveRemoteMode()})
}
