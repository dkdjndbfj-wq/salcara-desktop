package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"salcara/bridge/internal/protocol"
)

// StationObservation is public discovery metadata for the picker. It is never
// execution authority and survives restart only as a display hint.
type StationObservation struct {
	Mode      string                  `json:"mode"`
	Revision  string                  `json:"revision"`
	Station   *protocol.StationPolicy `json:"station,omitempty"`
	CheckedAt int64                   `json:"checkedAt"`
}

type StationAuthorization struct {
	Identity string
	Revision string
	Station  protocol.StationPolicy
}

func CloneStationPolicy(p *protocol.StationPolicy) *protocol.StationPolicy {
	if p == nil {
		return nil
	}
	out := *p
	out.APIBases = append([]string{}, p.APIBases...)
	if p.ExternalAPIBases != nil {
		out.ExternalAPIBases = append([]string{}, p.ExternalAPIBases...)
	}
	if p.ReverseProxyAPIBases != nil {
		out.ReverseProxyAPIBases = append([]string{}, p.ReverseProxyAPIBases...)
	}
	return &out
}

// StationPolicyEqual compares two StationPolicy values field-by-field.
// This replaces reflect.DeepEqual(CloneStationPolicy(a), CloneStationPolicy(b))
// on the per-command hot path: avoids two allocations and a reflect traversal.
func StationPolicyEqual(a, b *protocol.StationPolicy) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if a.Version != b.Version || a.AllowOfficialAccounts != b.AllowOfficialAccounts || a.AllowAllAccounts != b.AllowAllAccounts {
		return false
	}
	for i, left := range [][]string{a.APIBases, a.ExternalAPIBases, a.ReverseProxyAPIBases} {
		right := [][]string{b.APIBases, b.ExternalAPIBases, b.ReverseProxyAPIBases}[i]
		if len(left) != len(right) {
			return false
		}
		for j := range left {
			if left[j] != right[j] {
				return false
			}
		}
	}
	return true
}

// stationAuthorizationEqual compares two StationAuthorization pointers
// field-by-field. This replaces reflect.DeepEqual on the per-command hot
// path (BindRemoteStation runs under taskConfigMu on every sourced command).
func stationAuthorizationEqual(a, b *StationAuthorization) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Identity == b.Identity && a.Revision == b.Revision && StationPolicyEqual(&a.Station, &b.Station)
}

func CloneStationObservation(p *StationObservation) *StationObservation {
	if p == nil {
		return nil
	}
	out := *p
	out.Station = CloneStationPolicy(p.Station)
	return &out
}

// StationObservationIsNewer orders display observations from independent probe
// sources. A timestamp beyond the current local clock cannot win this tie-break
// after the user corrects that clock backwards. Identity and probe-sequence
// fences must still be checked by the caller; this does not grant authority.
func StationObservationIsNewer(current, incoming *StationObservation, now int64) bool {
	return current != nil && incoming != nil && current.CheckedAt <= now && current.CheckedAt > incoming.CheckedAt
}

// NormalizeStationAPIBase compares the full API root, preserving path case.
// Encoded/ambiguous separators and dot segments are rejected, never collapsed.
func NormalizeStationAPIBase(raw string) (string, error) {
	fail := errors.New("中转站 API 地址无效")
	if len(raw) > 4096 || raw != strings.TrimSpace(raw) || strings.ContainsAny(raw, "%\\#\r\n\x00\t ") {
		return "", fail
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" {
		return "", fail
	}
	host, scheme := strings.ToLower(u.Hostname()), strings.ToLower(u.Scheme)
	if strings.HasSuffix(host, ".") || strings.Contains(u.Path, "//") {
		return "", fail
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return "", fail
		}
	}
	ip := net.ParseIP(host)
	if scheme != "https" && !(scheme == "http" && (host == "localhost" || ip != nil && ip.IsLoopback())) {
		return "", fail
	}
	if ip != nil {
		host = ip.String()
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return "", fail
		}
		if scheme == "https" && port == "443" || scheme == "http" && port == "80" {
			port = ""
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return "", fail
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		if ip == nil {
			return "", fail
		}
		host = "[" + host + "]"
	}
	path := strings.TrimSuffix(u.Path, "/")
	if strings.HasSuffix(strings.ToLower(path), "/v1") {
		path = path[:len(path)-3]
	}
	return scheme + "://" + host + path, nil
}

