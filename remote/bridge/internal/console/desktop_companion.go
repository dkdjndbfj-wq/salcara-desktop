package console

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"salcara/bridge/internal/desktopcompanion"
	"time"
)

type companionControl interface {
	NativeStatus(context.Context) (desktopcompanion.NativeConnection, error)
	NativeDisconnect(context.Context) error
}

const nativeStatusCacheTTL = 750 * time.Millisecond

func emptyNativeConnection() desktopcompanion.NativeConnection {
	return desktopcompanion.NativeConnection{SessionKeys: []string{}}
}

// invalidateNativeConnection is used after an explicit revoke/install change.
// It also makes test adapters and a replaced local companion fail closed
// immediately instead of waiting for the presentation cache to expire.
func (s *Server) invalidateNativeConnection() {
	s.nativeMu.Lock()
	s.nativeEpoch++
	s.nativeAt = time.Time{}
	s.nativeState = emptyNativeConnection()
	s.nativeMu.Unlock()
}

func (s *Server) nativeConnection(ctx context.Context) desktopcompanion.NativeConnection {
	st := emptyNativeConnection()
	service, ok := s.d.Companion.(companionControl)
	if !ok {
		return st
	}
	now := time.Now()
	s.nativeMu.Lock()
	if !s.nativeAt.IsZero() && now.Sub(s.nativeAt) < nativeStatusCacheTTL {
		actual := s.nativeState
		s.nativeMu.Unlock()
		if desktopcompanion.ValidateNativeConnection(actual, now.UnixMilli()) {
			return actual
		}
		return st
	}
	flight := s.nativeFlight
	if flight == nil {
		flight = &nativeStatusFlight{done: make(chan struct{})}
		s.nativeFlight = flight
		epoch := s.nativeEpoch
		go func() {
			probeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			actual, err := service.NativeStatus(probeCtx)
			if err != nil || !desktopcompanion.ValidateNativeConnection(actual, time.Now().UnixMilli()) {
				actual = st
			}
			s.nativeMu.Lock()
			if s.nativeEpoch == epoch {
				s.nativeState = actual
				s.nativeAt = time.Now()
			}
			if s.nativeFlight == flight {
				s.nativeFlight = nil
			}
			close(flight.done)
			s.nativeMu.Unlock()
		}()
	}
	s.nativeMu.Unlock()
	select {
	case <-ctx.Done():
		return st
	case <-flight.done:
		s.nativeMu.Lock()
		actual := s.nativeState
		s.nativeMu.Unlock()
		if desktopcompanion.ValidateNativeConnection(actual, time.Now().UnixMilli()) {
			return actual
		}
		return st
	}
}

// A local key-library read does not need to wait for desktop authorization.
// Return the last valid presentation snapshot and refresh it independently.
func (s *Server) nativeConnectionSnapshot() (desktopcompanion.NativeConnection, bool) {
	if _, ok := s.d.Companion.(companionControl); !ok {
		return emptyNativeConnection(), false
	}
	s.nativeMu.Lock()
	actual, fresh := s.nativeState, !s.nativeAt.IsZero() && time.Since(s.nativeAt) < nativeStatusCacheTTL
	if !fresh && s.nativeFlight == nil {
		// nativeConnection owns/coalesces its own bounded probe. Do not perform
		// network work while holding this lock or the HTTP request goroutine.
		go func() { s.nativeConnection(context.Background()) }()
	}
	s.nativeMu.Unlock()
	if !desktopcompanion.ValidateNativeConnection(actual, time.Now().UnixMilli()) {
		actual = emptyNativeConnection()
	}
	return actual, !fresh
}

func (s *Server) handleDesktopControl(w http.ResponseWriter, r *http.Request) {
	st := s.nativeConnection(r.Context())
	writeJSON(w, map[string]any{"desktopControl": st.Active, "remoteSend": st.Active, "nativeConnection": st, "activationRequired": !st.Active})
}

