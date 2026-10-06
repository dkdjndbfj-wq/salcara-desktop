package hubclient

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"salcara/bridge/internal/config"
)

func validPhoneHash(s string) bool {
	b, err := hex.DecodeString(s)
	return len(s) == 64 && err == nil && hex.EncodeToString(b) == s
}

func phoneMatches(cfg config.Config, binding, phone string) bool {
	// The retired API-account transport keeps its compatibility behavior until
	// explicitly upgraded with a local QR. Device-pairing is fail-closed.
	if cfg.PhoneBindingID == "" {
		return !cfg.RemoteDeviceOnly
	}
	return validPhoneHash(phone) && cfg.PhoneHash != "" &&
		subtle.ConstantTimeCompare([]byte(binding), []byte(cfg.PhoneBindingID)) == 1 &&
		subtle.ConstantTimeCompare([]byte(phone), []byte(cfg.PhoneHash)) == 1
}

func eventIdentity(cfg config.Config) string {
	return config.RemoteIdentity(cfg) + ":" + cfg.PhoneBindingID + ":" + cfg.PhoneHash
}

func (c *Client) canUpload(cfg config.Config) bool {
	if cfg.PhoneBindingID == "" {
		return !cfg.RemoteDeviceOnly
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return cfg.PhoneHash != "" && c.uploadAuthorization == eventIdentity(cfg)
}

func (c *Client) currentUploadAuthorization() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.uploadAuthorization
}

// publishPairState updates the local pairing snapshot. `clearAwaiting` is
// reserved for a confirmed revoke (or a successful authorization); an
// unpaired/mismatched snapshot during a station handover is only an
// observation and must not release the queue retention gate before B has
// authenticated.
func (c *Client) publishPairState(pair PairStatus, authorized string, clearAwaiting bool) {
	c.mu.Lock()
	changed := c.status.Pairing == nil || *c.status.Pairing != pair
	c.status.Pairing, c.uploadAuthorization = &pair, authorized
	c.mu.Unlock()
	// A successful B authorization releases the queue retention gate. A
	// confirmed local/phone revoke also clears it; an unpaired snapshot whose
	// binding does not match this computer is not proof of revocation.
	if authorized != "" || clearAwaiting {
		c.handoverAwaitingAuth.Store(false)
	}
	if changed && c.o.OnStatus != nil {
		c.o.OnStatus(c.Status())
	}
}

func (c *Client) publishPair(pair PairStatus, authorized string) {
	c.publishPairState(pair, authorized, false)
}

func (c *Client) acceptPairState(station config.Config, data string) {
	c.phoneMu.Lock()
	defer c.phoneMu.Unlock()
	var wire struct {
		PairStatus
		BindingID string `json:"bindingId"`
		PhoneHash string `json:"phoneHash"`
		AttemptID string `json:"attemptId"`
	}
	if json.Unmarshal([]byte(data), &wire) != nil || wire.DeviceID != station.DeviceID || wire.Revision < 0 || wire.PendingExpiresAt < 0 {
		return
	}
	cfg := c.o.Store.Get()
	if config.RemoteIdentity(cfg) != config.RemoteIdentity(station) {
		return
	}
	// Pair revisions are scoped to the authenticated station stream. Ignore
	// delayed snapshots from before a newer claim/revoke; reset the baseline
	// when a stream reconnects because the Hub keeps revisions in memory.
	identity := config.RemoteIdentity(station)
	if c.pairRevisionIdentity != identity {
		c.pairRevisionIdentity, c.lastPairRevision = identity, -1
	}
	// The Hub may replay the same snapshot after a reconnect. Equal revisions
	// are duplicates, not a newer state; accepting one could let a delayed
	// unpaired copy revoke a freshly authorized handover. A legacy Hub may
	// report revision zero for a newly-created QR, so allow that one equal
	// snapshot only while the matching local attempt is still pending.
	equalPendingClaim := wire.Revision == c.lastPairRevision && wire.Paired && cfg.PhonePairPending == identity && cfg.PhonePairExpires > nowMS() && cfg.PhonePairAttempt != ""
	legacyZeroRevision := wire.Revision == 0 && wire.AttemptID == ""
	if wire.Revision < c.lastPairRevision || wire.Revision == c.lastPairRevision && !equalPendingClaim && !legacyZeroRevision {
		return
	}
	c.lastPairRevision = wire.Revision
	// Only a locally requested QR on the exact active station can establish
	// the physical phone identity. Replayed station snapshots cannot rebind it.
	legacyAttempt := wire.AttemptID == ""
	if wire.Paired && cfg.PhonePairPending == config.RemoteIdentity(station) && cfg.PhonePairExpires > nowMS() && cfg.PhonePairAttempt != "" && (legacyAttempt || wire.AttemptID == cfg.PhonePairAttempt) && wire.BindingID == cfg.PhoneBindingID && validPhoneHash(wire.PhoneHash) && (cfg.PhoneHash == "" || cfg.PhoneHash == wire.PhoneHash) {
		if err := c.o.Store.Update(func(next *config.Config) error {
			if next.PhoneBindingID != cfg.PhoneBindingID || config.RemoteIdentity(*next) != config.RemoteIdentity(station) {
				return errors.New("配对已更换")
			}
			next.PhoneHash, next.PhonePairPending = wire.PhoneHash, ""
			next.PhonePairAttempt, next.PhonePairExpires = "", 0
			return nil
		}); err != nil {
			wire.Paired = false
		} else {
			cfg = c.o.Store.Get()
		}
	}
	// A phone-side revoke at any connected station closes the global local
	// authorization. Keep the old hash on the Hub's unpaired snapshot so that
	// an unused/new station cannot accidentally revoke another station's pair.
	if !wire.Paired && phoneMatches(cfg, wire.BindingID, wire.PhoneHash) {
		if err := c.o.Store.Update(func(next *config.Config) error {
			if next.PhoneBindingID == cfg.PhoneBindingID {
				addPhoneRevokes(next, cfg)
				next.PhoneHash, next.PhonePairPending, next.PhoneBindingID = "", "", config.RandomToken(32)
				next.PhonePairAttempt, next.PhonePairExpires = "", 0
			}
			return nil
		}); err != nil {
			c.log.Printf("hub: cannot persist phone revocation")
		}
		// Even if persistence failed, do not continue uploading this connection.
		c.publishPairState(wire.PairStatus, "", true)
		c.wakePhoneRevoke()
		return
	}
	authorized := ""
	if wire.Paired && phoneMatches(cfg, wire.BindingID, wire.PhoneHash) {
		authorized = eventIdentity(cfg)
	} else {
		if wire.Paired && wire.PendingExpiresAt == 0 && validPhoneHash(wire.BindingID) && validPhoneHash(wire.PhoneHash) {
			// An old offline station is reconciled on reconnect. Conditional
			// revocation cannot delete a newly claimed pair that raced this read.
			_ = c.o.Store.Update(func(next *config.Config) error {
				addPhoneRevoke(next, config.PendingPhoneRevoke{HubURL: config.NormalizeHubURL(station.EffectiveHubURL()), DeviceID: station.DeviceID, BindingID: wire.BindingID, PhoneHash: wire.PhoneHash})
				return nil
			})
			c.wakePhoneRevoke()
		}
		wire.Paired = false
	}
	c.publishPairState(wire.PairStatus, authorized, false)
}

