package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func (d Discovery) HasCapability(value string) bool {
	for _, item := range d.Capabilities {
		if item == value {
			return true
		}
	}
	return false
}

// DiscoverDevicePolicy never changes service discovery into a model-key test.
// A failed scoped read deliberately discards the global policy from ping.
func DiscoverDevicePolicy(ctx context.Context, info Discovery, deviceID, secret, claim string) (Discovery, error) {
	if !info.HasCapability("discovery.policy.v1") {
		return info, nil
	}
	info.RemoteAccess = nil
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.HubURL+"/v1/discovery/policy", nil)
	if err != nil {
		return info, errors.New("远程策略地址无效")
	}
	if claim != "" {
		req.Header.Set("X-Salcara-Enrollment-Claim", claim)
	} else if deviceID != "" && secret != "" {
		req.Header.Set("X-Salcara-Device-Id", deviceID)
		req.Header.Set("X-Salcara-Device-Secret", secret)
	} else {
		return info, &RemoteHTTPError{Code: "device_credentials_required", StatusCode: http.StatusUnauthorized}
	}
	client := &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return info, &RemoteHTTPError{Code: "remote_unavailable", StatusCode: http.StatusServiceUnavailable}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return info, checkResp(resp)
	}
	var result struct {
		OK           bool                       `json:"ok"`
		Scope        string                     `json:"scope"`
		RemoteAccess *config.StationObservation `json:"remoteAccess"`
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, protocol.MaxSourcePolicyWireBytes+1))
	wantScope := "device"
	if claim != "" {
		wantScope = "enrollment"
	}
	if err != nil || len(data) > protocol.MaxSourcePolicyWireBytes || json.Unmarshal(data, &result) != nil || !result.OK || result.Scope != wantScope || !validDiscoveryObservation(result.RemoteAccess, info) {
		return info, errors.New("远程授权策略响应无效，请更新站点或稍后重试")
	}
	result.RemoteAccess.CheckedAt = time.Now().UnixMilli()
	info.RemoteAccess = result.RemoteAccess
	return info, nil
}

func validDiscoveryObservation(o *config.StationObservation, info Discovery) bool {
	if o == nil || !config.ValidStationPolicy(o.Station) || o.Revision == "" || (o.Mode != "open" && o.Mode != "restricted" && o.Mode != "disabled") {
		return false
	}
	guard := "station.guard.v1"
	if o.Station.Version == 2 {
		guard = "station.guard.v2"
	}
	return info.HasCapability(guard)
}