func (s *Server) handleDisconnectDesktop(w http.ResponseWriter, r *http.Request) {
	if !confirmedCompanionChange(w, r, "断开桌面连接") {
		return
	}
	service, ok := s.d.Companion.(companionControl)
	if !ok {
		writeJSON(w, map[string]any{"disconnected": true, "desktopControl": false, "remoteSend": false})
		return
	}
	ctx, cancel := ctxTimeout(r, 10*time.Second)
	defer cancel()
	if err := service.NativeDisconnect(ctx); err != nil {
		writeErr(w, http.StatusConflict, "无法确认桌面授权已撤回；请在 Codex 中取消连接工具调用，或等待授权到期")
		return
	}
	s.invalidateNativeConnection()
	writeJSON(w, map[string]any{"disconnected": true, "desktopControl": false, "remoteSend": false})
}

func (s *Server) handleDesktopCompanion(w http.ResponseWriter, r *http.Request) {
	if s.d.Companion == nil {
		writeErr(w, http.StatusServiceUnavailable, "本版本未包含桌面验证插件，请使用完整软件包")
		return
	}
	ctx, cancel := ctxTimeout(r, 10*time.Second)
	defer cancel()
	status, err := s.d.Companion.Preview(ctx)
	if err != nil {
		writeErr(w, http.StatusConflict, "无法检查插件安装条件；请检查软件包、本地运行环境及 Codex 配置")
		return
	}
	writeJSON(w, map[string]any{"status": status, "desktopControl": false, "remoteSend": false})
}

func (s *Server) handleInstallDesktopCompanion(w http.ResponseWriter, r *http.Request) {
	if !confirmedCompanionChange(w, r, "安装") {
		return
	}
	if s.d.Companion == nil {
		writeErr(w, http.StatusServiceUnavailable, "本版本未包含桌面验证插件，请使用完整软件包")
		return
	}
	ctx, cancel := ctxTimeout(r, 30*time.Second)
	defer cancel()
	status, err := s.d.Companion.Install(ctx)
	if err != nil {
		// Never reflect configuration content, local paths, or possible credentials.
		writeErr(w, http.StatusConflict, "插件未安装成功；未授权覆盖已有插件或并发修改，请检查本地配置与软件包")
		return
	}
	s.invalidateNativeConnection()
	writeJSON(w, map[string]any{"status": status, "desktopControl": false, "remoteSend": false})
}

func (s *Server) handlePreviewUninstallDesktopCompanion(w http.ResponseWriter, r *http.Request) {
	if s.d.Companion == nil {
		writeErr(w, http.StatusServiceUnavailable, "本版本未包含插件管理，请使用完整软件包")
		return
	}
	ctx, cancel := ctxTimeout(r, 10*time.Second)
	defer cancel()
	status, err := s.d.Companion.PreviewUninstall(ctx)
	if err != nil {
		writeErr(w, http.StatusConflict, "无法确认插件配置归属；同名或已编辑的配置不会自动移除，请手动检查")
		return
	}
	writeJSON(w, map[string]any{"status": status, "desktopControl": false, "remoteSend": false})
}

func (s *Server) handleUninstallDesktopCompanion(w http.ResponseWriter, r *http.Request) {
	if !confirmedCompanionChange(w, r, "卸载") {
		return
	}
	if s.d.Companion == nil {
		writeErr(w, http.StatusServiceUnavailable, "本版本未包含插件管理，请使用完整软件包")
		return
	}
	ctx, cancel := ctxTimeout(r, 30*time.Second)
	defer cancel()
	status, err := s.d.Companion.Uninstall(ctx)
	if err != nil {
		writeErr(w, http.StatusConflict, "未完成卸载；配置归属无法确认、已被编辑或有并发修改时不会覆盖，请手动检查")
		return
	}
	s.invalidateNativeConnection()
	writeJSON(w, map[string]any{"status": status, "desktopControl": false, "remoteSend": false})
}

func confirmedCompanionChange(w http.ResponseWriter, r *http.Request, action string) bool {
	var in struct {
		Confirmed bool `json:"confirmed"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, action+"请求无效")
		return false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeErr(w, http.StatusBadRequest, action+"请求无效")
		return false
	}
	if !in.Confirmed {
		writeErr(w, http.StatusBadRequest, "请先明确确认“"+action+"”操作")
		return false
	}
	return true
}