func (c *Client) revokePhone(ctx context.Context) error {
	c.phoneMu.Lock()
	cfg := c.o.Store.Get()
	// Keep the binding that the user explicitly revoked. Network cleanup may
	// finish after a new QR claim; sending the old binding metadata makes each
	// Hub reject that stale revoke instead of deleting the replacement pair.
	oldBindingID, oldPhoneHash := cfg.PhoneBindingID, cfg.PhoneHash
	// Close local access before any network request; an offline old station
	// must not retain execution/read access on this computer.
	if err := c.o.Store.Update(func(next *config.Config) error {
		addPhoneRevokes(next, cfg)
		next.PhoneHash, next.PhonePairPending, next.PhoneBindingID = "", "", config.RandomToken(32)
		next.PhonePairAttempt, next.PhonePairExpires = "", 0
		return nil
	}); err != nil {
		c.phoneMu.Unlock()
		return errors.New("无法保存解除绑定，请重试")
	}
	c.publishPairState(PairStatus{DeviceID: cfg.DeviceID}, "", true)
	c.phoneMu.Unlock()
	c.wakePhoneRevoke()
	if validPhoneHash(oldBindingID) && validPhoneHash(oldPhoneHash) {
		// A bounded best-effort pass gives an online Hub immediate feedback.
		// Remaining offline stations are durable and retried without holding
		// phoneMu, so this cannot block a new QR or another phone command.
		c.retryPhoneRevokes(ctx)
		return nil
	}
	// Compatibility for legacy/unclaimed pair attempts: perform a one-shot
	// cleanup only. An unconditional revoke must never be replayed later,
	// because it could delete a new phone pairing.
	c.phoneMu.Lock()
	defer c.phoneMu.Unlock()
	if c.o.Store.Get().PhoneHash != "" {
		// A new generation was paired while local cleanup was finishing. The
		// legacy request has no conditional identity, so skip it entirely.
		return nil
	}
	stations := append([]config.RemoteConnection(nil), cfg.RemoteConnections...)
	stations = append(stations, config.RemoteConnection{HubURL: cfg.EffectiveHubURL(), DeviceID: cfg.DeviceID, DeviceSecret: cfg.DeviceSecret})
	seen := map[string]bool{}
	for _, station := range stations {
		if seen[station.HubURL] || station.HubURL == "" {
			continue
		}
		seen[station.HubURL] = true
		target := cfg
		target.HubURL, target.DeviceID, target.DeviceSecret = station.HubURL, station.DeviceID, station.DeviceSecret
		rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		body := map[string]string{"deviceId": station.DeviceID}
		if oldBindingID != "" {
			body["bindingId"], body["phoneHash"] = oldBindingID, oldPhoneHash
		}
		if err := c.postFor(rctx, target, "/bridge/pair/revoke", body); err != nil {
			c.log.Printf("hub: station revoke deferred; local phone access is closed")
		}
		cancel()
	}
	return nil
}
