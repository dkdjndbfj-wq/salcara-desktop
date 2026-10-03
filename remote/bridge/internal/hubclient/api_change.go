package hubclient

import (
	"context"
	"errors"
	"strings"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/desktopcompanion"
)

func (c *Client) checkRemoteAPIChange(ctx context.Context, family, sessionKey string) error {
	if !config.ValidRemoteFamily(family) {
		return errors.New("不支持的 Agent")
	}
	if sessionKey != "" && (!strings.HasPrefix(sessionKey, family+":") || len(sessionKey) <= len(family)+1 || len(sessionKey) > 512) {
		return errors.New("会话与 Agent 不一致")
	}
	if manager := c.manager(); manager != nil {
		if agent := manager.Get(family); agent != nil {
			if active, ok := agent.(interface{ ActiveRemoteTurns() bool }); ok {
				if active.ActiveRemoteTurns() {
					return errors.New("这个 Agent 正在处理任务，请结束或停止后再换 API")
				}
			} else {
				sessions, err := agent.Sessions(ctx)
				if err != nil {
					return errors.New("没有确认任务状态，请刷新后再换 API")
				}
				for _, session := range sessions {
					if session.Status == "running" || session.Status == "waiting_approval" {
						return errors.New("这个 Agent 正在处理任务，请结束或停止后再换 API")
					}
				}
			}
		}
	}
	if family == "codex" && sessionKey != "" && c.o.Desktop != nil {
		status, err := c.o.Desktop.NativeStatus(ctx)
		if err != nil {
			// The installed adapter is not an active desktop lease. Background
			// conversations must work without installing/activating the experiment.
			if errors.Is(err, desktopcompanion.ErrActivationRequired) {
				return nil
			}
			return errors.New("无法确认桌面连接状态，请刷新后再换 API")
		}
		if desktopcompanion.ValidateNativeConnection(status, nowMS()) && containsKey(status.SessionKeys, sessionKey) {
			return errors.New("当前由原 Codex 桌面执行，请先结束桌面实时连接，再更换后台 API")
		}
	}
	return nil
}

func remoteModelAccount(cfg config.Config, family string) (config.LocalAccount, bool) {
	if a, ok := cfg.RemoteToolAccount(family); ok {
		return a, true
	}
	if a, ok := cfg.AppliedToolAccount(family); ok {
		return a, true
	}
	// A pending card selection is not the worker's API. While the tool still
	// uses its own login, its model menu must use that tool's own catalog too.
	return config.LocalAccount{}, false
}
