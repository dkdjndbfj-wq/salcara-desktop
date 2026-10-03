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
)

type Discovery struct {
	Root            string   `json:"root"`
	HubURL          string   `json:"hubUrl"`
	Version         string   `json:"version"`
	ProtocolVersion int      `json:"protocolVersion"`
	Capabilities    []string `json:"capabilities"`
}

func RemoteURL(raw string) (*url.URL, error) {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, errors.New("请填写不带凭据或查询参数的中转站地址")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, errors.New("远程设备连接必须使用 HTTPS；HTTP 只允许本机测试")
	}
	u.Host = strings.ToLower(u.Host)
	return u, nil
}

// Discover sends no model key or device secret. A advertised capability is not
// proof of ownership; ownership is established later by one-time pairing.
func Discover(ctx context.Context, raw, override string) (Discovery, error) {
	u, err := RemoteURL(raw)
	if err != nil {
		return Discovery{}, err
	}
	root := u.Scheme + "://" + u.Host
	hubURL := root + "/salcara-hub"
	if strings.Contains(u.Path, "/salcara-hub") {
		hubURL = strings.TrimSuffix(u.String(), "/v1")
	}
	if override != "" {
		h, err := RemoteURL(override)
		if err != nil || h.Scheme != u.Scheme || h.Host != u.Host {
			return Discovery{}, errors.New("插件地址必须与中转站同源，不能跨站发送设备凭证")
		}
		hubURL = strings.TrimSuffix(h.String(), "/v1")
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", hubURL+"/v1/ping", nil)
	client := &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return Discovery{}, errors.New("无法连接这个中转站，请检查网络和 HTTPS 地址")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return Discovery{}, errors.New("这个中转站没有部署兼容的远程插件")
	}
	if resp.StatusCode != 200 {
		return Discovery{}, errors.New("远程插件未就绪，或请求被本站反向代理拦截")
	}
	var info struct {
		Service         string   `json:"service"`
		Protocol        string   `json:"protocol"`
		Version         string   `json:"version"`
		ProtocolVersion int      `json:"protocolVersion"`
		Capabilities    []string `json:"capabilities"`
		AuthModes       []string `json:"authModes"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&info) != nil || info.Service != "salcara-hub" {
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
	return Discovery{Root: root, HubURL: hubURL, Version: info.Version, ProtocolVersion: info.ProtocolVersion, Capabilities: info.Capabilities}, nil
}
