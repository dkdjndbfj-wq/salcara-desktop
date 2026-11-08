package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"salcara/bridge/internal/protocol"
)

const maxProviderErrorBytes = 64 << 10
const capacityMessage = "Selected model is at capacity. Please try a different model."

// A rejected local admission is not a provider health observation. Likewise,
// one overloaded model does not establish that every model sharing this
// credential is unavailable. Preserve cooldowns for real credential/transport
// failures without making the advertised model-change action impossible.
func credentialCooldownError(err error) bool {
	var local interface{ LocalAdmissionStatus() int }
	if errors.As(err, &local) && local.LocalAdmissionStatus() >= 400 && local.LocalAdmissionStatus() <= 599 {
		return false
	}
	var classified interface{ ExecutionCode() string }
	if errors.As(err, &classified) && classified.ExecutionCode() == "model_capacity" {
		return false
	}
	return !errors.Is(err, context.Canceled)
}

func credentialCooldownCode(code string) bool { return code != "model_capacity" }

// Read only bounded, known provider error fields. Never copy a provider body,
// message, URL or credential into the downstream failure.
func providerExecutionCode(raw []byte) string {
	if len(raw) > maxProviderErrorBytes {
		return ""
	}
	type providerError struct {
		Message string `json:"message"`
		Code    string `json:"code"`
		Type    string `json:"type"`
	}
	var body struct {
		providerError
		Error    *providerError `json:"error"`
		Response *struct {
			Error *providerError `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return ""
	}
	errs := []*providerError{&body.providerError, body.Error}
	if body.Response != nil {
		errs = append(errs, body.Response.Error)
	}
	for _, e := range errs {
		if e == nil {
			continue
		}
		if e.Code == "model_capacity" || e.Code == "server_overloaded" || e.Type == "server_overloaded" || protocol.CapacityTextCode(e.Message) != "" {
			return "model_capacity"
		}
	}
	return ""
}

func failModelCapacity(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": capacityMessage, "type": "salcara_gateway_error", "code": "model_capacity"}})
}

// Official executors expose only a status and a whitelisted condition. Preserve
// those without making a refused provider request look like a network failure,
// and never inspect or forward the raw SDK error message here.
func failExecutorError(w http.ResponseWriter, err error) bool {
	var local interface{ LocalAdmissionStatus() int }
	if errors.As(err, &local) && local.LocalAdmissionStatus() == http.StatusServiceUnavailable {
		fail(w, http.StatusServiceUnavailable, "额度状态未知、已耗尽或保留额度不足，请刷新额度后再试；未更换账号")
		return true
	}
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() < 400 || status.StatusCode() > 599 {
		return false
	}
	var classified interface{ ExecutionCode() string }
	if errors.As(err, &classified) && classified.ExecutionCode() == "model_capacity" {
		failModelCapacity(w, status.StatusCode())
	} else {
		fail(w, status.StatusCode(), fmt.Sprintf("上游返回 HTTP %d，请检查这个账号的模型权限和接口；未更换模型", status.StatusCode()))
	}
	return true
}

// WriteExecutorError shares only a sanitized status and known condition with
// additional authenticated protocol handlers. Provider text remains private.
func WriteExecutorError(w http.ResponseWriter, err error) bool { return failExecutorError(w, err) }

func WriteProviderError(w http.ResponseWriter, status int, raw []byte) {
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	if providerExecutionCode(raw) == "model_capacity" {
		failModelCapacity(w, status)
	} else {
		fail(w, status, http.StatusText(status))
	}
}
