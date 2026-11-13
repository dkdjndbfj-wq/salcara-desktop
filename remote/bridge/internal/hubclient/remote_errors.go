package hubclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/hubingress"
)

// RemoteHTTPError describes only the remote transport's authorization/health.
// Its code and message come from a local allowlist, never a station's text.
type RemoteHTTPError struct {
	Code       string
	StatusCode int
	cause      error // local verified transport evidence, never remote JSON
}

func (e *RemoteHTTPError) Unwrap() error { return e.cause }

func sameClockSkew(a, b *hubingress.ClockSkewDetails) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func (e *RemoteHTTPError) Error() string {
	switch sourceRemoteErrorKind(e.Code) {
	case "authorization":
		return "本站未授权此 API 来源或当前来源凭证已变化，请在手机远程页检查来源配置；本机普通 API 调用不受影响"
	case "unavailable":
		return "本站暂时无法验证 API 来源，请稍后重试；无需重新登记电脑或手机"
	case "rate_limited":
		return "本站 API 来源验证请求较多，请稍后重试"
	case "pending":
		return "本站正在验证 API 来源，请稍后重试；无需重新登记电脑或手机"
	}
	switch e.Code {
	case "api_enrollment_unsupported":
		return "本站暂不支持通过 API 自动登记，请使用管理员提供的电脑登记授权"
	case "api_credentials_invalid":
		return "所选 API 已失效或过期，请检查 API 配置"
	case "api_enrollment_denied", "enrollment_issuer_untrusted":
		return "本站未授权此 API 登记电脑，请联系站点管理员"
	case "enrollment_grant_invalid", "enrollment_challenge_expired", "enrollment_request_conflict":
		return "电脑登记授权已变化或过期，请重新检查连接"
	case "api_verifier_unavailable":
		return "本站暂时无法验证 API，请稍后重试"
	case "enrollment_result_unknown":
		return "电脑登记结果尚未确认，请再次检查；不会重复创建电脑"
	case "enrollment_not_sent":
		return "电脑登记尚未发送，请检查连接后重试"
	case "device_credentials_invalid", "device_authorization_expired":
		return "这台电脑的设备登记已失效，请重新登记并绑定手机；本机 API、项目与对话保留"
	case "device_enrollment_required":
		return "这台电脑尚未登记，请使用本站设备登记码完成登记"
	case "device_credentials_required":
		return "远程连接需要设备凭据，请在手机远程页检查本站设备登记"
	case "enrollment_claim_invalid":
		return "设备登记码无效、已使用或已过期，请获取新的登记码"
	case "source_not_authorized", "source_policy_changed":
		return "本站未授权此 API 来源用于远程；本机普通 API 调用不受影响"
	case "remote_unsupported":
		return "这个中转站还没有部署兼容的远程编程服务 (404)"
	case "remote_unavailable":
		return "远程服务暂不可用，请检查网络或稍后重试；本机普通 API 调用不受影响"
	case "remote_authorization_denied":
		return "远程连接授权被拒绝，请检查本站远程连接凭据"
	case "pair_credentials_invalid":
		return "手机配对凭据已失效，请在手机远程页重新绑定手机"
	case "user_disabled":
		return "本站已停用此远程用户，请联系本站管理员；本机普通 API 配置保留"
	case "ambiguous_credentials", "invalid_request":
		return "远程连接凭据或请求配置不兼容，请检查本站连接配置或更新客户端"
	case "remote_rate_limited":
		return "远程服务请求过于频繁，请稍后重试"
	default:
		return fmt.Sprintf("远程服务返回 %d", e.StatusCode)
	}
}

func (e *RemoteHTTPError) Is(target error) bool {
	return target == errUnauthorized && (e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden)
}

func RemoteErrorCode(err error) string {
	var remote *RemoteHTTPError
	if errors.As(err, &remote) {
		return remote.Code
	}
	if hubingress.ClockSkew(err) != nil {
		return "clock_skew"
	}
	return ""
}

func knownRemoteErrorCode(code string) bool {
	if sourceRemoteErrorKind(code) != "" {
		return true
	}
	switch code {
	case "device_credentials_invalid", "device_authorization_expired", "device_credentials_required", "device_enrollment_required", "enrollment_claim_invalid", "pair_credentials_invalid", "user_disabled", "ambiguous_credentials", "invalid_request", "source_not_authorized", "source_policy_changed", "remote_unavailable", "api_credentials_invalid", "api_enrollment_denied", "enrollment_issuer_untrusted", "enrollment_grant_invalid", "enrollment_challenge_expired", "enrollment_request_conflict", "api_verifier_unavailable", "api_enrollment_unsupported", "enrollment_result_unknown":
		return true
	}
	return false
}

