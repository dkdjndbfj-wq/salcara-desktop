package gateway

import (
	"net/http"
	"net/url"
)

// The native Anthropic SDK sends ?beta=true for Messages and token counting.
// Keep this exception specific: query strings must never select another route,
// model, credential, or provider, and duplicates/unknown fields stay rejected.
func claudeBetaQuery(r *http.Request, target, path string) bool {
	if r.Method != http.MethodPost || (path != "v1/messages" && path != "v1/messages/count_tokens") {
		return false
	}
	switch target {
	case "claude", "claude-desktop", "remote-claude", "direct-claude":
	default:
		return false
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	return err == nil && len(query) == 1 && len(query["beta"]) == 1 && query["beta"][0] == "true"
}
