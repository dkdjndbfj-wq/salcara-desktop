package config

import "encoding/hex"

// RevokePhoneBinding closes exactly the observed phone generation, including
// its running gateway workers. Unlike an ordinary configuration update, a
// confirmed revocation must remain effective in memory if the disk write fails.
// The dirty flag and conditional Hub cleanup survive subsequent failed writes;
// the next successful config save persists both with the retired binding.
// The boolean reports whether the expected generation was still current.
func (s *Store) RevokePhoneBinding(bindingID, phoneHash string) (bool, error) {
	s.mu.Lock()
	if s.cfg.PhoneBindingID != bindingID || s.cfg.PhoneHash != phoneHash {
		s.mu.Unlock()
		return false, nil
	}
	old := s.cfg.Clone()
	if validRevokedPhoneIdentity(bindingID) && validRevokedPhoneIdentity(phoneHash) {
		stations := append([]RemoteConnection{{HubURL: old.EffectiveHubURL(), DeviceID: old.DeviceID}}, old.RemoteConnections...)
		for _, station := range stations {
			item := PendingPhoneRevoke{HubURL: NormalizeHubURL(station.HubURL), DeviceID: station.DeviceID, BindingID: bindingID, PhoneHash: phoneHash}
			if item.HubURL == "" || item.DeviceID == "" {
				continue
			}
			found := false
			for _, existing := range s.cfg.PendingPhoneRevokes {
				if existing == item {
					found = true
					break
				}
			}
			if !found {
				s.cfg.PendingPhoneRevokes = append(s.cfg.PendingPhoneRevokes, item)
			}
		}
	}
	s.cfg.PhoneHash, s.cfg.PhonePairPending, s.cfg.PhoneBindingID = "", "", RandomToken(32)
	s.cfg.PhonePairAttempt, s.cfg.PhonePairExpires = "", 0
	s.cfg.RemoteStationAuthorization = nil
	fenceGatewayAuthorization(old, &s.cfg)
	s.phoneRevocationPending = true
	err := s.saveLocked()
	cur := s.cfg.Clone()
	subs := append([]func(old, new Config){}, s.subs...)
	s.mu.Unlock()
	for _, f := range subs {
		f(old, cur)
	}
	return true, err
}

func validRevokedPhoneIdentity(value string) bool {
	b, err := hex.DecodeString(value)
	return len(value) == 64 && err == nil && hex.EncodeToString(b) == value
}

// PhoneRevocationPending also covers a legacy/unclaimed pair, which has no
// conditional Hub cleanup entries but still requires a durable local revoke.
func (s *Store) PhoneRevocationPending() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.phoneRevocationPending
}

// RetryPhoneRevocationPersistence saves the current configuration under the
// same lock as updates; it cannot restore an old snapshot over a newer pair.
func (s *Store) RetryPhoneRevocationPersistence() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.phoneRevocationPending {
		return nil
	}
	return s.saveLocked()
}
