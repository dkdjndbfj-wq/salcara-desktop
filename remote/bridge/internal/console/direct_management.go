package console

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/hubclient"
)

// Computer management is deliberately dispatched before the Hub client. Its
// credential-bearing payload is accepted only on the pinned, paired TLS relay.
func directManagementCommand(typ string) bool {
	return typ == "computer.management.status" || typ == "computer.api.create" || typ == "computer.agent.install"
}

type directManagementRequest struct {
	Type           string   `json:"type"`
	ControlSurface string   `json:"controlSurface"`
	RequestID      string   `json:"requestId,omitempty"`
	Name           string   `json:"name,omitempty"`
	BaseURL        string   `json:"baseUrl,omitempty"`
	Key            string   `json:"key,omitempty"`
	Wire           string   `json:"wire,omitempty"`
	AuthMode       string   `json:"authMode,omitempty"`
	Models         []string `json:"models,omitempty"`
	Tool           string   `json:"tool,omitempty"`
	Confirmed      bool     `json:"confirmed,omitempty"`
}

func parseDirectManagement(command map[string]any) (directManagementRequest, error) {
	var in directManagementRequest
	fields := map[string]bool{"type": true, "controlSurface": true}
	switch command["type"] {
	case "computer.api.create":
		for _, key := range []string{"requestId", "name", "baseUrl", "key", "wire", "authMode", "models"} {
			fields[key] = true
		}
	case "computer.agent.install":
		fields["tool"], fields["confirmed"], fields["requestId"] = true, true, true
	case "computer.management.status":
	default:
		return in, errors.New("不支持的电脑管理操作")
	}
	for key := range command {
		if !fields[key] {
			return in, errors.New("电脑管理参数无效")
		}
	}
	data, err := json.Marshal(command)
	if err != nil || len(data) > 40<<10 {
		return in, errors.New("电脑管理参数无效或过长")
	}
	if err := json.Unmarshal(data, &in); err != nil || in.ControlSurface != "cli" {
		return in, errors.New("电脑管理参数无效")
	}
	return in, nil
}

