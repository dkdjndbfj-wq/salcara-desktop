package hubclient

import (
	"context"

	"salcara/bridge/internal/desktopcompanion"
)

type nativeAdmission struct {
	identity string
	cancel   context.CancelFunc
}

// A phone disconnection is not a revocation. Keep normal delivery independent
// of its SSE connection, but cancel pending native requests when the actual
// pairing authorization changes. The final guard is carried through the
// companion's own asynchronous status check, including text-only sends.
func (c *Client) nativeAdmissionContext(ctx context.Context, cmd map[string]any) (context.Context, func()) {
	if _, remote := commandGuardFromContext(ctx); !remote || c.o.Store == nil {
		return ctx, func() {}
	}
	child, cancel := context.WithCancel(ctx)
	cfg := c.o.Store.Get()
	var pending *nativeAdmission
	// The retired API-account transport has no pairing publication. Preserve
	// that compatibility path while still enforcing the late command guard.
	if cfg.PhoneBindingID != "" {
		pending = &nativeAdmission{identity: eventIdentity(cfg), cancel: cancel}
		c.mu.Lock()
		if c.nativeAdmissions == nil {
			c.nativeAdmissions = make(map[*nativeAdmission]struct{})
		}
		c.nativeAdmissions[pending] = struct{}{}
		if pending.identity != c.uploadAuthorization {
			cancel()
		}
		c.mu.Unlock()
	}
	guarded := desktopcompanion.WithRemoteAdmissionGuard(child, func() error {
		if err := c.validateCommandGuard(child, str(cmd, "type")); err != nil {
			return err
		}
		if env, ok := executionSourceFromContext(child); ok {
			return c.validateExecutionSource(env)
		}
		return nil
	})
	return guarded, func() {
		cancel()
		if pending != nil {
			c.mu.Lock()
			delete(c.nativeAdmissions, pending)
			c.mu.Unlock()
		}
	}
}
