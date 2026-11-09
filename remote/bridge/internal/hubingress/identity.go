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
	if !validManifest(m, m.CanonicalURL, now) || id != "" && m.HubInstanceID != id || key != "" && m.IdentityPublicKey != key || (id == "") != (key == "") {
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
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	stored, storedErr := verifiedManifest(e.manifest, pin.id, pin.key, "", now)
	if !now.Before(e.refreshAt) || storedErr != nil {
		e.refreshAt = now.Add(time.Minute)
		addresses := []string{pin.endpoint, canonical}
		if storedErr == nil {
			for _, endpoint := range stored.Endpoints {
				addresses = append(addresses, endpoint.URL)
			}
		}
		seen := map[string]bool{}
		for _, address := range addresses {
			if ctx.Err() != nil {
				break
			}
			if seen[address] {
				continue
			}
			seen[address] = true
			var wire Manifest
			if t.readJSON(ctx, address+"/ingress/manifest", &wire) != nil {
				continue
			}
			m, err := verifiedManifest(wire, pin.id, pin.key, address, time.Now())
			if err != nil || storedErr == nil && m.Revision < stored.Revision {
				continue
			}
			stored, storedErr = m, nil
			e.manifest, e.refreshAt = m, now.Add(time.Hour)
			t.persistAs(canonical, m)
			break
		}
	}
	if storedErr != nil {
		return Manifest{}, nil
	}
	list := append([]Endpoint(nil), stored.Endpoints...)
	sort.SliceStable(list, func(i, j int) bool {
		left, right := now.Before(e.failed[list[i].URL]), now.Before(e.failed[list[j].URL])
		if left != right {
			return !left
		}
		if (list[i].URL == pin.endpoint) != (list[j].URL == pin.endpoint) {
			return list[i].URL == pin.endpoint
		}
		return list[i].Priority < list[j].Priority
	})
	return stored, list
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
	e := t.state(canonical)
	ctx, cancel := context.WithTimeout(req.Context(), 6*time.Second)
	defer cancel()
	m, candidates := t.pinnedCandidates(ctx, canonical, pin, e)
	for _, endpoint := range candidates {
		// This includes the canonical URL: TLS alone cannot preserve an older
		// device identity after that hostname's Hub registry has been reset.
		if ctx.Err() != nil {
			break
		}
		if t.verify(ctx, m, endpoint.URL) != nil {
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
	return nil, errors.New("ingress_identity_unavailable")
}
