package console

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/hubclient"
)

// Only these two authenticated local settings endpoints share the phone's
// Direct API selection. Normal local Agent/session commands stay local.
func (s *Server) remoteAgentSettingsContext(ctx context.Context) (context.Context, error) {
	snapshot := s.d.Store.Get()
	return s.remoteAgentSettingsContextFor(ctx, snapshot)
}

func remoteAgentSettingsScope(c config.Config) string {
	parts := []string{c.RemoteConnectionEpoch, c.EffectiveRemoteMode(), c.DeviceID}
	if c.DirectRemoteEnabled() {
		parts = append(parts, c.DirectPhoneID, c.DirectPhoneHash)
	}
	b, _ := json.Marshal(parts)
	sum := sha256.Sum256(b)
	return "ras-v1-" + hex.EncodeToString(sum[:])
}

func (s *Server) remoteAgentSettingsContextFor(ctx context.Context, snapshot config.Config) (context.Context, error) {
	ctx = hubclient.WithRemoteSettingsEpoch(ctx, snapshot.RemoteConnectionEpoch)
	if snapshot.EffectiveRemoteMode() != "direct" {
		return hubclient.WithLocalExecution(ctx), nil
	}
	valid := func(c config.Config) bool {
		return c.DirectRemoteEnabled() && c.DirectPhoneID != "" && c.DirectPhoneHash != "" &&
			c.DirectPhoneID == snapshot.DirectPhoneID && c.DirectPhoneHash == snapshot.DirectPhoneHash &&
			c.DeviceID == snapshot.DeviceID && c.RemoteConnectionEpoch == snapshot.RemoteConnectionEpoch
	}
	validate := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !valid(s.d.Store.Get()) {
			return errors.New("直连连接已变化，请重新连接手机后再试")
		}
		return nil
	}
	if !valid(snapshot) {
		return nil, errors.New("请先完成手机直连配对，再设置直连 Agent 的 API")
	}
	return hubclient.WithDirectExecutionValidator(ctx, "console-settings:"+snapshot.RemoteConnectionEpoch, validate), nil
}

func (s *Server) handleRemoteAgentSettings(typ string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.d.Hub == nil {
			writeErr(w, 503, "程序还在启动")
			return
		}
		cmd := map[string]any{"type": typ}
		if typ == "agents.api.set" {
			if err := readJSON(r, &cmd); err != nil {
				writeErr(w, 400, err.Error())
				return
			}
			cmd["type"] = typ
		}
		ctx, cancel := ctxTimeout(r, 45*time.Second)
		defer cancel()
		snapshot := s.d.Store.Get()
		scope := remoteAgentSettingsScope(snapshot)
		if typ == "agents.api.set" {
			if expected, ok := cmd["expectedSettingsScope"].(string); !ok || expected != scope {
				writeErr(w, 409, "连接或手机配对已变化，请刷新 API 设置后重试")
				return
			}
			delete(cmd, "expectedSettingsScope")
		}
		ctx, err := s.remoteAgentSettingsContextFor(ctx, snapshot)
		if err != nil {
			writeErr(w, 409, err.Error())
			return
		}
		res, err := s.d.Hub.Dispatch(ctx, cmd)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		if scope != remoteAgentSettingsScope(s.d.Store.Get()) {
			writeErr(w, 409, "连接或手机配对已变化，请刷新 API 设置核对")
			return
		}
		if data, ok := res.(map[string]any); ok {
			data["settingsScope"] = scope
		}
		writeJSON(w, res)
	}
}
