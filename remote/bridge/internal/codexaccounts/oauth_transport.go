package codexaccounts

import (
	"net"
	"net/http"
	"time"
)

// OAuth credentials use a private transport, independent from environment
// proxies and mutable process-wide HTTP defaults. Test-only clients can still
// supply a loopback fixture transport explicitly.
func newOAuthHTTPClient() http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy: nil, DialContext: dialer.DialContext,
			ForceAttemptHTTP2: true, MaxIdleConns: 8,
			IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
	}
}
