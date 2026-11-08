// header_inject.go — injects per-account ExtraHeaders into outgoing requests.
//
// Drop this file into internal/gateway/ alongside gateway.go.
//
// ExtraHeaders on a LocalAccount are static key→value pairs the operator wants
// sent on every request forwarded to that account's upstream endpoint. Common
// uses:
//   - X-Custom-Org, x-api-org: channel-specific routing
//   - anthropic-beta: feature-flag headers for preview APIs
//   - x-forwarded-for: spoofed source IP for geo-unlock
//   - Any provider-specific auth supplement alongside the Bearer token
//
// These headers are injected AFTER the gateway has already set Authorization,
// Content-Type, and all structural headers. An ExtraHeader that matches a
// structural key (Authorization, Host, Content-Length) is silently skipped to
// prevent credential or routing corruption.
//
// Wiring:
//   In gateway.go's ServeHTTP, after the upstream http.Request is built and
//   before it is dispatched, call:
//       InjectAccountHeaders(upstreamReq, account)
//   where account is the config.LocalAccount selected for this request.

package gateway

import (
	"net/http"

	"salcara/bridge/internal/config"
)

// InjectAccountHeaders copies ExtraHeaders from the account into the outgoing
// upstream request. It is safe to call with a nil or empty ExtraHeaders map.
//
// Call this after building the upstream *http.Request and before sending it.
func InjectAccountHeaders(req *http.Request, account config.LocalAccount) {
	config.ApplyAccountHeaders(req, account)
}

// MergedHeaders returns what the upstream request's headers will look like
// after injection, for logging or debugging purposes. It never modifies req.
func MergedHeaders(req *http.Request, account config.LocalAccount) http.Header {
	if req == nil {
		return http.Header{}
	}
	copy := &http.Request{Header: req.Header.Clone()}
	config.ApplyAccountHeaders(copy, account)
	return copy.Header
}
