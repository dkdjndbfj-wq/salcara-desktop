package hubingress

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type identityKey struct{}
type identityBinding struct{ id, key, endpoint string }

// ErrIdentityChanged distinguishes a genuinely signed replacement instance
// from an unavailable/expired route. The saved identity is never rewritten.
var ErrIdentityChanged = errors.New("ingress_identity_changed")

// BindIdentity attaches the durable device identity pin. An incomplete or
// malformed pin stays attached and fails closed; it never becomes legacy mode.
// Bind should be called first to identify the logical request URL.
func BindIdentity(req *http.Request, id, key, endpoint string) *http.Request {
	if id == "" && key == "" && endpoint == "" {
		return req
	}
	if endpoint == "" {
		endpoint, _ = req.Context().Value(canonicalKey{}).(string)
	}
	pin := identityBinding{id: id, key: key, endpoint: normalizeURL(endpoint)}
	return req.WithContext(context.WithValue(req.Context(), identityKey{}, pin))
}

// ResolveIdentity anonymously verifies the explicitly selected HTTPS endpoint.
// Its signed membership and a fresh proof are required before callers pin it.
func ResolveIdentity(ctx context.Context, apiURL string) (Manifest, error) {
	return New(nil, "").ResolveIdentity(ctx, apiURL)
}

// ResolveIdentity uses this transport's anonymous client, allowing local tests
// to exercise the complete exchange without real DNS or Hub credentials.
func (t *Transport) ResolveIdentity(ctx context.Context, apiURL string) (Manifest, error) {
	endpoint := normalizeURL(apiURL)
	if endpoint == "" {
		return Manifest{}, errors.New("ingress_invalid_endpoint")
	}
	var wire Manifest
	if err := t.readJSON(ctx, endpoint+"/ingress/manifest", &wire); err != nil {
		return Manifest{}, err
	}
	m, err := verifiedManifest(wire, "", "", endpoint, time.Now())
	if err != nil {
		return Manifest{}, err
	}
	if err := t.verify(ctx, m, endpoint); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func normalizeIngressURL(raw string) string {
	if raw == "" || len(raw) > 512 || strings.ContainsAny(raw, "%\\\r\n\t ?#") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || u.Opaque != "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return ""
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return ""
		}
	} else {
		if len(host) > 253 || !strings.Contains(host, ".") || strings.Trim(host, "0123456789.") == "" {
			return ""
		}
		for _, label := range strings.Split(host, ".") {
			if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' || strings.Trim(label, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
				return ""
			}
		}
	}
	if port := u.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return ""
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return ""
	}
	path := u.Path
	if path == "" || path == "/" {
		path = "/salcara-hub/v1"
	} else {
		if strings.HasSuffix(path, "/salcara-hub/") {
			path = strings.TrimSuffix(path, "/")
		}
		if strings.HasSuffix(path, "/salcara-hub") {
			path += "/v1"
		}
		if !strings.HasSuffix(path, "/v1") {
			return ""
		}
	}
	for _, segment := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." || strings.Trim(segment, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._~-") != "" {
			return ""
		}
	}
	u.Path = path
	u.Host = strings.ToLower(u.Host)
	if u.Port() == "443" {
		u.Host = host
		if strings.Contains(host, ":") {
			u.Host = "[" + host + "]"
		}
	}
	return u.String()
}

func requestSuffix(req *http.Request, canonical string) (string, bool) {
	if canonical == "" || normalizeURL(canonical) != canonical || req.URL == nil || req.URL.RawPath != "" {
		return "", false
	}
	base, _ := url.Parse(canonical)
	if !strings.HasPrefix(req.URL.Path, base.Path+"/") {
		return "", false
	}
	origin := *req.URL
	origin.Path, origin.RawPath, origin.RawQuery, origin.ForceQuery, origin.Fragment = base.Path, "", "", false, ""
	if normalizeURL(origin.String()) != canonical {
		return "", false
	}
	return strings.TrimPrefix(req.URL.Path, base.Path), true
}

// The envelope's flat fields are deliberately ignored. Only the exact signed
// bytes can authorize an endpoint; a proxy cannot append itself to that list.
func verifiedManifest(wire Manifest, id, key, endpoint string, now time.Time) (Manifest, error) {
	return verifiedManifestEnvelope(wire, id, key, endpoint, now, false)
}

