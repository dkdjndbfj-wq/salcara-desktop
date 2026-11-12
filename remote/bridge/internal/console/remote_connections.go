package console

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"salcara/bridge/internal/config"
)

// Excludes changing observations: a background status refresh must not make a
// user's edit stale. Credentials are hashed, never sent to the console.
func remoteConnectionRevision(p config.RemoteConnection) string {
	p.RemoteSupport = nil
	p.HubURL = config.NormalizeHubURL(p.HubURL)
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return "rc-v1-" + hex.EncodeToString(sum[:])
}

type connectionManageError struct {
	code, message string
	status        int
}

func (e *connectionManageError) Error() string { return e.message }
func connectionConflict(code, message string) error {
	return &connectionManageError{code, message, http.StatusConflict}
}

func (s *Server) handleRemoteConnectionManage(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Action           string `json:"action"`
		Mode             string `json:"mode"`
		HubURL           string `json:"hubUrl"`
		GroupID          string `json:"groupId"`
		ExpectedRevision string `json:"expectedRevision"`
		Name             string `json:"name"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	dec.DisallowUnknownFields()
	if dec.Decode(&in) != nil || dec.Decode(new(any)) != io.EOF {
		writeErr(w, 400, "连接管理参数无效")
		return
	}
	in.HubURL, in.Name = config.NormalizeHubURL(in.HubURL), strings.TrimSpace(in.Name)
	if (in.Action != "rename" && in.Action != "remove") || (in.Mode != "hub" && in.Mode != "bot") || in.HubURL == "" ||
		(in.Mode == "hub" && in.GroupID != "") || (in.Mode == "bot" && in.GroupID == "") || len(in.ExpectedRevision) != 70 ||
		(in.Action == "rename" && (in.Name == "" || !utf8.ValidString(in.Name) || utf8.RuneCountInString(in.Name) > 80 || strings.IndexFunc(in.Name, unicode.IsControl) >= 0)) ||
		(in.Action == "remove" && in.Name != "") {
		writeErr(w, 400, "连接管理参数无效")
		return
	}
	s.remoteModeMu.Lock()
	defer s.remoteModeMu.Unlock()
	err := s.d.Store.Update(func(c *config.Config) error {
		index := -1
		for i, p := range c.RemoteConnections {
			if config.NormalizeHubURL(p.HubURL) == in.HubURL && p.BotGroupID == in.GroupID {
				if index != -1 {
					return connectionConflict("connection_changed", "连接记录有重复，请重新登记此连接")
				}
				index = i
			}
		}
		if index == -1 {
			return &connectionManageError{"connection_not_found", "这个连接已移除，请刷新列表", http.StatusNotFound}
		}
		p := c.RemoteConnections[index]
		if remoteConnectionRevision(p) != in.ExpectedRevision {
			return connectionConflict("connection_changed", "连接已变化，请刷新后重试")
		}
		if in.Action == "rename" {
			c.RemoteConnections[index].Name = in.Name
			return nil
		}
		if c.EffectiveRemoteMode() == in.Mode && config.NormalizeHubURL(c.EffectiveHubURL()) == in.HubURL && c.DeviceID == p.DeviceID {
			return connectionConflict("connection_active", "此连接正在使用，请先停用或切换连接")
		}
		for _, pending := range c.PendingPhoneRevokes {
			if config.NormalizeHubURL(pending.HubURL) == in.HubURL && pending.DeviceID == p.DeviceID {
				return connectionConflict("connection_pending", "此连接正在撤销手机绑定，请完成后再移除")
			}
		}
		for _, pending := range c.RemoteEnrollmentAttempts {
			if config.NormalizeHubURL(pending.Connection.HubURL) == in.HubURL && pending.Connection.DeviceID == p.DeviceID {
				return connectionConflict("connection_pending", "此连接仍有待确认的登记，请完成后再移除")
			}
		}
		c.RemoteConnections = append(c.RemoteConnections[:index], c.RemoteConnections[index+1:]...)
		// Keep the dormant top-level device/model context: Direct pairing and
		// gateway credentials also use DeviceID. A removed saved identity cannot
		// be selected by the mode endpoint; a new registration creates a new one.
		return nil
	})
	if err != nil {
		var conflict *connectionManageError
		if errors.As(err, &conflict) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(conflict.status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": conflict.message, "code": conflict.code})
		} else {
			writeErr(w, 500, "无法保存连接设置，请稍后重试")
		}
		return
	}
	if in.Action == "remove" {
		s.remoteModeSeq++
	}
	writeJSON(w, map[string]any{"ok": true, "action": in.Action, "mode": in.Mode, "hubUrl": in.HubURL, "groupId": in.GroupID})
}