func (s *Server) dispatchDirectManagement(ctx context.Context, command map[string]any) (any, error) {
	identity, validate, ok := hubclient.DirectExecutionValidator(ctx)
	if !ok || !hubclient.IsDirectExecution(ctx) {
		return nil, errors.New("电脑管理需要已认证的电脑直连")
	}
	check := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := validate(); err != nil {
			return errors.New("电脑直连授权已失效，请重新连接")
		}
		return nil
	}
	if err := check(); err != nil {
		return nil, err
	}
	defer delete(command, "key")
	in, err := parseDirectManagement(command)
	if err != nil {
		return nil, err
	}
	defer func() { in.Key = "" }()
	switch in.Type {
	case "computer.management.status":
		items, pending := s.setupItems()
		// Paths, installer logs, environment and local account IDs stay local.
		public := make([]map[string]any, 0, len(items))
		for _, item := range items {
			public = append(public, map[string]any{"id": item.ID, "name": item.Name, "role": item.Role,
				"installed": item.Installed, "installable": item.Installable && setupInstallAllowed(item.ID),
				"version": item.Version, "note": item.Note})
		}
		setupState.mu.Lock()
		var job any
		busy := setupState.job != nil && setupState.job.State == "running"
		if setupState.job != nil && setupState.job.Owner == identity {
			job = directSetupJob(*setupState.job)
		}
		setupState.mu.Unlock()
		if err := check(); err != nil {
			return nil, err
		}
		return map[string]any{"supported": true, "items": public, "job": job, "busy": busy, "discoveryPending": pending}, nil
	case "computer.agent.install":
		if !in.Confirmed || !setupInstallAllowed(in.Tool) {
			return nil, errors.New("请确认安装受支持的官方工具")
		}
		job, _, err := s.startSetupInstall(in.Tool, identity, in.RequestID, check)
		if err != nil {
			return nil, errors.New("安装未启动，请刷新电脑状态后重试")
		}
		return map[string]any{"ok": true, "job": directSetupJob(job)}, nil
	case "computer.api.create":
		if len(in.BaseURL) > 4096 || strings.ContainsAny(in.Name, "\r\n\x00") {
			return nil, errors.New("API 名称或地址无效")
		}
		if !config.ValidAccountID(in.RequestID) || len(in.RequestID) < 16 {
			return nil, errors.New("新增 API 请求标识无效")
		}
		if len(in.Models) > 100 {
			return nil, errors.New("模型目录最多填写 100 项")
		}
		models := []string{}
		seen := map[string]bool{}
		for _, raw := range in.Models {
			model := strings.TrimSpace(raw)
			if len(model) > 200 || strings.ContainsAny(model, "\r\n\x00") {
				return nil, errors.New("模型目录无效")
			}
			if model != "" && !seen[model] {
				models = append(models, model)
				seen[model] = true
			}
		}
		var saved config.LocalAccount
		if err := check(); err != nil {
			return nil, err
		}
		binding := s.d.Store.Get()
		err := s.d.Store.Update(func(c *config.Config) error {
			// Revoke persists its binding while holding the relay lock. Never
			// acquire that lock from inside the Store lock (inverse lock order).
			// The cancelled admission context plus the stored pairing snapshot
			// fence a command that waited behind revocation or re-pairing.
			if err := ctx.Err(); err != nil {
				return err
			}
			if !c.DirectRelayEnabled || c.DirectPhoneHash == "" || c.DirectPhoneID != binding.DirectPhoneID || c.DirectPhoneHash != binding.DirectPhoneHash {
				return errors.New("电脑直连授权已失效")
			}
			// Stable across reconnect/restart, scoped to the paired phone. A lost
			// response can never make an identical retry create a second entry.
			sum := sha256.Sum256([]byte("direct-api-create-v1\x00" + c.DirectPhoneHash + "\x00" + in.RequestID))
			a := config.LocalAccount{ID: "direct_" + hex.EncodeToString(sum[:20]), Name: in.Name, Kind: "api",
				SourceType: "api_key", BaseURL: in.BaseURL, Key: in.Key, Wire: in.Wire, AuthMode: in.AuthMode,
				Models: models, ModelsManual: len(models) > 0, RemoteHubBindingsSet: true, RemoteHubURLs: []string{}, CreatedAt: time.Now().UnixMilli()}
			if err := config.ValidateLocalAccount(&a); err != nil {
				return errors.New("API 配置无效，请检查名称、地址、密钥和协议")
			}
			if old, exists := c.LocalAccount(a.ID); exists {
				if config.AccountFingerprint(old) != config.AccountFingerprint(a) || old.Name != a.Name || !slices.Equal(old.Models, a.Models) {
					return errors.New("此请求已经保存过不同配置，请刷新后重新新增")
				}
				saved = old
				return nil
			}
			if len(c.LocalAccounts) >= 1000 {
				return errors.New("API 目录已满，请先在电脑端整理")
			}
			c.LocalAccounts = append(c.LocalAccounts, a)
			saved = a
			return nil
		})
		if err != nil {
			// Store errors can contain the private configuration path. The
			// phone receives only a fixed message and verifies the directory.
			return nil, errors.New("API 未能保存，请检查配置并刷新列表核对")
		}
		return map[string]any{"ok": true, "accountId": hubclient.APIHandle(s.d.Store.Get(), saved.ID), "name": saved.Name}, nil
	}
	return nil, errors.New("不支持的电脑管理操作")
}

func directSetupJob(job setupJob) map[string]any {
	errText := ""
	if job.State == "failed" {
		errText = "安装未完成，请在电脑端环境页面查看详情"
	}
	return map[string]any{"id": job.ID, "tool": job.Tool, "state": job.State, "error": errText}
}