func verifiedManifestEnvelope(wire Manifest, id, key, endpoint string, now time.Time, allowExpired bool) (Manifest, error) {
	payload, err := base64.StdEncoding.Strict().DecodeString(wire.SignedPayload)
	if err != nil || len(payload) == 0 || len(payload) > 24<<10 {
		return Manifest{}, errors.New("ingress_manifest_signature_required")
	}
	var m Manifest
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &m) != nil || json.Unmarshal(payload, &fields) != nil {
		return Manifest{}, errors.New("ingress_invalid_manifest")
	}
	if _, exists := fields["signed_payload"]; exists {
		return Manifest{}, errors.New("ingress_invalid_manifest")
	}
	if _, exists := fields["signature"]; exists {
		return Manifest{}, errors.New("ingress_invalid_manifest")
	}
	if !validManifestAt(m, m.CanonicalURL, now, allowExpired) || id != "" && m.HubInstanceID != id || key != "" && m.IdentityPublicKey != key || (id == "") != (key == "") {
		return Manifest{}, errors.New("ingress_identity_mismatch")
	}
	publicKey, err := base64.StdEncoding.Strict().DecodeString(m.IdentityPublicKey)
	signature, sigErr := base64.StdEncoding.Strict().DecodeString(wire.Signature)
	if err != nil || sigErr != nil || len(publicKey) != ed25519.PublicKeySize || !ed25519.Verify(publicKey, append([]byte("salcara-ingress-manifest-v1\n"), payload...), signature) {
		return Manifest{}, errors.New("ingress_identity_mismatch")
	}
	if endpoint != "" && !manifestContains(m, endpoint) {
		return Manifest{}, errors.New("ingress_endpoint_not_authorized")
	}
	m.SignedPayload, m.Signature = wire.SignedPayload, wire.Signature
	return m, nil
}

func manifestContains(m Manifest, endpoint string) bool {
	for _, candidate := range m.Endpoints {
		if candidate.URL == endpoint {
			return true
		}
	}
	return false
}

func (t *Transport) pinnedCandidates(ctx context.Context, canonical string, pin identityBinding, e *entry) (Manifest, []Endpoint) {
	for {
		e.mu.Lock()
		if pending := e.refreshing; pending != nil {
			e.mu.Unlock()
			select {
			case <-pending:
				continue
			case <-ctx.Done():
				return Manifest{}, nil
			}
		}
		pinKey := pin.id + ":" + pin.key
		if ctx.Err() != nil || e.refreshPin == pinKey && time.Now().Before(e.refreshAt) {
			m, candidates := pinnedSnapshot(e, pin)
			e.mu.Unlock()
			return m, candidates
		}
		// The mutex protects state only. Concurrent requests wait on this flight
		// with their own deadline instead of queuing behind network I/O.
		pending := make(chan struct{})
		e.refreshing, e.refreshPin = pending, pinKey
		e.refreshAt = time.Now().Add(time.Minute)
		hint, hintErr := verifiedManifestEnvelope(e.manifest, pin.id, pin.key, "", time.Now(), true)
		addresses := []string{pin.endpoint, canonical}
		if hintErr == nil {
			if manifestContains(hint, e.lastEndpoint) {
				addresses = append([]string{e.lastEndpoint}, addresses...)
			}
			for _, endpoint := range hint.Endpoints {
				addresses = append(addresses, endpoint.URL)
			}
		}
		unique := make([]string, 0, len(addresses))
		seen := map[string]bool{}
		for _, address := range addresses {
			if normalizeURL(address) != "" && !seen[address] {
				seen[address] = true
				unique = append(unique, address)
			}
		}
		offset := 0
		if len(unique) > 0 {
			offset = e.refreshOffset % len(unique)
		}
		e.mu.Unlock()

		var refreshed Manifest
		identityChanged := false
		attempted := 0
		for attempted < len(unique) && attempted < 8 {
			if ctx.Err() != nil {
				break
			}
			// Old logical/pinned anchors can be outside the current eight-member
			// manifest. Rotate failed batches so they cannot starve its tail.
			address := unique[(offset+attempted)%len(unique)]
			attempted++
			probeCtx, cancel := context.WithTimeout(ctx, time.Second)
			var wire Manifest
			err := t.readJSON(probeCtx, address+"/ingress/manifest", &wire)
			cancel()
			if err != nil {
				continue
			}
			m, err := verifiedManifest(wire, pin.id, pin.key, address, time.Now())
			if err != nil {
				if other, otherErr := verifiedManifest(wire, "", "", address, time.Now()); otherErr == nil && (other.HubInstanceID != pin.id || other.IdentityPublicKey != pin.key) {
					identityChanged = true
				}
				continue
			}
			if hintErr == nil && m.Revision < hint.Revision {
				continue
			}
			refreshed = m
			break
		}

		e.mu.Lock()
		e.identityChanged = identityChanged && refreshed.HubInstanceID == ""
		if refreshed.HubInstanceID != "" && ctx.Err() == nil {
			e.refreshOffset = 0
			e.manifest, e.refreshAt = refreshed, time.Now().Add(time.Hour)
			if expires := time.UnixMilli(refreshed.ExpiresAt); expires.Before(e.refreshAt) {
				e.refreshAt = expires
			}
			// Retain only current members to bound failure state across migrations.
			retained := map[string]time.Time{}
			for _, endpoint := range refreshed.Endpoints {
				retained[endpoint.URL] = e.failed[endpoint.URL]
			}
			e.failed = retained
			t.persistAs(canonical, refreshed)
		} else if ctx.Err() != nil {
			// One cancelled caller must not suppress the next user's attempt.
			e.refreshAt = time.Time{}
		}
		if refreshed.HubInstanceID == "" && len(unique) > 0 {
			e.refreshOffset = (offset + attempted) % len(unique)
		}
		e.refreshing = nil
		close(pending)
		m, candidates := pinnedSnapshot(e, pin)
		e.mu.Unlock()
		return m, candidates
	}
}

