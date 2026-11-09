package console

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"salcara/bridge/internal/launcher"
)

// Opt-in streaming keeps the original JSON endpoint and authorization middleware
// intact. The mutation is executed exactly once; a lost stream is never retried.
func launchProgressEndpoint(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept"), "application/x-ndjson") {
			next(w, r)
			return
		}
		if _, ok := w.(http.Flusher); !ok {
			next(w, r)
			return
		}
		// HTTP/1 flushes may drain an unread request body. Buffer the small
		// configuration POST before emitting progress, with the same 1 MiB bound
		// as readJSON, so the real handler still receives the exact input.
		if r.Body != nil {
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
			_ = r.Body.Close()
			if err != nil {
				writeErr(w, http.StatusBadRequest, "请求格式有误或内容过大")
				return
			}
			r = r.Clone(r.Context())
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		var mu sync.Mutex
		closed := false
		sequence := 0
		progressContext := r.Context()
		cancelDelivery := func() {}
		if r.URL.Path == "/api/local/agent-change/v1" || r.URL.Path == "/api/local/codex-api/launch/safe-v1" {
			progressContext, cancelDelivery = context.WithCancel(progressContext)
		}
		defer cancelDelivery()
		controller := http.NewResponseController(w)
		emit := func(value any) {
			mu.Lock()
			defer mu.Unlock()
			if closed {
				return
			}
			sequence++
			// A stopped reader cannot hold a launcher mutex indefinitely.
			_ = controller.SetWriteDeadline(time.Now().Add(2 * time.Second))
			err := json.NewEncoder(w).Encode(struct {
				Sequence int `json:"sequence"`
				Event    any `json:"event"`
			}{sequence, value})
			if err == nil {
				err = controller.Flush()
			}
			_ = controller.SetWriteDeadline(time.Time{})
			if err != nil {
				closed = true
				cancelDelivery()
			}
		}
		ctx := launcher.WithLaunchProgress(progressContext, func(event launcher.LaunchProgress) { emit(event) })
		launcher.LaunchPhase(ctx, "source", "核对账号与启动配置")
		result := &launchResponse{header: make(http.Header), status: http.StatusOK}
		next(result, r.WithContext(ctx))
		var data any
		if json.Unmarshal(result.body.Bytes(), &data) != nil {
			result.status = http.StatusInternalServerError
			data = map[string]string{"error": "启动响应不完整"}
		}
		emit(struct {
			Type   string `json:"type"`
			Status int    `json:"status"`
			Data   any    `json:"data"`
		}{"result", result.status, data})
	}
}

type launchResponse struct {
	header      http.Header
	status      int
	wroteHeader bool
	body        bytes.Buffer
}

func (w *launchResponse) Header() http.Header { return w.header }
func (w *launchResponse) WriteHeader(status int) {
	if !w.wroteHeader {
		w.status, w.wroteHeader = status, true
	}
}
func (w *launchResponse) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.body.Write(b)
}
