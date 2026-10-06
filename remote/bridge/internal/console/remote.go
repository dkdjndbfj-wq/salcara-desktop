package console

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/hubclient"

	qrcode "github.com/skip2/go-qrcode"
)

func (s *Server) handleRemoteDiscover(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL     string `json:"url"`
		HubURL  string `json:"hubUrl"`
		Connect bool   `json:"connect"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ctx, cancel := ctxTimeout(r, 10*time.Second)
	defer cancel()
	info, err := hubclient.Discover(ctx, in.URL, in.HubURL)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if in.Connect {
		err = s.d.Store.Update(func(c *config.Config) error { c.ConnectRemote(info.Root, info.HubURL); return nil })
		if err != nil {
			writeErr(w, 500, "保存设备连接失败")
			return
		}
		s.clearQR()
		if s.d.Hub != nil {
			s.d.Hub.Kick()
		}
	}
	writeJSON(w, map[string]any{"ok": true, "plugin": info, "connected": in.Connect, "message": "插件兼容，设备连接不需要模型 API Key；电脑在线后生成二维码"})
}

func (s *Server) clearQR() { s.pairMu.Lock(); s.pairPNG = nil; s.pairExpires = 0; s.pairMu.Unlock() }

func (s *Server) buildQR(c config.Config, pair hubclient.PairInfo) error {
	if pair.Ticket == "" {
		s.clearQR()
		return nil
	}
	payload, err := json.Marshal(map[string]any{"type": "salcara-remote-pair", "version": 1, "hubUrl": c.EffectiveHubURL() + "/v1", "deviceId": c.DeviceID, "deviceName": c.DeviceName, "computerId": pair.ComputerID, "ticket": pair.Ticket, "expiresAt": pair.ExpiresAt})
	if err != nil {
		return err
	}
	png, err := qrcode.Encode(string(payload), qrcode.Medium, 384)
	if err != nil {
		return err
	}
	s.pairMu.Lock()
	s.pairPNG, s.pairExpires, s.pairIdentity = png, pair.ExpiresAt, config.RemoteIdentity(c)
	s.pairMu.Unlock()
	return nil
}

func (s *Server) handlePairQR(w http.ResponseWriter, r *http.Request) {
	s.pairMu.Lock()
	defer s.pairMu.Unlock()
	if len(s.pairPNG) == 0 || s.pairExpires <= time.Now().UnixMilli() || s.pairIdentity != config.RemoteIdentity(s.d.Store.Get()) {
		writeErr(w, 410, "二维码已失效，请重新生成")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(s.pairPNG)
}

func (s *Server) handlePairRevoke(w http.ResponseWriter, r *http.Request) {
	if s.d.Hub == nil {
		writeErr(w, 503, "远程服务还未准备好")
		return
	}
	ctx, cancel := ctxTimeout(r, 10*time.Second)
	defer cancel()
	if err := s.d.Hub.RevokePair(ctx); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.clearQR()
	writeJSON(w, map[string]any{"ok": true, "message": "手机绑定和未使用的二维码已撤销，API 密钥与原会话未改动"})
}

// Expose endpoints only, never saved per-origin device secrets.
func remoteConnectionChoices(c config.Config) []map[string]string {
	out := []map[string]string{}
	for _, p := range c.RemoteConnections {
		out = append(out, map[string]string{"hubUrl": p.HubURL, "name": strings.TrimPrefix(strings.TrimPrefix(p.HubURL, "https://"), "http://")})
	}
	return out
}
