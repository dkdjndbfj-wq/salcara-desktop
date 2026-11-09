package hubclient

import (
	"context"
	"errors"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/hubingress"
)

var ErrHubIdentityChanged = errors.New("中转站身份已变化，请重新登记这台电脑并扫码绑定；本地 API、项目和对话已保留")
var ErrHubIdentityVerification = errors.New("无法验证中转站身份，未发送电脑凭证，请稍后重试")

// ResolveDiscoveryConnection is anonymous. Only signed endpoint membership and
// a fresh identity proof can associate a new URL with a persisted device pin.
// The saved logical URL stays stable so sources, pairing and receipts keep
// their original scope when a verified ingress address changes.
func ResolveDiscoveryConnection(ctx context.Context, info Discovery, cfg config.Config) (Discovery, *config.RemoteConnection, error) {
	return resolveDiscoveryConnection(ctx, info, cfg, "")
}

func resolveDiscoveryConnection(ctx context.Context, info Discovery, cfg config.Config, botGroupID string) (Discovery, *config.RemoteConnection, error) {
	var exact *config.RemoteConnection
	for _, saved := range cfg.RemoteConnections {
		if saved.BotGroupID == botGroupID && config.NormalizeHubURL(saved.HubURL) == config.NormalizeHubURL(info.HubURL) {
			copy := saved
			exact = &copy
			break
		}
	}
	if !info.HasCapability("ingress.signed-manifest.v1") {
		if exact != nil && (exact.HubInstanceID != "" || exact.IdentityPublicKey != "") {
			return info, exact, ErrHubIdentityChanged
		}
		return info, exact, nil
	}
	m, err := hubingress.ResolveIdentity(ctx, info.HubURL+"/v1")
	if err != nil {
		return info, exact, ErrHubIdentityVerification
	}
	info.Identity = &m
	if exact != nil {
		if exact.HubInstanceID != "" && (exact.HubInstanceID != m.HubInstanceID || exact.IdentityPublicKey != m.IdentityPublicKey) {
			return info, exact, ErrHubIdentityChanged
		}
		return info, exact, nil
	}
	var match *config.RemoteConnection
	for _, saved := range cfg.RemoteConnections {
		if saved.BotGroupID != botGroupID || saved.HubInstanceID != m.HubInstanceID || saved.IdentityPublicKey != m.IdentityPublicKey {
			continue
		}
		if match != nil {
			return info, nil, errors.New("本站有多个已登记电脑身份，请选择原连接后重试")
		}
		copy := saved
		match = &copy
	}
	return info, match, nil
}

// DiscoverForConnection applies the same pin and scoped-policy checks during
// background refresh and phone-triggered station switches as during setup.
func DiscoverForConnection(ctx context.Context, cfg config.Config, saved config.RemoteConnection) (Discovery, error) {
	endpoint := saved.HubURL
	if saved.IngressURL != "" {
		endpoint = config.NormalizeHubURL(saved.IngressURL)
	}
	info, err := Discover(ctx, endpoint, endpoint)
	if err != nil {
		return info, err
	}
	info, resolved, err := resolveDiscoveryConnection(ctx, info, cfg, saved.BotGroupID)
	if err != nil {
		info.RemoteAccess = nil
		return info, err
	}
	if resolved == nil || resolved.DeviceID != saved.DeviceID || resolved.DeviceSecret != saved.DeviceSecret {
		info.RemoteAccess = nil
		return info, ErrHubIdentityChanged
	}
	return DiscoverDevicePolicy(ctx, info, saved.DeviceID, saved.DeviceSecret, saved.EnrollmentClaim)
}