// Caller holds e.mu. Expired hints never reach the credential-routing list.
func pinnedSnapshot(e *entry, pin identityBinding) (Manifest, []Endpoint) {
	now := time.Now()
	stored, err := verifiedManifest(e.manifest, pin.id, pin.key, "", now)
	if err != nil {
		return Manifest{}, nil
	}
	list := append([]Endpoint(nil), stored.Endpoints...)
	sort.SliceStable(list, func(i, j int) bool {
		left, right := now.Before(e.failed[list[i].URL]), now.Before(e.failed[list[j].URL])
		if left != right {
			return !left
		}
		if (list[i].URL == e.lastEndpoint) != (list[j].URL == e.lastEndpoint) {
			return list[i].URL == e.lastEndpoint
		}
		if (list[i].URL == pin.endpoint) != (list[j].URL == pin.endpoint) {
			return list[i].URL == pin.endpoint
		}
		return list[i].Priority < list[j].Priority
	})
	return stored, list
}

// ResolvePinnedIdentity recovers an existing logical connection anonymously.
// Only the returned endpoint may be used for fresh discovery; the caller keeps
// its logical URL, device identity, binding and history unchanged.
func (t *Transport) ResolvePinnedIdentity(ctx context.Context, canonical, id, key, endpoint string) (Manifest, string, error) {
	canonical = normalizeURL(canonical)
	if endpoint == "" {
		endpoint = canonical
	}
	pin := identityBinding{id: id, key: key, endpoint: normalizeURL(endpoint)}
	publicKey, err := base64.StdEncoding.Strict().DecodeString(key)
	if canonical == "" || pin.endpoint == "" || !validHex(id, 16) || err != nil || len(publicKey) != ed25519.PublicKeySize {
		return Manifest{}, "", errors.New("ingress_invalid_identity_binding")
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	e := t.acquireState(canonical)
	if e == nil {
		return Manifest{}, "", errors.New("ingress_busy")
	}
	defer t.releaseState(e)
	m, candidates := t.pinnedCandidates(ctx, canonical, pin, e)
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			break
		}
		if t.verifyPinned(ctx, m, candidate.URL) != nil {
			t.failed(e, candidate.URL)
			continue
		}
		t.succeeded(e, candidate.URL)
		return m, candidate.URL, nil
	}
	return Manifest{}, "", pinnedUnavailable(e, pin)
}

