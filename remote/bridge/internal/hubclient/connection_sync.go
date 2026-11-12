package hubclient

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

type phoneConnection struct {
	HubURL     string `json:"hubUrl"`
	DeviceID   string `json:"deviceId"`
	DeviceName string `json:"deviceName"`
	Token      string `json:"token,omitempty"`
	Error      string `json:"error,omitempty"`
	Retained   bool   `json:"retained,omitempty"`
}

var errConnectionCredentialRetained = errors.New("本站已有手机凭据，已保留原连接")

func connectionPhoneSeed(cfg config.Config, station config.RemoteConnection) string {
	mac := hmac.New(sha256.New, []byte(station.DeviceSecret))
	_, _ = mac.Write([]byte(strings.Join([]string{"salcara-phone-connection-v1", config.NormalizeHubURL(station.HubURL), station.DeviceID, cfg.ComputerID, cfg.PhoneBindingID}, "\x00")))
	return hex.EncodeToString(mac.Sum(nil))
}

func connectionPhoneToken(cfg config.Config, station config.RemoteConnection) string {
	mac := hmac.New(sha256.New, []byte(connectionPhoneSeed(cfg, station)))
	_, _ = mac.Write([]byte(cfg.PhoneHash))
	return hex.EncodeToString(mac.Sum(nil))
}

// Only an authenticated command from the bound phone can enumerate its relay
// credentials. Targets are read exclusively from this computer's saved vault;
// the command accepts no URL, device secret, or arbitrary destination.
func (c *Client) syncPhoneConnections(ctx context.Context) (any, error) {
	c.phoneMu.Lock()
	defer c.phoneMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.o.Store == nil {
		return nil, errors.New("电脑配置尚未就绪")
	}
	guard, guarded := commandGuardFromContext(ctx)
	base := c.o.Store.Get()
	if !guarded || !base.HubRemoteEnabled() || base.EffectiveRemoteMode() == "bot" || !base.RemoteDeviceOnly || base.ComputerID == "" || !phoneMatches(base, guard.bindingID, guard.phoneHash) || !c.canUpload(base) {
		return nil, errors.New("手机绑定尚未确认，请稍后重试")
	}
	if err := c.validateCommandGuard(ctx, "remote.connections.sync"); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 17*time.Second)
	defer cancel()
	stations := []config.RemoteConnection{}
	seen := map[string]bool{}
	for _, station := range base.RemoteConnections {
		key := config.NormalizeHubURL(station.HubURL) + "\x00" + station.DeviceID
		if seen[key] || station.DeviceID == "" || station.DeviceSecret == "" || station.BotGroupID != "" || (config.NormalizeHubURL(station.HubURL) == config.NormalizeHubURL(base.EffectiveHubURL()) && station.DeviceID == base.DeviceID) {
			continue
		}
		if len(stations) >= config.MaxRemoteHubBindings {
			return nil, errors.New("电脑保存的中转站过多，请在电脑整理后重试")
		}
		seen[key] = true
		stations = append(stations, station)
	}
	rows := make([]phoneConnection, len(stations))
	var wg sync.WaitGroup
	gate := make(chan struct{}, 4)
	for i, station := range stations {
		wg.Add(1)
		go func(i int, station config.RemoteConnection) {
			defer wg.Done()
			row := phoneConnection{HubURL: config.NormalizeHubURL(station.HubURL) + "/v1", DeviceID: station.DeviceID, DeviceName: base.DeviceName}
			select {
			case gate <- struct{}{}:
			case <-ctx.Done():
				row.Error = "中转站连接超时，稍后自动重试"
				rows[i] = row
				return
			}
			defer func() { <-gate }()
			token, err := c.syncPhoneConnection(ctx, base, station)
			if errors.Is(err, errConnectionCredentialRetained) {
				row.Retained = true
			} else if err != nil {
				row.Error = err.Error()
			} else {
				row.Token = token
			}
			rows[i] = row
		}(i, station)
	}
	wg.Wait()
	if err := c.validateCommandGuard(ctx, "remote.connections.sync"); err != nil {
		return nil, err
	}
	current := c.o.Store.Get()
	if current.ComputerID != base.ComputerID || !phoneMatches(current, base.PhoneBindingID, base.PhoneHash) {
		return nil, errors.New("电脑或手机绑定已变化，请刷新后重试")
	}
	return map[string]any{"computerId": base.ComputerID, "connections": rows}, nil
}

