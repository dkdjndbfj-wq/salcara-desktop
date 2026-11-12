package console

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"runtime"
	"sort"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/hubclient"
	"salcara/bridge/internal/protocol"
)

// AccountSourceRevision is intentionally process-bound. This private, keyed
// comparison additionally detects changed credentials after a restart without
// persisting a plain API-key digest or treating an old public revision as live.
func enrollmentSourceProof(account config.LocalAccount, secret string) string {
	hubs := append([]string(nil), account.RemoteHubURLs...)
	sort.Strings(hubs)
	raw, _ := json.Marshal([]any{config.AccountFingerprint(account), account.RemoteHubBindingsSet, hubs})
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}

// Only a fresh attempt whose transport proved non-delivery may be reopened.
// Caller cancellation is intentionally irrelevant to this exact-state cleanup;
// source/connection changes and earlier uncertain submissions still fence it.
func clearUnsentEnrollmentAttempt(c *config.Config, before config.Config, accountID string, sourceRevision *string, attempt config.RemoteEnrollmentAttempt) error {
	account, ok := c.LocalAccount(accountID)
	if attempt.Submitted || !ok || attempt.AccountID != accountID || attempt.SourceProof == "" ||
		checkLocalSourceRevision(*c, account, sourceRevision) != nil ||
		c.RemoteConnectionEpoch != before.RemoteConnectionEpoch || c.GatewayRuntimeKey != before.GatewayRuntimeKey || config.RemoteIdentity(*c) != config.RemoteIdentity(before) ||
		enrollmentSourceProof(account, attempt.Connection.DeviceSecret) != attempt.SourceProof {
		return errLocalSourceChanged
	}
	for i := range c.RemoteEnrollmentAttempts {
		pending := &c.RemoteEnrollmentAttempts[i]
		left, right := pending.Connection, attempt.Connection
		if pending.RequestID != attempt.RequestID || pending.AccountID != accountID || pending.SourceProof != attempt.SourceProof ||
			left.HubURL != right.HubURL || left.DeviceID != right.DeviceID || left.DeviceSecret != right.DeviceSecret ||
			left.HubInstanceID != right.HubInstanceID || left.IdentityPublicKey != right.IdentityPublicKey || left.IngressURL != right.IngressURL {
			continue
		}
		pending.Submitted = false
		return nil
	}
	return errLocalSourceChanged
}

