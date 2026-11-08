package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"salcara/bridge/internal/config"
)

func addPhoneRevoke(next *config.Config, item config.PendingPhoneRevoke) {
	for _, existing := range next.PendingPhoneRevokes {
		if existing == item {
			return
		}
	}
	next.PendingPhoneRevokes = append(next.PendingPhoneRevokes, item)
}

func (c *Client) wakePhoneRevoke() {
	select {
	case c.revokeWake <- struct{}{}:
	default:
	}
}

// Only exact old generations are retried. A 409 means that the Hub already
// moved to another generation, which is safe to leave alone. Network failures,
// missing endpoints and auth errors remain on disk for a later attempt.
func (c *Client) tryPhoneRevoke(ctx context.Context, item config.PendingPhoneRevoke) bool {
	cfg := c.o.Store.Get()
	station, ok := cfg.SavedRemoteConnection(item.HubURL, item.DeviceID)
	if ok {
		cfg = activeStationConfig(cfg, station)
	} else if config.NormalizeHubURL(cfg.EffectiveHubURL()) != item.HubURL || cfg.DeviceID != item.DeviceID {
		return false
	}
	body, _ := json.Marshal(map[string]string{"deviceId": item.DeviceID, "bindingId": item.BindingID, "phoneHash": item.PhoneHash})
	req, err := c.requestFor(ctx, cfg, http.MethodPost, "/bridge/pair/revoke", bytes.NewReader(body), true)
	if err != nil {
		return false
	}
	resp, err := c.o.HTTP.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode/100 == 2 || resp.StatusCode == http.StatusConflict
}

// Keep each pass short and rotate unreachable entries to the tail. One offline
// station must not starve another station's cleanup, or block pairing/commands.
func (c *Client) retryPhoneRevokes(ctx context.Context) {
	if !c.revokeCleanupMu.TryLock() {
		return
	}
	defer c.revokeCleanupMu.Unlock()
	// A disk failure must not undo the in-memory revoke. Keep retrying even
	// for a legacy pair with no conditional Hub cleanup entries.
	if err := c.o.Store.RetryPhoneRevocationPersistence(); err != nil {
		c.log.Printf("hub: phone revocation persistence is still pending")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	items := c.o.Store.Get().PendingPhoneRevokes
	if len(items) > 8 {
		items = items[:8]
	}
	for _, item := range items {
		if ctx.Err() != nil {
			return
		}
		rctx, rcancel := context.WithTimeout(ctx, time.Second)
		resolved := c.tryPhoneRevoke(rctx, item)
		rcancel()
		// Do not remove a newer entry that was appended during the request.
		_ = c.o.Store.Update(func(next *config.Config) error {
			for i, existing := range next.PendingPhoneRevokes {
				if existing != item {
					continue
				}
				next.PendingPhoneRevokes = append(next.PendingPhoneRevokes[:i], next.PendingPhoneRevokes[i+1:]...)
				if !resolved {
					next.PendingPhoneRevokes = append(next.PendingPhoneRevokes, item)
				}
				break
			}
			return nil
		})
	}
}

// No timer/networking when there is no cleanup. Failed stations back off to
// five minutes; pending work is resumed after every desktop process restart.
func (c *Client) phoneRevokeLoop(ctx context.Context) {
	delay := time.Minute
	for ctx.Err() == nil {
		c.retryPhoneRevokes(ctx)
		var timer *time.Timer
		var tick <-chan time.Time
		if len(c.o.Store.Get().PendingPhoneRevokes) > 0 || c.o.Store.PhoneRevocationPending() {
			timer = time.NewTimer(delay)
			tick = timer.C
			delay *= 2
			if delay > 5*time.Minute {
				delay = 5 * time.Minute
			}
		} else {
			delay = time.Minute
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-c.revokeWake:
			delay = time.Minute
		case <-tick:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}