func (c *Client) syncPhoneConnection(ctx context.Context, base config.Config, station config.RemoteConnection) (string, error) {
	if _, err := RemoteURL(station.HubURL); err != nil {
		return "", errors.New("中转站地址无效，请在电脑更新")
	}
	current := c.o.Store.Get()
	saved, exists := current.SavedRemoteConnection(station.HubURL, station.DeviceID)
	if !exists || saved.DeviceSecret != station.DeviceSecret || !phoneMatches(current, base.PhoneBindingID, base.PhoneHash) || config.RemoteIdentity(current) != config.RemoteIdentity(base) {
		return "", errors.New("电脑连接已变化，请重试")
	}
	// The outer 17s synchronization pass still bounds phoneMu ownership.
	ctx, cancel := context.WithTimeout(ctx, hubAuthenticatedRequestTimeout)
	defer cancel()
	// Never forward the saved device secret through a redirect, even if a
	// caller supplied an HTTP client with permissive defaults.
	client := *c.o.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	discoveryReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, config.NormalizeHubURL(station.HubURL)+"/v1/ping", nil)
	resp, err := client.Do(discoveryReq)
	if err != nil {
		return "", errors.New("中转站暂时无法连接，稍后自动重试")
	}
	var discovery struct {
		Service      string   `json:"service"`
		Capabilities []string `json:"capabilities"`
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, protocol.MaxSourcePolicyWireBytes+1))
	resp.Body.Close()
	if resp.StatusCode != 200 || readErr != nil || len(body) > protocol.MaxSourcePolicyWireBytes || json.Unmarshal(body, &discovery) != nil || discovery.Service != "salcara-hub" {
		return "", errors.New("中转站暂时无法连接，稍后自动重试")
	}
	supported := false
	for _, capability := range discovery.Capabilities {
		if capability == "pair.connections.v1" {
			supported = true
		}
	}
	if !supported {
		return "", errors.New("中转站需要更新后才能自动同步绑定")
	}
	current = c.o.Store.Get()
	saved, exists = current.SavedRemoteConnection(station.HubURL, station.DeviceID)
	if !exists || saved.DeviceSecret != station.DeviceSecret || !phoneMatches(current, base.PhoneBindingID, base.PhoneHash) || config.RemoteIdentity(current) != config.RemoteIdentity(base) {
		return "", errors.New("电脑连接已变化，请重试")
	}
	token := connectionPhoneToken(base, station)
	hash := sha256.Sum256([]byte(token))
	body, _ = json.Marshal(map[string]string{"deviceId": station.DeviceID, "computerId": base.ComputerID, "bindingId": base.PhoneBindingID, "phoneHash": base.PhoneHash, "pairHash": hex.EncodeToString(hash[:])})
	req, err := c.requestFor(ctx, activeStationConfig(base, station), http.MethodPost, "/bridge/pair/sync", bytes.NewReader(body), true)
	if err != nil {
		return "", errors.New("电脑连接配置无效")
	}
	resp, err = client.Do(req)
	if err != nil {
		return "", errors.New("中转站同步未确认，稍后自动重试")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		if resp.StatusCode == 409 {
			return "", errors.New("本站手机绑定已更换，请在电脑刷新连接")
		}
		return "", errors.New("中转站同步未确认，稍后自动重试")
	}
	var result struct {
		ComputerID string `json:"computerId"`
		BindingID  string `json:"bindingId"`
		Retained   bool   `json:"retained"`
		Device     struct {
			DeviceID string `json:"deviceId"`
		} `json:"device"`
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, (32<<10)+1))
	if err != nil || len(body) > 32<<10 || json.Unmarshal(body, &result) != nil || result.ComputerID != base.ComputerID || result.BindingID != base.PhoneBindingID || result.Device.DeviceID != station.DeviceID {
		return "", errors.New("中转站返回的电脑身份不一致")
	}
	current = c.o.Store.Get()
	saved, exists = current.SavedRemoteConnection(station.HubURL, station.DeviceID)
	if !exists || saved.DeviceSecret != station.DeviceSecret || !phoneMatches(current, base.PhoneBindingID, base.PhoneHash) || config.RemoteIdentity(current) != config.RemoteIdentity(base) {
		return "", errors.New("电脑连接已变化，请重试")
	}
	if result.Retained {
		return "", errConnectionCredentialRetained
	}
	return token, nil
}
