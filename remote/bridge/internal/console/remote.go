package console

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/hubclient"

	qrcode "github.com/skip2/go-qrcode"
)

func (s *Server) handleRemoteDiscover(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL             string `json:"url"`
		HubURL          string `json:"hubUrl"`
		EnrollmentClaim string `json:"enrollmentClaim"`
		Connect         bool   `json:"connect"`
		RequireOfficial bool   `json:"requireOfficial,omitempty"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	target, targetErr := remoteDiscoveryTarget(in.URL, in.HubURL)
	if targetErr != nil {
		writeErr(w, http.StatusBadRequest, targetErr.Error())
		return
	}
	claim := strings.TrimSpace(in.EnrollmentClaim)
	if in.Connect && claim != "" && len(claim) != 64 {
		writeErr(w, http.StatusBadRequest, "登记码格式无效，请复制完整的一次性设备登记码")
		return
	}
	if in.Connect && claim != "" {
		if _, err := hex.DecodeString(claim); err != nil {
			writeErr(w, http.StatusBadRequest, "登记码格式无效，请复制完整的一次性设备登记码")
			return
		}
	}
	s.remoteDiscoveryMu.Lock()
	s.remoteDiscoverySeq++
	sequence := s.remoteDiscoverySeq
	if s.remoteDiscoveryLatest == nil {
		s.remoteDiscoveryLatest = map[string]uint64{}
	}
	s.remoteDiscoveryLatest[target] = sequence
	s.remoteDiscoveryMu.Unlock()
	startedAt, runtimeKey := time.Now().UnixMilli(), s.d.Store.Get().GatewayRuntimeKey
	s.remoteModeMu.Lock()
	modeSequence := s.remoteModeSeq
	s.remoteModeMu.Unlock()
	ctx, cancel := ctxTimeout(r, 10*time.Second)
	defer cancel()
	deviceID, deviceSecret := "", ""
	for _, saved := range s.d.Store.Get().RemoteConnections {
		if config.NormalizeHubURL(saved.HubURL) == target {
			deviceID, deviceSecret = saved.DeviceID, saved.DeviceSecret
			break
		}
	}
	// Only an explicit connect operation may use the freshly supplied claim.
	discoveryClaim := ""
	if in.Connect {
		discoveryClaim = claim
	}
	info, err := hubclient.DiscoverForDevice(ctx, in.URL, in.HubURL, deviceID, deviceSecret, discoveryClaim)
	detectErr := err
	observation := config.CloneStationObservation(info.RemoteAccess)
	if observation == nil || detectErr != nil {
		observation = &config.StationObservation{Mode: "unknown"}
	}
	observation.CheckedAt = startedAt
	officialUnavailable := false
	if in.Connect {
		s.remoteModeMu.Lock()
		defer s.remoteModeMu.Unlock()
		if modeSequence != s.remoteModeSeq {
			writeErr(w, http.StatusConflict, "远程方式已变化，请重新确认连接")
			return
		}
	}
	s.remoteDiscoveryMu.Lock()
	if s.remoteDiscoveryLatest[target] != sequence {
		s.remoteDiscoveryMu.Unlock()
		writeErr(w, http.StatusConflict, "检测结果已更新，请重试")
		return
	}
	err = s.d.Store.Update(func(c *config.Config) error {
		if c.GatewayRuntimeKey != runtimeKey {
			return errors.New("设备状态已更新，请重新检测")
		}
		effective := observation
		for _, connection := range c.RemoteConnections {
			if config.NormalizeHubURL(connection.HubURL) == target && connection.RemoteSupport != nil && connection.RemoteSupport.CheckedAt > startedAt {
				effective = connection.RemoteSupport
			}
		}
		if detectErr == nil && in.Connect {
			if in.RequireOfficial && (!effective.Available() || !effective.Station.AllowOfficialAccounts) {
				officialUnavailable = true
			} else {
				if claim != "" {
					c.ConnectRemoteWithEnrollmentClaim(info.Root, info.HubURL, claim)
				} else {
					c.ConnectRemote(info.Root, info.HubURL)
				}
			}
		}
		for i := range c.RemoteConnections {
			if config.NormalizeHubURL(c.RemoteConnections[i].HubURL) == target {
				c.RemoteConnections[i].RemoteSupport = config.CloneStationObservation(effective)
			}
		}
		if target == config.NormalizeHubURL(c.EffectiveHubURL()) && (c.RemoteStationObservation == nil || effective.CheckedAt >= c.RemoteStationObservation.CheckedAt) {
			c.RecordRemoteStationObservation(effective)
		}
		observation = config.CloneStationObservation(effective)
		return nil
	})
	delete(s.remoteDiscoveryLatest, target)
	s.remoteDiscoveryMu.Unlock()
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if detectErr != nil {
		writeErr(w, http.StatusBadRequest, detectErr.Error())
		return
	}
	if officialUnavailable {
		writeErr(w, http.StatusConflict, "本站暂不支持账号远程，请重新选择")
		return
	}
	info.RemoteAccess = observation
	if in.Connect {
		s.finishRemoteModeSwitch()
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
func remoteConnectionChoices(c config.Config) []map[string]any {
	out := []map[string]any{}
	for _, p := range c.RemoteConnections {
		entry := map[string]any{"hubUrl": p.HubURL, "name": strings.TrimPrefix(strings.TrimPrefix(p.HubURL, "https://"), "http://"), "support": stationRemoteSupport(p.RemoteSupport, nil, p.HubURL)}
		// Group identity is display metadata only. Never expose the invite or
		// signature used to register this computer with a Bot relay.
		if p.BotGroupID != "" {
			entry["mode"] = "bot"
			entry["groupId"] = p.BotGroupID
		}
		if p.RemoteSupport != nil && config.ValidStationPolicy(p.RemoteSupport.Station) {
			entry["remoteSupport"] = config.CloneStationObservation(p.RemoteSupport)
		}
		out = append(out, entry)
	}
	return out
}
