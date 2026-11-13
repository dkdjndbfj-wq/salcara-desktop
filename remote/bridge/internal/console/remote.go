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
	"salcara/bridge/internal/hubingress"

	qrcode "github.com/skip2/go-qrcode"
)

type remoteDiscoverInput struct {
	URL                    string  `json:"url"`
	HubURL                 string  `json:"hubUrl"`
	EnrollmentClaim        string  `json:"enrollmentClaim"`
	Connect                bool    `json:"connect"`
	RequireOfficial        bool    `json:"requireOfficial,omitempty"`
	Reenroll               bool    `json:"reenroll,omitempty"`
	AccountID              string  `json:"accountId,omitempty"`
	ExpectedSourceRevision *string `json:"expectedSourceRevision,omitempty"`
	RetryEnrollment        bool    `json:"retryEnrollment,omitempty"`
}

func (s *Server) handleRemoteDiscover(w http.ResponseWriter, r *http.Request) {
	var in remoteDiscoverInput
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if in.AccountID != "" {
		s.handleRemoteAccountDiscover(w, r, in)
		return
	}
	if in.ExpectedSourceRevision != nil || in.RetryEnrollment {
		writeErr(w, http.StatusBadRequest, "请选择对应的 API 来源")
		return
	}
	target, targetErr := remoteDiscoveryTarget(in.URL, in.HubURL)
	if targetErr != nil {
		writeErr(w, http.StatusBadRequest, targetErr.Error())
		return
	}
	targets, _ := hubclient.DiscoveryTargets(in.URL, in.HubURL)
	if in.Reenroll && !in.Connect {
		writeErr(w, http.StatusBadRequest, "重新登记需要明确连接操作")
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
	for _, item := range targets {
		s.remoteDiscoveryLatest[item] = sequence
	}
	s.remoteDiscoveryMu.Unlock()
	snapshot := s.d.Store.Get()
	startedAt, runtimeKey := time.Now().UnixMilli(), snapshot.GatewayRuntimeKey
	s.remoteModeMu.Lock()
	modeSequence := s.remoteModeSeq
	s.remoteModeMu.Unlock()
	// Discovery includes the public ping, signed identity exchange and policy
	// lookup. Keep their bounded network budgets usable on a cold connection.
	ctx, cancel := ctxTimeout(r, 25*time.Second)
	defer cancel()
	// Only an explicit connect operation may use the freshly supplied claim.
	discoveryClaim := ""
	if in.Connect {
		discoveryClaim = claim
	}
	info, detectErr := hubclient.Discover(ctx, in.URL, in.HubURL)
	var saved *config.RemoteConnection
	var identityErr, policyErr error
	if info.HubURL != "" {
		target = config.NormalizeHubURL(info.HubURL)
	}
	if detectErr != nil {
		for _, item := range snapshot.RemoteConnections {
			if item.BotGroupID == "" && config.NormalizeHubURL(item.HubURL) == target {
				copy := item
				saved = &copy
				break
			}
		}
	}
	if detectErr == nil {
		info, saved, identityErr = hubclient.ResolveDiscoveryConnection(ctx, info, snapshot)
		if info.HubURL != "" {
			target = config.NormalizeHubURL(info.HubURL)
		}
		if saved != nil {
			target = config.NormalizeHubURL(saved.HubURL)
		}
		// A supplied, validated new claim explicitly replaces an old device
		// enrollment. Never send a retired device secret to a replacement Hub.
		if identityErr == nil || in.Connect && in.Reenroll && errors.Is(identityErr, hubclient.ErrHubIdentityChanged) {
			deviceID, deviceSecret := "", ""
			policyClaim := discoveryClaim
			if saved != nil && !in.Reenroll {
				deviceID, deviceSecret = saved.DeviceID, saved.DeviceSecret
				if policyClaim == "" {
					policyClaim = saved.EnrollmentClaim
				}
			}
			info, policyErr = hubclient.DiscoverDevicePolicy(ctx, info, deviceID, deviceSecret, policyClaim)
			if identityErr != nil && (policyErr == nil || hubclient.RemoteErrorCode(policyErr) == "device_credentials_required") {
				identityErr = nil
			}
		} else {
			info.RemoteAccess = nil
		}
	}
	observation := config.CloneStationObservation(info.RemoteAccess)
	if observation == nil || detectErr != nil || identityErr != nil || policyErr != nil {
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
	for _, item := range targets {
		if s.remoteDiscoveryLatest[item] != sequence {
			s.remoteDiscoveryMu.Unlock()
			writeErr(w, http.StatusConflict, "检测结果已更新，请重试")
			return
		}
	}
	err := s.d.Store.Update(func(c *config.Config) error {
		// A canceled probe must not clear a saved observation or select a
		// connection after the caller has switched away. Check inside the
		// transaction too, since admission may wait for the mode/config locks.
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.GatewayRuntimeKey != runtimeKey || c.RemoteConnectionEpoch != snapshot.RemoteConnectionEpoch {
			return errors.New("设备状态已更新，请重新检测")
		}
		if saved != nil {
			current, exists := c.SavedRemoteConnection(saved.HubURL, saved.DeviceID)
			if !exists || current.DeviceSecret != saved.DeviceSecret || current.EnrollmentClaim != saved.EnrollmentClaim || current.HubInstanceID != saved.HubInstanceID || current.IdentityPublicKey != saved.IdentityPublicKey || current.IngressURL != saved.IngressURL {
				return errors.New("电脑登记已变化，请重新检测")
			}
		}
		effective := observation
		scopeID, scopeSecret := "", ""
		if saved != nil {
			scopeID, scopeSecret = saved.DeviceID, saved.DeviceSecret
		}
		matchesScope := func(connection config.RemoteConnection) bool {
			return connection.BotGroupID == "" && config.NormalizeHubURL(connection.HubURL) == target && scopeID != "" && connection.DeviceID == scopeID && connection.DeviceSecret == scopeSecret
		}
		observationNow := time.Now().UnixMilli()
		for _, connection := range c.RemoteConnections {
			if matchesScope(connection) && config.StationObservationIsNewer(connection.RemoteSupport, observation, observationNow) {
				effective = connection.RemoteSupport
			}
		}
		if detectErr == nil && identityErr == nil && in.Connect {
			if in.RequireOfficial && (!effective.Available() || !effective.Station.AllowOfficialAccounts) {
				officialUnavailable = true
			} else if policyErr == nil || hubclient.RemoteErrorCode(policyErr) == "device_credentials_required" {
				logicalHub := info.HubURL
				if saved != nil {
					logicalHub = saved.HubURL
				}
				if in.Reenroll || claim != "" && saved != nil && saved.EnrollmentClaim != claim {
					c.ReenrollRemote(info.Root, logicalHub, claim)
				} else if claim != "" {
					c.ConnectRemoteWithEnrollmentClaim(info.Root, logicalHub, claim)
				} else {
					c.ConnectRemote(info.Root, logicalHub)
				}
				scopeID, scopeSecret = c.DeviceID, c.DeviceSecret
				if info.Identity != nil {
					for i := range c.RemoteConnections {
						if c.RemoteConnections[i].DeviceID == c.DeviceID && config.NormalizeHubURL(c.RemoteConnections[i].HubURL) == config.NormalizeHubURL(logicalHub) {
							c.RemoteConnections[i].HubInstanceID = info.Identity.HubInstanceID
							c.RemoteConnections[i].IdentityPublicKey = info.Identity.IdentityPublicKey
							c.RemoteConnections[i].IngressURL = info.HubURL + "/v1"
						}
					}
				}
			}
		}
		for i := range c.RemoteConnections {
			if matchesScope(c.RemoteConnections[i]) {
				c.RemoteConnections[i].RemoteSupport = config.CloneStationObservation(effective)
			}
		}
		if c.EffectiveRemoteMode() == "hub" && c.DeviceID == scopeID && c.DeviceSecret == scopeSecret && target == config.NormalizeHubURL(c.EffectiveHubURL()) && !config.StationObservationIsNewer(c.RemoteStationObservation, effective, observationNow) {
			c.RecordRemoteStationObservation(effective)
		}
		observation = config.CloneStationObservation(effective)
		return nil
	})
	for _, item := range targets {
		delete(s.remoteDiscoveryLatest, item)
	}
	s.remoteDiscoveryMu.Unlock()
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if detectErr != nil {
		writeErr(w, http.StatusBadRequest, detectErr.Error())
		return
	}
	if identityErr != nil {
		status, code := http.StatusServiceUnavailable, "remote_unavailable"
		if errors.Is(identityErr, hubclient.ErrHubIdentityChanged) {
			status, code = http.StatusConflict, "hub_identity_changed"
		}
		writeRemoteDiscoveryError(w, status, code, identityErr, info)
		return
	}
	if in.Connect && policyErr != nil && hubclient.RemoteErrorCode(policyErr) != "device_credentials_required" {
		writeRemoteDiscoveryError(w, http.StatusForbidden, hubclient.RemoteErrorCode(policyErr), policyErr, info)
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
	out := map[string]any{"ok": true, "plugin": info, "connected": in.Connect, "message": "插件兼容，设备连接不需要模型 API Key；电脑在线后生成二维码"}
	out["registered"], out["stage"] = false, "discovered"
	out["eligibility"] = map[string]any{"supported": true, "method": "manual"}
	if in.Connect {
		out["stage"] = "saved"
	}
	if policyErr != nil {
		out["deviceError"], out["errorCode"] = policyErr.Error(), hubclient.RemoteErrorCode(policyErr)
	}
	writeJSON(w, out)
}

func writeRemoteDiscoveryError(w http.ResponseWriter, status int, code string, err error, info hubclient.Discovery) {
	clockSkew := hubingress.ClockSkew(err)
	if clockSkew != nil && (code == "" || code == "remote_unavailable" || code == "clock_skew") {
		status, code = http.StatusConflict, "clock_skew"
	}
	out := map[string]any{"ok": false, "plugin": info, "connected": false, "error": err.Error(), "code": code}
	if clockSkew != nil && (code == "clock_skew" || code == "enrollment_not_sent") {
		out["clockSkew"] = clockSkew
		if code == "enrollment_not_sent" {
			out["error"] = err.Error() + "；" + (&hubingress.ClockSkewError{}).Error()
		} else {
			out["error"] = (&hubingress.ClockSkewError{}).Error()
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(out)
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
		name := strings.TrimSpace(p.Name)
		if name == "" {
			name = strings.TrimPrefix(strings.TrimPrefix(p.HubURL, "https://"), "http://")
		}
		entry := map[string]any{"hubUrl": p.HubURL, "name": name, "mode": "hub", "revision": remoteConnectionRevision(p), "support": stationRemoteSupport(p.RemoteSupport, nil, p.HubURL)}
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
