package console

import (
	"errors"
	"net/http"

	"salcara/bridge/internal/config"
)

func (s *Server) codexAPIImageRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/local/codex-api/images", s.handleCodexAPIImages)
	m.HandleFunc("POST /api/local/codex-api/images", s.handleCodexAPIImages)
}

func (s *Server) handleCodexAPIImages(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		writeErr(w, 400, "图片服务设置不接收查询参数")
		return
	}
	if r.Method == http.MethodPost {
		if !s.codexProfileMu.TryLock() {
			writeErr(w, http.StatusConflict, "API 服务或 Codex 配置正在更新")
			return
		}
		defer s.codexProfileMu.Unlock()
		if s.apiServiceClosed() {
			writeErr(w, http.StatusServiceUnavailable, "API 监听服务已关闭")
			return
		}
		var in struct {
			Mode        string `json:"mode"`
			Concurrency int    `json:"maxConcurrentRequests"`
		}
		if readReverseJSON(r, &in, 4096) != nil || config.ValidateCodexAPIImages(in.Mode, in.Concurrency) != nil {
			writeErr(w, 400, "请选择图片服务模式，并填写 1–16 的并发上限")
			return
		}
		before := s.d.Store.Get()
		epoch, ok := s.d.Store.RetireCodexAPIImagesRuntime(before.CodexAPI.AuthEpoch, in.Mode, in.Concurrency)
		if !ok {
			writeErr(w, 409, "API 授权已变化，请刷新后再试")
			return
		}
		if err := s.d.Store.Update(func(c *config.Config) error {
			if c.CodexAPI.AuthEpoch != epoch {
				return errCodexAPIRuntimeChanged
			}
			c.CodexAPI.ImagesMode, c.CodexAPI.MaxConcurrentImageRequests = in.Mode, in.Concurrency
			return nil
		}); err != nil {
			if errors.Is(err, errCodexAPIRuntimeChanged) {
				writeErr(w, 409, "API 授权已变化，请刷新后再试")
				return
			}
			writeErr(w, 500, "图片设置未保存；关闭和降低权限已在本次进程生效，新增权限尚未启用")
			return
		}
	}
	writeJSON(w, struct {
		config.CodexAPIImageSettings
		OfficialExecutorAvailable bool     `json:"officialExecutorAvailable"`
		Routes                    []string `json:"routes"`
	}{s.d.Store.CodexAPIImageSettings(), s.d.OfficialImages != nil, []string{"POST /v1/images/generations", "POST /v1/images/edits"}})
}