func ValidStationPolicy(p *protocol.StationPolicy) bool {
	if p == nil || (p.Version != 1 && p.Version != 2) || len(p.APIBases) > 32 || len(p.ExternalAPIBases) > 32 || len(p.ReverseProxyAPIBases) > 32 {
		return false
	}
	if p.Version == 1 && (p.ExternalAPIBases != nil || p.ReverseProxyAPIBases != nil) || p.Version == 2 && p.AllowAllAccounts {
		return false
	}
	// AllowAllAccounts is only valid when APIBases is empty — a policy that
	// lists specific bases AND claims to allow everything would be inconsistent.
	if p.AllowAllAccounts && len(p.APIBases) > 0 {
		return false
	}
	seenAPI := map[string]bool{}
	all := map[string]bool{}
	for i, bases := range [][]string{p.APIBases, p.ExternalAPIBases, p.ReverseProxyAPIBases} {
		seen := seenAPI
		if i == 2 {
			seen = map[string]bool{}
		}
		for _, base := range bases {
			normalized, err := NormalizeStationAPIBase(base)
			if err != nil || normalized != base || seen[base] {
				return false
			}
			seen[base] = true
			all[base] = true
		}
	}
	return len(all) <= 32
}

// IsProtectedOfficialAccount requires a managed vault reference and the fixed
// provider route; a local source label or an arbitrary OAuth URL is insufficient.
func IsProtectedOfficialAccount(a LocalAccount) bool {
	return a.SourceType == "official_account" && a.ProviderID == "openai" && a.CredentialRef != "" && a.BaseURL == "https://chatgpt.com/backend-api/codex" && a.Wire == "responses"
}

func StationAllowsAccount(p *protocol.StationPolicy, a LocalAccount) bool {
	if !ValidStationPolicy(p) {
		return false
	}
	// Self-hosted Hub in open mode with no APIBases configured sets AllowAllAccounts
	// to bypass the per-account origin check.  The official cloud Hub never sets
	// this field, so it cannot be used to escalate privileges on a hosted station.
	if p.AllowAllAccounts {
		// Still block official-account proxy if the station doesn't allow it.
		if IsProtectedOfficialAccount(a) {
			return p.AllowOfficialAccounts
		}
		// api_key and reverse_proxy accounts are allowed without origin matching.
		if a.SourceType == "api_key" || a.SourceType == "reverse_proxy" {
			return a.CredentialRef == "" // vault-managed ref still excluded from remote
		}
		return false
	}
	if IsProtectedOfficialAccount(a) {
		return p.AllowOfficialAccounts
	}
	if a.SourceType != "api_key" && a.SourceType != "reverse_proxy" || a.CredentialRef != "" {
		return false
	}
	base, err := NormalizeStationAPIBase(a.BaseURL)
	if err != nil {
		return false
	}
	scopes := [][]string{p.APIBases}
	if p.Version == 2 {
		if a.SourceType == "reverse_proxy" {
			scopes = [][]string{p.ReverseProxyAPIBases}
		} else {
			scopes = append(scopes, p.ExternalAPIBases)
		}
	}
	for _, scope := range scopes {
		for _, allowed := range scope {
			if allowed == base {
				return true
			}
		}
	}
	return false
}

// StationHasAPIAccounts is an aggregate display hint, never an account grant.
func StationHasAPIAccounts(p *protocol.StationPolicy) bool {
	return ValidStationPolicy(p) && (p.AllowAllAccounts || len(p.APIBases)+len(p.ExternalAPIBases)+len(p.ReverseProxyAPIBases) > 0)
}

func (s *StationObservation) Available() bool {
	return s != nil && (s.Mode == "open" || s.Mode == "restricted") && s.Revision != "" && ValidStationPolicy(s.Station) && s.CheckedAt > 0 && time.Now().UnixMilli()-s.CheckedAt < int64((5*time.Minute)/time.Millisecond) && s.CheckedAt <= time.Now().UnixMilli()+30000
}

func (c Config) RemoteStationAllows(a LocalAccount) bool {
	if !c.RemoteHubAllowsAccount(c.EffectiveHubURL(), a) {
		return false
	}
	if !c.RemoteExecutionArmed {
		return true
	}
	auth := c.RemoteStationAuthorization
	return auth != nil && auth.Revision != "" && auth.Identity == RemoteIdentity(c) && StationAllowsAccount(&auth.Station, a)
}

// RecordRemoteStationObservation updates the current station's public status.
// It can retire authority but never grants it. Callers must bind the observation
// to the current saved connection before invoking it under the Store lock.
func (c *Config) RecordRemoteStationObservation(observation *StationObservation) {
	if observation == nil {
		observation = &StationObservation{CheckedAt: time.Now().UnixMilli()}
	}
	c.RemoteStationObservation = CloneStationObservation(observation)
	if auth := c.RemoteStationAuthorization; auth != nil && (!observation.Available() || auth.Revision != observation.Revision || !StationPolicyEqual(&auth.Station, observation.Station)) {
		c.RemoteStationAuthorization = nil
	}
}