func sourceRemoteErrorKind(code string) string {
	switch code {
	case "source_not_authorized", "source_policy_changed", "source_agent_denied", "source_agent_mismatch", "source_binding_mismatch", "source_challenge_expired", "source_challenge_invalid", "source_challenge_mismatch", "source_command_invalid", "source_command_mismatch", "source_command_unsupported", "source_control_surface_mismatch", "source_credential_mismatch", "source_grant_expired", "source_grant_invalid", "source_grant_required", "source_guard_unsupported", "source_issuer_denied", "source_metadata_required", "source_model_mismatch", "source_policy_stale", "source_scope_denied", "source_station_denied", "source_acquisition_unsupported":
		return "authorization"
	case "source_policy_unavailable", "source_challenge_unavailable":
		return "unavailable"
	case "source_challenge_capacity":
		return "rate_limited"
	case "source_acquisition_pending":
		return "pending"
	}
	return ""
}

func checkResp(resp *http.Response) error {
	if resp.StatusCode/100 == 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	// A misconfigured server can echo credentials or private commands. Inspect
	// only an allowlisted machine code in a bounded, complete JSON envelope.
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4097))
	var wire struct {
		Code string `json:"code"`
	}
	code := ""
	if readErr == nil && len(body) <= 4096 && json.Unmarshal(body, &wire) == nil && knownRemoteErrorCode(wire.Code) {
		code = wire.Code
	}
	if code == "" {
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			code = "remote_authorization_denied"
			if req := resp.Request; req != nil && req.Header.Get("Authorization") == "" && req.Header.Get("X-Salcara-Device-Id") != "" {
				code = "device_credentials_invalid"
				if req.URL != nil && strings.HasSuffix(req.URL.Path, "/device/register") && req.Header.Get("X-Salcara-Enrollment-Claim") != "" {
					code = "enrollment_claim_invalid"
				}
			}
		case http.StatusLocked:
			code = "user_disabled"
		case http.StatusTooManyRequests:
			code = "remote_rate_limited"
		case http.StatusNotFound:
			code = "remote_unsupported"
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			code = "remote_unavailable"
		default:
			code = "remote_request_failed"
		}
	}
	return &RemoteHTTPError{Code: code, StatusCode: resp.StatusCode}
}

func remoteFailure(err error, cfg config.Config) (state, code, message string) {
	if hubingress.ClockSkew(err) != nil {
		return StateConnecting, "clock_skew", (&hubingress.ClockSkewError{}).Error()
	}
	if errors.Is(err, ErrHubIdentityChanged) {
		return StateDeviceAuthRequired, "hub_identity_changed", ErrHubIdentityChanged.Error()
	}
	if errors.Is(err, ErrHubIdentityVerification) {
		return StateConnecting, "remote_unavailable", ErrHubIdentityVerification.Error()
	}
	code = RemoteErrorCode(err)
	if cfg.RemoteDeviceOnly && code == "remote_authorization_denied" {
		code = "device_credentials_invalid"
	}
	switch code {
	case "device_credentials_invalid", "device_authorization_expired", "device_credentials_required", "device_enrollment_required", "enrollment_claim_invalid":
		state = StateDeviceAuthRequired
	case "pair_credentials_invalid":
		state = StatePairRequired
	case "user_disabled", "ambiguous_credentials", "invalid_request":
		state = StateRemoteBlocked
	case "source_not_authorized", "source_policy_changed":
		state = StateSourceUnauthorized
	case "remote_authorization_denied":
		state = StateInvalidKey // legacy wire compatibility; this is remote auth.
	default:
		state = StateConnecting
		if sourceRemoteErrorKind(code) == "authorization" {
			state = StateSourceUnauthorized
		}
	}
	if code != "" {
		var remote *RemoteHTTPError
		status := 0
		if errors.As(err, &remote) {
			status = remote.StatusCode
		}
		message = (&RemoteHTTPError{Code: code, StatusCode: status}).Error()
	} else if errors.Is(err, errHeartbeat) {
		code, message = "remote_unavailable", errHeartbeat.Error()
	} else {
		code, message = "remote_unavailable", (&RemoteHTTPError{Code: "remote_unavailable"}).Error()
	}
	return
}
