package hubclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/desktopcompanion"
)

// stationSwitchCommand is decoded separately from the public protocol so a
// future client cannot smuggle a device secret or a complete LocalAccount into
// the command. Only the opaque API handle is accepted.
type stationSwitchCommand struct {
	TargetHubURL     string
	TargetDeviceID   string
	TargetComputerID string
	Agent            string
	AccountID        string
	Model            string
	SessionKey       string
	OperationID      string
}

func handoverPayloadHash(in stationSwitchCommand) string {
	// Delimit fields so concatenation cannot make two different payloads share
	// a digest input. These values are handles/metadata only; no API key enters
	// the hash or the persisted config.
	data := strings.Join([]string{in.TargetHubURL, in.TargetDeviceID, in.TargetComputerID, in.Agent, in.AccountID, in.Model, in.SessionKey}, "\x00")
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// Handover receipts are persisted and replayed. Keep their namespace as
// strict as the native command operation namespace instead of accepting
// arbitrary long strings.
func stationSwitchOperationID(id string) bool {
	return desktopcompanion.ValidNativeOperationID(id)
}

func (c *Client) stationSwitchBusy() bool {
	c.qmu.Lock()
	defer c.qmu.Unlock()
	return len(c.queue) > 0 || c.inFlight > 0 || len(c.recovery) > 0
}

// beginStationSwitch closes the queue/flush admission window for the whole
// validation phase.  It must acquire qmu together with flushLoop: if an upload
// already owns an in-flight prefix, the switch is rejected; if the switch wins
// the lock, flushLoop observes the flag and cannot start a new upload before
// the final queue check and config commit.
func (c *Client) beginStationSwitch() error {
	c.qmu.Lock()
	defer c.qmu.Unlock()
	if len(c.queue) > 0 || c.inFlight > 0 || len(c.recovery) > 0 {
		return errors.New("当前站点还有未同步的会话进度，请稍后再切换")
	}
	if c.stationSwitching.Load() {
		return errors.New("中转站正在切换，请稍候")
	}
	if c.handoverAwaitingAuth.Load() {
		return errors.New("中转站正在重新连接，请稍候")
	}
	c.stationSwitching.Store(true)
	return nil
}

func (c *Client) endStationSwitch() {
	c.qmu.Lock()
	c.stationSwitching.Store(false)
	c.qmu.Unlock()
}

// commitStationSwitch keeps the final queue check and persisted identity change
// in one critical section. A late Push must either be observed here (abort on
// A) or read the committed B config; there must not be an unlocked gap between
// the check and the write. All network probes run before taking this lock.
func (c *Client) commitStationSwitch(update func(*config.Config) error) error {
	c.qmu.Lock()
	defer c.qmu.Unlock()
	if len(c.queue) > 0 || c.inFlight > 0 || len(c.recovery) > 0 {
		return errors.New("当前站点还有未同步的会话进度，请稍后再切换中转站")
	}
	err := c.o.Store.Update(update)
	if err == nil {
		// Set this while qmu is still held. Push cannot observe the new B
		// identity without also seeing the retention gate.
		c.handoverAwaitingAuth.Store(true)
	}
	return err
}

func stationRoot(hubURL string) string {
	hub := config.NormalizeHubURL(hubURL)
	return strings.TrimSuffix(hub, "/salcara-hub")
}

func activeStationConfig(base config.Config, station config.RemoteConnection) config.Config {
	next := base.Clone()
	next.RelayRoot = stationRoot(station.HubURL)
	next.HubURL = config.NormalizeHubURL(station.HubURL)
	next.RemoteDeviceOnly = true
	next.DeviceID = station.DeviceID
	next.DeviceSecret = station.DeviceSecret
	return next
}

// verifyStationSwitch checks the target Hub publicly and then proves that the
// saved device credential is accepted by that exact station. No model API key
// is sent during this probe.
func (c *Client) verifyStationSwitch(ctx context.Context, target config.RemoteConnection) (config.Config, error) {
	if config.NormalizeHubURL(target.HubURL) == "" || target.DeviceID == "" || target.DeviceSecret == "" {
		return config.Config{}, errors.New("目标中转站连接记录不完整，请重新绑定")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	discovery, err := Discover(probeCtx, target.HubURL, target.HubURL)
	if err != nil {
		return config.Config{}, fmt.Errorf("目标中转站不可用：%w", err)
	}
	if config.NormalizeHubURL(discovery.HubURL) != config.NormalizeHubURL(target.HubURL) {
		return config.Config{}, errors.New("目标中转站地址发生跳转，已取消切换")
	}
	base := c.o.Store.Get()
	next := activeStationConfig(base, target)
	req, err := c.requestFor(probeCtx, next, http.MethodGet, "/bridge/pair/status?deviceId="+url.QueryEscape(target.DeviceID), nil, false)
	if err != nil {
		return config.Config{}, err
	}
	resp, err := c.o.HTTP.Do(req)
	if err != nil {
		return config.Config{}, errors.New("目标电脑暂时无法连接，请保持电脑端在线")
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return config.Config{}, errors.New("目标中转站没有接受这台电脑的凭证，请先在电脑端连接该站")
		}
		return config.Config{}, fmt.Errorf("目标中转站返回 %d", resp.StatusCode)
	}
	var status struct {
		DeviceID  string `json:"deviceId"`
		Paired    bool   `json:"paired"`
		BindingID string `json:"bindingId"`
		PhoneHash string `json:"phoneHash"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&status); err != nil || status.DeviceID != target.DeviceID || !status.Paired {
		return config.Config{}, errors.New("目标中转站未确认这台电脑")
	}
	if base.PhoneBindingID != "" && (status.BindingID != base.PhoneBindingID || status.PhoneHash != base.PhoneHash) {
		return config.Config{}, errors.New("目标中转站绑定的手机不是当前手机，请先重新配对")
	}
	return next, nil
}

// switchStation atomically changes the desktop's active Hub identity and,
// when requested, the API used by the selected remote agent. The caller sends
// the reply through the old Hub before Kick reconnects the new one.
func (c *Client) switchStation(ctx context.Context, in stationSwitchCommand) (map[string]any, error) {
	if c.o.Store == nil {
		return nil, errors.New("程序还在启动，请稍后再试")
	}
	current := c.o.Store.Get()
	sameStation := config.NormalizeHubURL(in.TargetHubURL) == config.NormalizeHubURL(current.EffectiveHubURL()) && in.TargetDeviceID == current.DeviceID
	if in.OperationID != "" && current.RemoteHandoverOperation == in.OperationID && !sameStation {
		return nil, errors.New("切换请求已提交到另一个中转站，请重新选择")
	}
	if sameStation && current.RemoteHandoverOperation != in.OperationID {
		return nil, errors.New("已经在这个中转站上")
	}
	target, ok := current.SavedRemoteConnection(in.TargetHubURL, in.TargetDeviceID)
	if !ok {
		return nil, errors.New("电脑上没有目标中转站的连接记录，请先在电脑端添加并连接")
	}
	if current.ComputerID == "" || in.TargetComputerID == "" || in.TargetComputerID != current.ComputerID {
		return nil, errors.New("目标中转站不是这台电脑的已配对连接")
	}
	if !stationSwitchOperationID(in.OperationID) {
		return nil, errors.New("切换请求编号无效")
	}
	if current.RemoteHandoverOperation == in.OperationID && current.RemoteHandoverAt > 0 && time.Since(time.UnixMilli(current.RemoteHandoverAt)) > 24*time.Hour {
		return nil, errors.New("切换请求已过期，请重新选择")
	}
	// A retry of an already committed operation is a receipt lookup, not a new
	// handover. Return it before queue checks so events produced after B came
	// online cannot turn a safe idempotent retry into a false rejection.
	if current.RemoteHandoverOperation == in.OperationID && config.NormalizeHubURL(current.EffectiveHubURL()) == config.NormalizeHubURL(target.HubURL) && current.DeviceID == target.DeviceID {
		if current.RemoteHandoverPayloadHash != "" && current.RemoteHandoverPayloadHash != handoverPayloadHash(in) {
			return nil, errors.New("切换请求与已提交操作不一致，请重新选择")
		}
		return map[string]any{"switched": true, "alreadyCommitted": true, "operationId": in.OperationID,
			"payloadHash": current.RemoteHandoverPayloadHash, "hubUrl": config.NormalizeHubURL(target.HubURL), "deviceId": target.DeviceID}, nil
	}
	// Do not move a live/partially-uploaded task to another relay. The caller
	// can retry after the final event has reached A; otherwise the old Hub's
	// history would have a silent tail. Keep the queue admission closed while
	// the target station is probed; a late event makes the final check fail and
	// leaves A active for a safe retry.
	if err := c.beginStationSwitch(); err != nil {
		return nil, err
	}
	defer c.endStationSwitch()
	if current.ComputerID != "" && target.DeviceID == current.DeviceID && config.NormalizeHubURL(target.HubURL) == config.NormalizeHubURL(current.EffectiveHubURL()) {
		return nil, errors.New("目标电脑身份与当前连接重复")
	}
	if in.Agent != "" && !config.ValidRemoteFamily(strings.TrimSuffix(in.Agent, "-desktop")) {
		return nil, errors.New("不支持的 Agent")
	}
	family := strings.TrimSuffix(in.Agent, "-desktop")
	// A station switch changes the event transport for the whole computer, not
	// only the card selected on the phone. Check every worker family so an
	// unrelated running Claude/Codex turn cannot leave a tail on A. The
	// selected session key is still checked against its exact native lease.
	for _, candidate := range config.RemoteFamilies {
		sessionKey := ""
		if candidate == family {
			sessionKey = in.SessionKey
		}
		if err := c.checkRemoteAPIChange(ctx, candidate, sessionKey); err != nil {
			return nil, err
		}
	}
	if c.o.Desktop != nil {
		if native, err := c.o.Desktop.NativeStatus(ctx); err == nil && desktopcompanion.ValidateNativeConnection(native, nowMS()) {
			return nil, errors.New("Codex 桌面实时连接正在使用，请先结束后再切换中转站")
		}
	}
	next, err := c.verifyStationSwitch(ctx, target)
	if err != nil {
		return nil, err
	}
	// The target probe can take several seconds. Re-check immediately before
	// the atomic write so a turn or recovery event that started during that
	// probe cannot be stranded on A.
	if c.stationSwitchBusy() {
		return nil, errors.New("当前站点还有未同步的会话进度，请稍后再切换中转站")
	}
	selectedID := ""
	selectedSnapshot := config.LocalAccount{}
	if in.Agent != "" {
		if in.AccountID != "" {
			var found bool
			selectedID, found = AccountForHandle(next, in.AccountID)
			if !found {
				return nil, errors.New("目标中转站上的 API 不在这台电脑，请刷新后重选")
			}
			selected, _ := next.LocalAccount(selectedID)
			selectedSnapshot = selected
			if strings.TrimSpace(in.Model) != "" {
				models, modelErr := c.verifiedAPIModels(ctx, selected, false)
				if modelErr != nil {
					return nil, modelErr
				}
				if !containsKey(models, strings.TrimSpace(in.Model)) {
					return nil, errors.New("目标 API 不支持该模型，请刷新后重选")
				}
			}
		}
	}
	identity := config.RemoteIdentity(current)
	expectedBinding, expectedPhone := current.PhoneBindingID, current.PhoneHash
	if err := c.commitStationSwitch(func(cfg *config.Config) error {
		if config.RemoteIdentity(*cfg) != identity {
			return errors.New("连接已变化，请刷新后重试")
		}
		if expectedBinding != "" && (cfg.PhoneBindingID != expectedBinding || cfg.PhoneHash != expectedPhone) {
			return errors.New("手机绑定已更换，请重新扫码")
		}
		station, exists := cfg.SavedRemoteConnection(in.TargetHubURL, in.TargetDeviceID)
		if !exists || station.DeviceSecret != target.DeviceSecret {
			return errors.New("目标中转站连接记录已变化，请刷新")
		}
		if selectedID != "" {
			latest, found := cfg.LocalAccount(selectedID)
			if !found || catalogAccountFingerprint(latest) != catalogAccountFingerprint(selectedSnapshot) {
				return errors.New("API 已更换，请刷新后重选")
			}
		}
		apply := activeStationConfig(*cfg, station)
		// Preserve the vault, physical phone binding, projects and sessions; only
		// the active station identity changes.
		cfg.RelayRoot, cfg.HubURL, cfg.RemoteDeviceOnly = apply.RelayRoot, apply.HubURL, apply.RemoteDeviceOnly
		cfg.DeviceID, cfg.DeviceSecret = apply.DeviceID, apply.DeviceSecret
		cfg.RemoteHandoverOperation = in.OperationID
		cfg.RemoteHandoverFrom = identity
		cfg.RemoteHandoverAt = time.Now().UnixMilli()
		cfg.RemoteHandoverPayloadHash = handoverPayloadHash(in)
		if in.Agent != "" {
			if err := cfg.SetRemoteAPI(family, selectedID, in.Model); err != nil {
				return err
			}
			if selectedID != "" {
				mirrorRemoteChoice(cfg, in.Agent, selectedID, in.Model)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if selectedID != "" && strings.TrimSpace(in.Model) == "" {
		// Stored Models are picker hints. A handover must not leave the old
		// provider's verified catalog attached to the newly selected key.
		c.invalidateAPIModels(selectedID)
	}
	return map[string]any{"switched": true, "alreadyCommitted": false, "operationId": in.OperationID,
		"payloadHash": handoverPayloadHash(in), "hubUrl": config.NormalizeHubURL(target.HubURL), "deviceId": target.DeviceID,
		"agent": in.Agent, "apiChanged": in.Agent != ""}, nil
}
