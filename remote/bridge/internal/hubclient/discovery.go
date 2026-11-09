package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/hubingress"
	"salcara/bridge/internal/protocol"
)

type Discovery struct {
	RemoteAccess    *config.StationObservation `json:"remoteAccess,omitempty"`
	Root            string                     `json:"root"`
	HubURL          string                     `json:"hubUrl"`
	Version         string                     `json:"version"`
	ProtocolVersion int                        `json:"protocolVersion"`
	Capabilities    []string                   `json:"capabilities"`
	Identity        *hubingress.Manifest       `json:"-"`
}

func RemoteURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") || u.Opaque != "" || !safeRemotePath(u) || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("请填写不带凭据或查询参数的中转站地址")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("远程设备连接必须使用 HTTPS；HTTP 只允许本机测试")
	}
	u.Host = strings.ToLower(u.Host)
	if u.Scheme == "https" && u.Port() == "443" {
		u.Host = u.Hostname()
		if strings.Contains(u.Host, ":") {
			u.Host = "[" + u.Host + "]"
		}
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u, nil
}

// Discover sends no model key or device secret. A advertised capability is not
// proof of ownership; ownership is established later by one-time pairing.
func Discover(ctx context.Context, raw, override string) (Discovery, error) {
	return discoverAnonymous(ctx, raw, override)
}

// Capability discovery is always anonymous, even for a saved device. New Hubs
// expose effective policy on a separate authenticated endpoint.
func DiscoverForDevice(ctx context.Context, raw, override, deviceID, secret, claim string) (Discovery, error) {
	info, err := Discover(ctx, raw, override)
	if err != nil {
		return info, err
	}
	return DiscoverDevicePolicy(ctx, info, deviceID, secret, claim)
}

func safeRemotePath(u *url.URL) bool {
	if strings.ContainsAny(u.EscapedPath(), "%\\") || strings.Contains(u.Path, "//") {
		return false
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
		for _, ch := range segment {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("._~-", ch)) {
				return false
			}
		}
	}
	return true
}

// DiscoveryTargets preserves an API prefix and tries only anonymous plugin
// endpoints. No request is made to models/completions or a status HTML page.
func DiscoveryTargets(raw, override string) ([]string, error) {
	u, err := RemoteURL(raw)
	if err != nil {
		return nil, err
	}
	root := u.Scheme + "://" + u.Host
	if override != "" {
		h, err := RemoteURL(override)
		if err != nil || h.Scheme != u.Scheme || h.Host != u.Host {
			return nil, errors.New("插件地址必须与中转站同源，不能跨站发送设备凭证")
		}
		return []string{config.NormalizeHubURL(h.String())}, nil
	}
	prefix := strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/v1")
	if strings.HasSuffix(prefix, "/salcara-hub") {
		return []string{root + prefix}, nil
	}
	items := []string{root + prefix + "/salcara-hub"}
	if prefix != "" {
		items = append(items, root+"/salcara-hub")
	}
	// A plugin mounted directly at the supplied API prefix is also a bounded
	// public discovery candidate. Only a 404 at earlier locations reaches it.
	if prefix != "" || strings.HasSuffix(u.Path, "/v1") {
		items = append(items, root+prefix)
	}
	return items, nil
}

func discoverAnonymous(ctx context.Context, raw, override string) (Discovery, error) {
	targets, err := DiscoveryTargets(raw, override)
	if err != nil {
		return Discovery{}, err
	}
	for i, target := range targets {
		info, err := discoverAt(ctx, target)
		if info.HubURL == "" {
			u, _ := url.Parse(target)
			info.HubURL, info.Root = target, u.Scheme+"://"+u.Host
		}
		if err == nil || i == len(targets)-1 {
			return info, err
		}
		var missing *discoveryMissing
		if !errors.As(err, &missing) {
			return info, err
		}
	}
	return Discovery{}, errors.New("无法发现远程插件")
}

type discoveryMissing struct{}

func (*discoveryMissing) Error() string { return "这个中转站没有部署兼容的远程插件" }

func discoverAt(ctx context.Context, hubURL string) (Discovery, error) {
	u, _ := url.Parse(hubURL)
	root := u.Scheme + "://" + u.Host
	req, _ := http.NewRequestWithContext(ctx, "GET", hubURL+"/v1/ping", nil)
	checkedAt := time.Now().UnixMilli()
	client := &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return Discovery{}, errors.New("无法连接这个中转站，请检查网络和 HTTPS 地址")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return Discovery{}, &discoveryMissing{}
	}
	if resp.StatusCode != 200 {
		return Discovery{}, errors.New("远程插件未就绪，或请求被本站反向代理拦截")
	}
	var info struct {
		RemoteAccess    *config.StationObservation `json:"remoteAccess"`
		Service         string                     `json:"service"`
		Protocol        string                     `json:"protocol"`
		Version         string                     `json:"version"`
		ProtocolVersion int                        `json:"protocolVersion"`
		Capabilities    []string                   `json:"capabilities"`
		AuthModes       []string                   `json:"authModes"`
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, protocol.MaxSourcePolicyWireBytes+1))
	if readErr != nil || len(body) > protocol.MaxSourcePolicyWireBytes || json.Unmarshal(body, &info) != nil || info.Service != "salcara-hub" {
		return Discovery{}, errors.New("响应不是兼容的远程插件")
	}
	if info.Protocol != "salcara-remote" || info.ProtocolVersion != 1 {
		return Discovery{}, errors.New("远程插件协议不兼容或版本过旧，请更新插件")
	}
	has := func(items []string, value string) bool {
		for _, item := range items {
			if item == value {
				return true
			}
		}
		return false
	}
	if !has(info.AuthModes, "device-pairing") || !has(info.Capabilities, "pair.qr.v1") || !has(info.Capabilities, "device.identity.v1") || !has(info.Capabilities, "session.remote.v1") || !has(info.Capabilities, "pair.revoke.v1") {
		return Discovery{}, errors.New("本站插件未提供完整的设备扫码功能，请更新插件")
	}
	if info.RemoteAccess != nil {
		info.RemoteAccess.CheckedAt = checkedAt
		guard := "station.guard.v1"
		if info.RemoteAccess.Station != nil && info.RemoteAccess.Station.Version == 2 {
			guard = "station.guard.v2"
		}
		if !has(info.Capabilities, guard) || !config.ValidStationPolicy(info.RemoteAccess.Station) || info.RemoteAccess.Revision == "" || (info.RemoteAccess.Mode != "open" && info.RemoteAccess.Mode != "restricted" && info.RemoteAccess.Mode != "disabled") {
			info.RemoteAccess = nil
		}
	}
	return Discovery{Root: root, HubURL: hubURL, Version: info.Version, ProtocolVersion: info.ProtocolVersion, Capabilities: info.Capabilities, RemoteAccess: info.RemoteAccess}, nil
}
