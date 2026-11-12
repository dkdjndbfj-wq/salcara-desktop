package hubclient

import "time"

// Authenticated Hub reads/writes may first need a bounded 15s anonymous
// identity flight. Leave room for the original request without extending its
// caller's deadline or changing mutation retry rules.
const (
	hubAuthenticatedRequestTimeout = 25 * time.Second
	hubConnectionProbeTimeout      = 30 * time.Second
)
