package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"salcara/bridge/internal/protocol"
)

const maxSideQuestionWireBody = 1 << 20
const maxSideQuestionResponse = 256 << 10

// Only this private error type can expose transport details to the phone. Its
// messages contain fixed text and a status, never a provider body or URL.
type sideQuestionGatewayError struct {
	status int
	kind   string
}

func (e *sideQuestionGatewayError) Error() string {
	switch e.kind {
	case "model_capacity":
		return "所选模型容量已满"
	case "timeout":
		return "独立提问请求超时，结果未确认"
	case "cancelled":
		return "独立提问已取消，结果未确认"
	case "connection":
		return "独立提问连接失败，结果未确认"
	case "interrupted":
		return "独立提问响应中断，结果未确认"
	case "oversized":
		return "独立提问返回内容超过限制"
	}
	switch e.status {
	case http.StatusUnauthorized:
		return "独立提问未获授权（HTTP 401）"
	case http.StatusForbidden:
		return "独立提问访问被拒绝（HTTP 403）"
	case http.StatusTooManyRequests:
		return "独立提问请求受限（HTTP 429）"
	}
	return fmt.Sprintf("独立提问请求失败（HTTP %d）", e.status)
}

func sideQuestionHTTPFailure(status int, raw []byte) error {
	failure := &sideQuestionGatewayError{status: status}
	// Status alone cannot distinguish model capacity from rate or quota limits.
	// Inspect bounded known fields only; the provider prose is never returned.
	if len(raw) > 64<<10 || status < 400 || status > 599 || status == http.StatusUnauthorized || status == http.StatusForbidden {
		return failure
	}
	var body struct {
		Error *struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &body) == nil && body.Error != nil {
		e := body.Error
		if e.Code == "model_capacity" || e.Code == "server_overloaded" || e.Type == "server_overloaded" || protocol.CapacityTextCode(e.Message) != "" {
			failure.kind = "model_capacity"
		}
	}
	return failure
}

// This helper is deliberately limited to exact, authenticated loopback tool
// routes. Providers must first prove that endpoint and credential belong to the
// selected immutable source. The gateway retains upstream safety/accounting.
func sideQuestionGatewayRequest(ctx context.Context, endpoint, credential string, body []byte) ([]byte, bool, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" {
		return nil, false, errSideQuestionInvalid
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || u.Host != "127.0.0.1:"+strconv.Itoa(port) || credential == "" {
		return nil, false, errSideQuestionInvalid
	}
	switch u.Path {
	case "/gateway/claude/v1/messages", "/gateway/remote-claude/v1/messages", "/gateway/direct-claude/v1/messages",
		"/gateway/codex/v1/responses", "/gateway/remote-codex/v1/responses", "/gateway/direct-codex/v1/responses":
	default:
		return nil, false, errSideQuestionInvalid
	}
	if len(body) == 0 || len(body) > maxSideQuestionWireBody {
		return nil, false, errSideQuestionLimit
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, false, errSideQuestionInvalid
	}
	request.GetBody = nil
	request.Header.Set("Authorization", "Bearer "+credential)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if strings.HasSuffix(u.Path, "/messages") {
		request.Header.Set("anthropic-version", "2023-06-01")
	}
	// No proxy environment, redirects, idempotency headers, persistent connection
	// retries or upstream URL fallback. Each explicit operation gets one Do call.
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, DisableKeepAlives: true, ResponseHeaderTimeout: sideQuestionTimeout}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: sideQuestionTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		kind := "connection"
		var network net.Error
		if errors.Is(ctx.Err(), context.Canceled) {
			kind = "cancelled"
		} else if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.As(err, &network) && network.Timeout() {
			kind = "timeout"
		}
		return nil, true, &sideQuestionGatewayError{kind: kind}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
		return nil, true, sideQuestionHTTPFailure(response.StatusCode, data)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxSideQuestionResponse+1))
	if err != nil {
		return nil, true, &sideQuestionGatewayError{kind: "interrupted"}
	}
	if len(data) > maxSideQuestionResponse {
		return nil, true, &sideQuestionGatewayError{kind: "oversized"}
	}
	return data, true, nil
}