// VerifyPinnedEndpoint checks exactly the address a user supplied for recovery.
// It cannot silently succeed through a different cached endpoint. Share the
// refresh flight so an older automatic refresh cannot roll this result back.
func (t *Transport) VerifyPinnedEndpoint(ctx context.Context, canonical, id, key, endpoint string) (Manifest, error) {
	canonical, endpoint = normalizeURL(canonical), normalizeURL(endpoint)
	publicKey, err := base64.StdEncoding.Strict().DecodeString(key)
	if canonical == "" || endpoint == "" || !validHex(id, 16) || err != nil || len(publicKey) != ed25519.PublicKeySize {
		return Manifest{}, errors.New("ingress_invalid_identity_binding")
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	e := t.acquireState(canonical)
	if e == nil {
		return Manifest{}, errors.New("ingress_busy")
	}
	defer t.releaseState(e)
	for {
		e.mu.Lock()
		if pending := e.refreshing; pending != nil {
			e.mu.Unlock()
			select {
			case <-pending:
				continue
			case <-ctx.Done():
				return Manifest{}, ctx.Err()
			}
		}
		if err := ctx.Err(); err != nil {
			e.mu.Unlock()
			return Manifest{}, err
		}
		pending := make(chan struct{})
		e.refreshing = pending
		hint, hintErr := verifiedManifestEnvelope(e.manifest, id, key, "", time.Now(), true)
		e.mu.Unlock()

		probeCtx, probeCancel := context.WithTimeout(ctx, time.Second)
		var wire Manifest
		err := t.readJSON(probeCtx, endpoint+"/ingress/manifest", &wire)
		probeCancel()
		var m Manifest
		if err == nil {
			m, err = verifiedManifest(wire, id, key, endpoint, time.Now())
			if err != nil {
				if other, otherErr := verifiedManifest(wire, "", "", endpoint, time.Now()); otherErr == nil && (other.HubInstanceID != id || other.IdentityPublicKey != key) {
					err = ErrIdentityChanged
				}
			} else if hintErr == nil && m.Revision < hint.Revision {
				err = errors.New("ingress_revision_rollback")
			} else {
				err = t.verifyPinned(ctx, m, endpoint)
			}
		}
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		e.mu.Lock()
		if err == nil {
			e.manifest, e.lastEndpoint, e.refreshPin = m, endpoint, id+":"+key
			e.identityChanged, e.refreshOffset = false, 0
			e.refreshAt = time.Now().Add(time.Hour)
			if expires := time.UnixMilli(m.ExpiresAt); expires.Before(e.refreshAt) {
				e.refreshAt = expires
			}
			e.failed = map[string]time.Time{}
			t.persistAs(canonical, m)
		}
		e.refreshing = nil
		close(pending)
		e.mu.Unlock()
		if err != nil {
			return Manifest{}, err
		}
		return m, nil
	}
}

func pinnedUnavailable(e *entry, pin identityBinding) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	// A still-fresh manifest may describe retired routes. Bound the delay
	// before looking for a new one, without rescanning on every poll.
	if next := time.Now().Add(time.Minute); e.refreshAt.After(next) {
		e.refreshAt = next
	}
	if e.refreshPin == pin.id+":"+pin.key && e.identityChanged {
		return ErrIdentityChanged
	}
	return errors.New("ingress_identity_unavailable")
}

func (t *Transport) verifyPinned(ctx context.Context, m Manifest, endpoint string) error {
	probeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	return t.verify(probeCtx, m, endpoint)
}

func (t *Transport) succeeded(e *entry, endpoint string) {
	e.mu.Lock()
	e.lastEndpoint = endpoint
	delete(e.failed, endpoint)
	e.mu.Unlock()
}

func (t *Transport) roundTripPinned(req *http.Request, pin identityBinding) (*http.Response, error) {
	canonical, _ := req.Context().Value(canonicalKey{}).(string)
	suffix, matches := requestSuffix(req, canonical)
	key, err := base64.StdEncoding.Strict().DecodeString(pin.key)
	if !matches || !validHex(pin.id, 16) || err != nil || len(key) != ed25519.PublicKeySize || pin.endpoint == "" {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, errors.New("ingress_invalid_identity_binding")
	}
	e := t.acquireState(canonical)
	if e == nil {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, errors.New("ingress_busy")
	}
	defer t.releaseState(e)
	ctx, cancel := context.WithTimeout(req.Context(), 6*time.Second)
	defer cancel()
	m, candidates := t.pinnedCandidates(ctx, canonical, pin, e)
	for _, endpoint := range candidates {
		// This includes the canonical URL: TLS alone cannot preserve an older
		// device identity after that hostname's Hub registry has been reset.
		if ctx.Err() != nil {
			break
		}
		if t.verifyPinned(ctx, m, endpoint.URL) != nil {
			t.failed(e, endpoint.URL)
			continue
		}
		next := req.Clone(req.Context())
		target, _ := url.Parse(endpoint.URL)
		nextURL := *req.URL
		nextURL.Scheme, nextURL.Host, nextURL.Path = target.Scheme, target.Host, target.Path+suffix
		next.URL, next.Host = &nextURL, ""
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			next.GetBody = nil
		}
		resp, err := t.base.RoundTrip(next)
		failed := err != nil || resp != nil && (resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504)
		if failed {
			t.failed(e, endpoint.URL)
		} else {
			t.succeeded(e, endpoint.URL)
		}
		if failed && req.Method == http.MethodGet && req.Body == nil {
			if resp != nil {
				resp.Body.Close()
			}
			continue
		}
		return resp, err
	}
	if req.Body != nil {
		req.Body.Close()
	}
	return nil, pinnedUnavailable(e, pin)
}
