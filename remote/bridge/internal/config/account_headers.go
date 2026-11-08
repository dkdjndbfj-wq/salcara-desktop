package config

import (
	"net/http"
	"strings"
)

// ApplyAccountHeaders gives model discovery and inference the same explicit
// channel settings while retaining the selected credential and HTTP framing.
func ApplyAccountHeaders(request *http.Request, account LocalAccount) {
	if request == nil {
		return
	}
	for name, value := range account.ExtraHeaders {
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		switch strings.ToLower(name) {
		case "", "authorization", "x-api-key", "host", "content-length", "transfer-encoding", "connection", "upgrade":
			continue
		}
		if value != "" {
			request.Header.Set(name, value)
		}
	}
}
