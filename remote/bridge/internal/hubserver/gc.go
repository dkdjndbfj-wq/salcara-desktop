package hubserver

import (
	"context"
	"time"
)

// RunGC runs background garbage collection.  Call it in a goroutine after New().
// It removes devices that have not re-registered for deviceTTL, and expired
// pending QR tokens.
func (h *Hub) RunGC(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.gc()
		}
	}
}

func (h *Hub) gc() {
	now := time.Now()

	h.mu.Lock()
	defer h.mu.Unlock()

	for id, d := range h.devices {
		d.mu.Lock()
		stale := now.Sub(d.lastSeen) > deviceTTL
		// Prune expired QR tokens.
		for tok, exp := range d.pendingTokens {
			if now.After(exp) {
				delete(d.pendingTokens, tok)
			}
		}
		d.mu.Unlock()

		if stale {
			delete(h.devices, id)
			h.o.Logger.Printf("gc: dropped stale device %s", id)
		}
	}
}

// DeviceCount returns the number of currently registered devices.
func (h *Hub) DeviceCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.devices)
}

// PhoneCount returns the number of open phone SSE connections.
func (h *Hub) PhoneCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.phones)
}