func (s *Server) handleRemoteAccountDiscover(w http.ResponseWriter, r *http.Request, in remoteDiscoverInput) {
	if !s.remoteEnrollmentMu.TryLock() {
		writeRemoteDiscoveryError(w, 409, "enrollment_in_progress", errors.New("正在检查电脑登记，请等待当前检查结束"), hubclient.Discovery{})
		return
	}
	defer s.remoteEnrollmentMu.Unlock()
	if in.RetryEnrollment && !in.Connect {
		writeErr(w, 400, "重新登记需要明确的连接操作")
		return
	}
	before := s.d.Store.Get()
	account, exists := before.LocalAccount(in.AccountID)
	if !exists || in.ExpectedSourceRevision == nil || checkLocalSourceRevision(before, account, in.ExpectedSourceRevision) != nil {
		writeRemoteDiscoveryError(w, 409, "source_changed", errLocalSourceChanged, hubclient.Discovery{})
		return
	}
	base, baseErr := config.NormalizeStationAPIBase(account.BaseURL)
	inputBase, inputErr := config.NormalizeStationAPIBase(in.URL)
	if baseErr != nil || inputErr != nil || base != inputBase || account.SourceType != "api_key" || account.CredentialRef != "" || account.Key == "" || in.EnrollmentClaim != "" || in.Reenroll || in.RequireOfficial {
		writeRemoteDiscoveryError(w, 400, "source_changed", errors.New("所选 API 与连接地址不一致，请重新选择 API"), hubclient.Discovery{})
		return
	}
	if _, err := remoteDiscoveryTarget(base, in.HubURL); err != nil {
		writeRemoteDiscoveryError(w, 400, "source_changed", err, hubclient.Discovery{})
		return
	}
	s.remoteModeMu.Lock()
	modeSeq := s.remoteModeSeq
	s.remoteModeMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	sourceValid := func(c config.Config) error {
		current, ok := c.LocalAccount(in.AccountID)
		if !ok || checkLocalSourceRevision(c, current, in.ExpectedSourceRevision) != nil || c.RemoteConnectionEpoch != before.RemoteConnectionEpoch || c.GatewayRuntimeKey != before.GatewayRuntimeKey || config.RemoteIdentity(c) != config.RemoteIdentity(before) {
			return errLocalSourceChanged
		}
		return nil
	}
	valid := func(c config.Config) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return sourceValid(c)
	}
	current := func() error { return valid(s.d.Store.Get()) }
	fail := func(err error, info hubclient.Discovery) {
		code, status := hubclient.RemoteErrorCode(err), http.StatusBadGateway
		var remote *hubclient.RemoteHTTPError
		if errors.As(err, &remote) {
			status = remote.StatusCode
		}
		if errors.Is(err, hubclient.ErrHubIdentityChanged) {
			status, code = 409, "hub_identity_changed"
		} else if errors.Is(err, errLocalSourceChanged) || sourceValid(s.d.Store.Get()) != nil {
			status, code = 409, "source_changed"
		}
		writeRemoteDiscoveryError(w, status, code, err, info)
	}
	var info hubclient.Discovery
	var saved *config.RemoteConnection
	var err error
	scoped := false
	lookup := in.HubURL
	if lookup == "" {
		lookup, err = accountRemoteDiscoveryTarget(before, account)
	}
	if err != nil {
		fail(err, info)
		return
	}
	for _, candidate := range before.RemoteConnections {
		if candidate.BotGroupID == "" && config.NormalizeHubURL(candidate.HubURL) == config.NormalizeHubURL(lookup) && (candidate.HubInstanceID != "" || candidate.IdentityPublicKey != "") {
			copy := candidate
			saved = &copy
			break
		}
	}
	if saved != nil {
		// Existing identity recovery must not be blocked by a dead logical URL
		// before the cached signed routing hints have had a chance to refresh.
		if s.d.Hub != nil {
			info, err = s.d.Hub.DiscoverForConnection(ctx, before, *saved)
		} else {
			info, err = hubclient.DiscoverForConnection(ctx, before, *saved)
		}
		scoped = err == nil
	} else {
		info, err = hubclient.Discover(ctx, base, in.HubURL)
		if err == nil {
			info, saved, err = hubclient.ResolveDiscoveryConnection(ctx, info, before)
		}
	}
	if err != nil {
		fail(err, info)
		return
	}
	registered := false
	method := "api"
	if saved != nil {
		if !scoped {
			info, err = hubclient.DiscoverDevicePolicy(ctx, info, saved.DeviceID, saved.DeviceSecret, "")
		}
		if err != nil {
			fail(err, info)
			return
		}
		registered, method = info.HasCapability("discovery.policy.v1"), "device"
	} else if err = hubclient.CheckDeviceEnrollment(info, account); err != nil {
		fail(err, info)
		return
	}
	if err = current(); err != nil {
		fail(err, info)
		return
	}
	if !in.Connect {
		writeJSON(w, map[string]any{"ok": true, "connected": false, "registered": registered, "stage": "discovered", "plugin": info, "eligibility": map[string]any{"supported": true, "method": method}})
		return
	}
	device := protocol.Device{Name: before.DeviceName, OS: runtime.GOOS, Version: s.d.Version, Tools: []protocol.Tool{}, Projects: before.Projects}
	var attempt config.RemoteEnrollmentAttempt
	retryAuthorized := false
	if saved != nil {
		attempt.Connection = *saved
		device.DeviceID = saved.DeviceID
		if !registered {
			err = hubclient.RegisterRemoteDevice(ctx, info, device, saved.DeviceSecret, "")
		}
	} else {
		// Save a pending identity before any request that could register it.
		// It is not a saved/active connection, and contains no short-lived grant.
		err = s.d.Store.Update(func(c *config.Config) error {
			if err := valid(*c); err != nil {
				return err
			}
			for i, pending := range c.RemoteEnrollmentAttempts {
				if pending.Connection.HubURL == info.HubURL && pending.Connection.HubInstanceID == info.Identity.HubInstanceID && pending.Connection.IdentityPublicKey == info.Identity.IdentityPublicKey {
					if pending.AccountID != account.ID {
						return errLocalSourceChanged
					}
					proof := enrollmentSourceProof(account, pending.Connection.DeviceSecret)
					if pending.SourceProof != proof {
						if pending.Submitted {
							return errLocalSourceChanged
						}
						// No registration was sent, or a definitive refusal was
						// received. Correcting this same API card can safely reuse
						// its prepared identity; an unknown submission cannot.
						pending.RequestID, pending.SourceRevision, pending.SourceProof = config.NewUUID(), *in.ExpectedSourceRevision, proof
						c.RemoteEnrollmentAttempts[i] = pending
					}
					attempt = pending
					return nil
				}
			}
			if in.RetryEnrollment {
				return errors.New("没有待确认的登记，请先检查连接")
			}
			if len(c.RemoteEnrollmentAttempts) >= 8 {
				return errors.New("待确认的电脑登记较多，请先检查已有登记结果")
			}
			attempt = config.RemoteEnrollmentAttempt{AccountID: account.ID, RequestID: config.NewUUID(), Connection: config.RemoteConnection{HubURL: info.HubURL, DeviceID: config.NewUUID(), DeviceSecret: config.RandomToken(32), HubInstanceID: info.Identity.HubInstanceID, IdentityPublicKey: info.Identity.IdentityPublicKey, IngressURL: info.HubURL + "/v1"}}
			attempt.SourceRevision = *in.ExpectedSourceRevision
			attempt.SourceProof = enrollmentSourceProof(account, attempt.Connection.DeviceSecret)
			c.RemoteEnrollmentAttempts = append(c.RemoteEnrollmentAttempts, attempt)
			return nil
		})
		if err == nil {
			device.DeviceID = attempt.Connection.DeviceID
			if attempt.Submitted {
				// An unknown mutation is never automatically issued again. Reading
				// current device policy recovers a commit whose reply was lost.
				info, err = hubclient.DiscoverDevicePolicy(ctx, info, device.DeviceID, attempt.Connection.DeviceSecret, "")
				var policyFailure *hubclient.RemoteHTTPError
				if in.RetryEnrollment && errors.As(err, &policyFailure) && policyFailure.Code == "device_credentials_invalid" {
					// This error does not prove absence. Only the Hub's next signed
					// challenge (under its registration lock) may positively permit
					// another admission. Keep the original device ID and secret.
					previousRequest := attempt.RequestID
					err = s.d.Store.Update(func(c *config.Config) error {
						if err := valid(*c); err != nil {
							return err
						}
						for i := range c.RemoteEnrollmentAttempts {
							pending := &c.RemoteEnrollmentAttempts[i]
							if pending.RequestID == previousRequest && pending.Submitted && pending.SourceProof == enrollmentSourceProof(account, pending.Connection.DeviceSecret) {
								pending.RequestID, pending.SourceRevision = config.NewUUID(), *in.ExpectedSourceRevision
								attempt = *pending
								retryAuthorized = true
								return nil
							}
						}
						return errLocalSourceChanged
					})
				}
				if hubclient.RemoteErrorCode(err) == "device_credentials_invalid" || err == nil && !info.HasCapability("discovery.policy.v1") {
					err = &hubclient.RemoteHTTPError{StatusCode: 409, Code: "enrollment_result_unknown"}
				}
			}
			if err == nil && (!attempt.Submitted || retryAuthorized) {
				var grant string
				grant, err = hubclient.AcquireDeviceEnrollmentGrant(ctx, info, account, device.DeviceID, attempt.Connection.DeviceSecret, attempt.RequestID, current)
				if err == nil {
					err = s.d.Store.Update(func(c *config.Config) error {
						if err := valid(*c); err != nil {
							return err
						}
						for i := range c.RemoteEnrollmentAttempts {
							if c.RemoteEnrollmentAttempts[i].RequestID == attempt.RequestID && (!c.RemoteEnrollmentAttempts[i].Submitted || retryAuthorized) {
								c.RemoteEnrollmentAttempts[i].Submitted = true
								return nil
							}
						}
						return errLocalSourceChanged
					})
					if err == nil {
						err = hubclient.RegisterRemoteDevice(ctx, info, device, attempt.Connection.DeviceSecret, grant)
						var remote *hubclient.RemoteHTTPError
						if retryAuthorized && hubclient.RemoteErrorCode(err) == "enrollment_not_sent" {
							// This attempt also represents an earlier uncertain write.
							// Non-delivery of the latest retry cannot clear that fact.
							err = &hubclient.RemoteHTTPError{StatusCode: 409, Code: "enrollment_result_unknown"}
						} else if !retryAuthorized && hubclient.RemoteErrorCode(err) == "enrollment_not_sent" {
							// The transport proved no device mutation crossed its
							// boundary. Caller expiry must not strand a fresh attempt;
							// exact identity/source CAS still fences newer state.
							if clearErr := s.d.Store.Update(func(c *config.Config) error {
								return clearUnsentEnrollmentAttempt(c, before, in.AccountID, in.ExpectedSourceRevision, attempt)
							}); clearErr != nil {
								err = clearErr
							}
						} else if !retryAuthorized && errors.As(err, &remote) && remote.StatusCode >= 400 && remote.StatusCode < 500 && remote.Code != "enrollment_result_unknown" {
							// A definitive refusal permits another explicit check with
							// this same device. Unknown writes stay submitted.
							_ = s.d.Store.Update(func(c *config.Config) error {
								if err := valid(*c); err != nil {
									return err
								}
								for i := range c.RemoteEnrollmentAttempts {
									if c.RemoteEnrollmentAttempts[i].RequestID == attempt.RequestID {
										c.RemoteEnrollmentAttempts[i].Submitted = false
									}
								}
								return nil
							})
						}
					}
					grant = ""
				}
			}
		}
	}
	if err != nil {
		fail(err, info)
		return
	}
	s.remoteModeMu.Lock()
	defer s.remoteModeMu.Unlock()
	if modeSeq != s.remoteModeSeq {
		fail(errLocalSourceChanged, info)
		return
	}
	err = s.d.Store.Update(func(c *config.Config) error {
		if err := valid(*c); err != nil {
			return err
		}
		if saved == nil {
			c.RemoteConnections = append(c.RemoteConnections, attempt.Connection)
			pending := c.RemoteEnrollmentAttempts[:0]
			for _, item := range c.RemoteEnrollmentAttempts {
				if item.RequestID != attempt.RequestID {
					pending = append(pending, item)
				}
			}
			c.RemoteEnrollmentAttempts = pending
		} else {
			currentSaved, exists := c.SavedRemoteConnection(saved.HubURL, saved.DeviceID)
			if !exists || currentSaved.DeviceSecret != saved.DeviceSecret || currentSaved.HubInstanceID != saved.HubInstanceID || currentSaved.IdentityPublicKey != saved.IdentityPublicKey || currentSaved.IngressURL != saved.IngressURL {
				return errLocalSourceChanged
			}
			if info.Identity != nil {
				for i := range c.RemoteConnections {
					if c.RemoteConnections[i].DeviceID == saved.DeviceID && c.RemoteConnections[i].HubURL == saved.HubURL {
						c.RemoteConnections[i].HubInstanceID, c.RemoteConnections[i].IdentityPublicKey = info.Identity.HubInstanceID, info.Identity.IdentityPublicKey
						c.RemoteConnections[i].IngressURL = info.HubURL + "/v1"
					}
				}
			}
		}
		if err := c.RestoreRemoteConnection(info.Root, attempt.Connection.HubURL, attempt.Connection.DeviceID); err != nil {
			return err
		}
		if info.RemoteAccess != nil {
			c.RecordRemoteStationObservation(info.RemoteAccess)
		}
		return nil
	})
	if err != nil {
		fail(err, info)
		return
	}
	s.finishRemoteModeSwitch()
	writeJSON(w, map[string]any{"ok": true, "plugin": info, "connected": true, "registered": true, "stage": "waiting_online", "registration": map[string]string{"hubUrl": attempt.Connection.HubURL, "deviceId": attempt.Connection.DeviceID}, "eligibility": map[string]any{"supported": true, "method": method}})
}