func (c Config) remoteStationFingerprint() string {
	if c.RemoteStationAuthorization == nil {
		return ""
	}
	b, _ := json.Marshal(c.RemoteStationAuthorization)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// BindRemoteStation stores only the process-local authority from an authenticated
// command. Config writes and gateway lease changes share the same store lock.
func (s *Store) BindRemoteStation(identity, revision string, policy *protocol.StationPolicy, validate func(Config) error) error {
	s.mu.Lock()
	if identity != RemoteIdentity(s.cfg) || revision == "" || !ValidStationPolicy(policy) {
		s.mu.Unlock()
		return errors.New("中转站能力已变化，请刷新后重试")
	}
	if validate != nil {
		if err := validate(s.cfg); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	next := &StationAuthorization{Identity: identity, Revision: revision, Station: *CloneStationPolicy(policy)}
	if stationAuthorizationEqual(s.cfg.RemoteStationAuthorization, next) {
		s.mu.Unlock()
		return nil
	}
	old := s.cfg.Clone()
	s.cfg.RemoteStationAuthorization = next
	fenceGatewayAuthorization(old, &s.cfg)
	cur := s.cfg.Clone()
	subs := append([]func(old, new Config){}, s.subs...)
	s.mu.Unlock()
	for _, f := range subs {
		f(old, cur)
	}
	return nil
}

// BeginRemoteStationObservation orders probes independently of wall-clock
// precision. The latest started probe wins, even if an older failure arrives
// after a successful response or two requests start in the same millisecond.
func (s *Store) BeginRemoteStationObservation(expected Config) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.GatewayRuntimeKey != expected.GatewayRuntimeKey {
		return 0
	}
	valid := map[string]bool{RemoteIdentity(s.cfg): true}
	for _, saved := range s.cfg.RemoteConnections {
		candidate := s.cfg
		candidate.HubURL, candidate.DeviceID, candidate.DeviceSecret = saved.HubURL, saved.DeviceID, saved.DeviceSecret
		candidate.RemoteDeviceOnly = true
		valid[RemoteIdentity(candidate)] = true
	}
	identity := RemoteIdentity(expected)
	if !valid[identity] {
		return 0
	}
	if s.stationObservationLatest == nil {
		s.stationObservationLatest = map[string]uint64{}
	}
	for key := range s.stationObservationLatest {
		if !valid[key] {
			delete(s.stationObservationLatest, key)
		}
	}
	s.stationObservationSequence++
	s.stationObservationLatest[identity] = s.stationObservationSequence
	return s.stationObservationSequence
}

// ObserveRemoteStation compares the exact saved credentials at completion so
// a delayed response cannot advertise capabilities for a replaced connection.
// It does not grant a worker lease, and is intentionally an in-memory update.
func (s *Store) ObserveRemoteStation(expected Config, observation *StationObservation, sequence uint64) {
	s.mu.Lock()
	var old Config
	var authorizationChanged bool
	defer func() {
		if !authorizationChanged {
			s.mu.Unlock()
			return
		}
		cur := s.cfg.Clone()
		subs := append([]func(old, new Config){}, s.subs...)
		s.mu.Unlock()
		for _, f := range subs {
			f(old, cur)
		}
	}()
	if s.cfg.GatewayRuntimeKey != expected.GatewayRuntimeKey || sequence == 0 || s.stationObservationLatest[RemoteIdentity(expected)] != sequence {
		return
	}
	if observation == nil {
		// Keep an unavailable observation so a queued command cannot mistake a
		// failed refresh for an unobserved station and restore its former scope.
		observation = &StationObservation{CheckedAt: time.Now().UnixMilli()}
	}
	now := time.Now().UnixMilli()
	active := RemoteIdentity(s.cfg) == RemoteIdentity(expected)
	known := active
	for i := range s.cfg.RemoteConnections {
		saved := &s.cfg.RemoteConnections[i]
		if NormalizeHubURL(saved.HubURL) == NormalizeHubURL(expected.EffectiveHubURL()) && saved.DeviceID == expected.DeviceID && saved.DeviceSecret == expected.DeviceSecret {
			if StationObservationIsNewer(saved.RemoteSupport, observation, now) {
				return
			}
			known = true
			saved.RemoteSupport = CloneStationObservation(observation)
		}
	}
	if !known || !active {
		return
	}
	old = s.cfg.Clone()
	if StationObservationIsNewer(s.cfg.RemoteStationObservation, observation, now) {
		return
	}
	s.cfg.RecordRemoteStationObservation(observation)
	fenceGatewayAuthorization(old, &s.cfg)
	authorizationChanged = !stationAuthorizationEqual(old.RemoteStationAuthorization, s.cfg.RemoteStationAuthorization)
}
