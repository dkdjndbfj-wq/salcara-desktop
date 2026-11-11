package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/hubclient"
	"salcara/bridge/internal/hubingress"
)

// Updating a route never consumes a Bot invitation or creates a device. The
// user supplies only an anonymous lookup hint for an already pinned profile.
func (s *Server) handleRemoteIngress(w http.ResponseWriter, r *http.Request) {
	var in struct {
		HubURL  string `json:"hubUrl"`
		GroupID string `json:"groupId"`
		URL     string `json:"url"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	dec.DisallowUnknownFields()
	if dec.Decode(&in) != nil || dec.Decode(new(any)) != io.EOF {
		writeErr(w, 400, "连接地址参数无效")
		return
	}
	u, err := hubclient.RemoteURL(in.URL)
	if err != nil || u.Scheme != "https" {
		writeErr(w, 400, "请填写新的 HTTPS 连接地址")
		return
	}
	endpoint := config.NormalizeHubURL(u.String()) + "/v1"
	before := s.d.Store.Get()
	var target *config.RemoteConnection
	for _, saved := range before.RemoteConnections {
		if config.NormalizeHubURL(saved.HubURL) == config.NormalizeHubURL(in.HubURL) && saved.BotGroupID == in.GroupID {
			if target != nil {
				writeErr(w, 409, "原连接不明确，请重新选择")
				return
			}
			copy := saved
			target = &copy
		}
	}
	if target == nil || target.HubInstanceID == "" || target.IdentityPublicKey == "" {
		writeErr(w, 409, "原连接尚未确认服务身份，请先检查原连接")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	var manifest hubingress.Manifest
	if s.d.Hub != nil {
		manifest, err = s.d.Hub.VerifyRemoteIngress(ctx, *target, endpoint)
	} else {
		manifest, err = hubingress.New(nil, "").VerifyPinnedEndpoint(ctx, target.HubURL+"/v1", target.HubInstanceID, target.IdentityPublicKey, endpoint)
	}
	if err != nil {
		if errors.Is(err, hubingress.ErrIdentityChanged) {
			writeRemoteDiscoveryError(w, 409, "hub_identity_changed", hubclient.ErrHubIdentityChanged, hubclient.Discovery{})
			return
		}
		writeErr(w, 503, "暂时无法验证新连接地址，原绑定已保留")
		return
	}
	if manifest.HubInstanceID != target.HubInstanceID || manifest.IdentityPublicKey != target.IdentityPublicKey {
		writeRemoteDiscoveryError(w, 409, "hub_identity_changed", hubclient.ErrHubIdentityChanged, hubclient.Discovery{})
		return
	}
	s.remoteModeMu.Lock()
	defer s.remoteModeMu.Unlock()
	err = s.d.Store.Update(func(c *config.Config) error {
		if ctx.Err() != nil || c.RemoteConnectionEpoch != before.RemoteConnectionEpoch {
			return errors.New("连接已变化或操作已取消，请重新检查")
		}
		for i := range c.RemoteConnections {
			saved := &c.RemoteConnections[i]
			if saved.HubURL != target.HubURL || saved.DeviceID != target.DeviceID || saved.BotGroupID != target.BotGroupID {
				continue
			}
			if saved.DeviceSecret != target.DeviceSecret || saved.HubInstanceID != target.HubInstanceID || saved.IdentityPublicKey != target.IdentityPublicKey || saved.IngressURL != target.IngressURL {
				return errors.New("原连接已更新，请重新检查")
			}
			saved.IngressURL = endpoint
			return nil
		}
		return errors.New("原连接已移除")
	})
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	if s.d.Hub != nil && before.DeviceID == target.DeviceID && config.NormalizeHubURL(before.EffectiveHubURL()) == config.NormalizeHubURL(target.HubURL) {
		s.d.Hub.Kick()
	}
	writeJSON(w, map[string]any{"ok": true, "hubUrl": target.HubURL, "groupId": target.BotGroupID, "ingressUrl": endpoint})
}
