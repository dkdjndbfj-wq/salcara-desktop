// Package hubingress changes only the HTTP destination of a configured Hub.
// Pairing, device identity, receipts and caller URLs remain canonical.
package hubingress

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"salcara/bridge/internal/atomicfile"
)

type Endpoint struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Priority int    `json:"priority"`
}
type Manifest struct {
	Version           int        `json:"version"`
	HubInstanceID     string     `json:"hub_instance_id"`
	IdentityPublicKey string     `json:"identity_public_key"`
	CanonicalURL      string     `json:"canonical_url"`
	Revision          int64      `json:"revision"`
	IssuedAt          int64      `json:"issued_at"`
	ExpiresAt         int64      `json:"expires_at"`
	Endpoints         []Endpoint `json:"endpoints"`
}
type proof struct {
	Version       int    `json:"version"`
	HubInstanceID string `json:"hub_instance_id"`
	Nonce         string `json:"nonce"`
	IssuedAt      int64  `json:"issued_at"`
	ExpiresAt     int64  `json:"expires_at"`
	Signature     string `json:"signature"`
}
type canonicalKey struct{}

// Bind allows routing only requests constructed for this logical Hub. A random
// request to the same HTTP client never gains access to its cached endpoints.
func Bind(req *http.Request, canonical string) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), canonicalKey{}, normalizeURL(canonical)))
}

type entry struct {
	mu        sync.Mutex
	manifest  Manifest
	refreshAt time.Time
	failed    map[string]time.Time
}
type Transport struct {
	base    http.RoundTripper
	client  *http.Client
	dir     string
	mu      sync.Mutex
	entries map[string]*entry
}

func New(base http.RoundTripper, dir string) *Transport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &Transport{base: base, dir: dir, entries: map[string]*entry{}, client: &http.Client{Transport: base, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func validHex(s string, bytes int) bool {
	return len(s) == bytes*2 && strings.Trim(s, "0123456789abcdef") == ""
}
func validURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 512 || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || u.Opaque != "" || u.Path != "/salcara-hub/v1" || strings.ContainsAny(raw, "\\\r\n\t ") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || !strings.ContainsAny(host, ".:") || strings.Contains(host, "%") {
		return false
	}
	if ip, e := netip.ParseAddr(host); e == nil && (!ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
		return false
	}
	if port := u.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return false
		}
	}
	return true
}
func normalizeURL(raw string) string {
	if !validURL(raw) {
		return ""
	}
	u, _ := url.Parse(raw)
	u.Host = strings.ToLower(u.Host)
	if u.Port() == "443" {
		u.Host = u.Hostname()
		if strings.Contains(u.Host, ":") {
			u.Host = "[" + u.Host + "]"
		}
	}
	return u.String()
}
func validManifest(m Manifest, canonical string, now time.Time) bool {
	key, err := base64.StdEncoding.DecodeString(m.IdentityPublicKey)
	if m.Version != 1 || !validHex(m.HubInstanceID, 16) || err != nil || len(key) != ed25519.PublicKeySize || m.CanonicalURL != canonical || m.Revision < 1 || m.IssuedAt < now.Add(-24*time.Hour).UnixMilli() || m.IssuedAt > now.Add(5*time.Second).UnixMilli() || m.ExpiresAt <= now.UnixMilli() || m.ExpiresAt-m.IssuedAt != int64((24*time.Hour)/time.Millisecond) || len(m.Endpoints) < 1 || len(m.Endpoints) > 8 {
		return false
	}
	seen := map[string]bool{}
	found := false
	for _, e := range m.Endpoints {
		if !validURL(e.URL) || e.Priority < 0 || e.Priority > 1000 || e.ID != "canonical" && !validHex(e.ID, 16) || seen[e.URL] || seen[e.ID] {
			return false
		}
		seen[e.URL] = true
		seen[e.ID] = true
		if e.ID == "canonical" {
			if e.URL != canonical {
				return false
			}
			found = true
		} else if e.URL == canonical {
			return false
		}
	}
	return found
}
func (t *Transport) cachePath(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return filepath.Join(t.dir, hex.EncodeToString(sum[:])+".json")
}
func (t *Transport) persist(m Manifest) {
	if t.dir == "" {
		return
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if atomicfile.WriteFile(t.cachePath(m.CanonicalURL), raw, 0600) != nil {
		return
	}
	files, _ := os.ReadDir(t.dir)
	type cached struct {
		name string
		at   time.Time
	}
	owned := []cached{}
	for _, file := range files {
		if file.Type().IsRegular() && strings.HasSuffix(file.Name(), ".json") && validHex(strings.TrimSuffix(file.Name(), ".json"), 32) {
			if info, err := file.Info(); err == nil {
				owned = append(owned, cached{file.Name(), info.ModTime()})
			}
		}
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i].at.After(owned[j].at) })
	for i := 16; i < len(owned); i++ {
		_ = os.Remove(filepath.Join(t.dir, owned[i].name))
	}
}
func (t *Transport) state(canonical string) *entry {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e := t.entries[canonical]; e != nil {
		return e
	}
	// A client normally has one active Hub and a few explicitly paired sites.
	// Avoid unbounded per-URL memory even if a caller is reconfigured repeatedly.
	if len(t.entries) >= 16 {
		for key := range t.entries {
			delete(t.entries, key)
			break
		}
	}
	e := &entry{failed: map[string]time.Time{}}
	if t.dir != "" {
		path := t.cachePath(canonical)
		if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() && info.Size() <= 32<<10 {
			if file, err := os.Open(path); err == nil {
				raw, readErr := io.ReadAll(io.LimitReader(file, (32<<10)+1))
				file.Close()
				var m Manifest
				if readErr == nil && len(raw) <= 32<<10 && json.Unmarshal(raw, &m) == nil && validManifest(m, canonical, time.Now()) {
					e.manifest = m
				}
			}
		}
	}
	t.entries[canonical] = e
	return e
}
func (t *Transport) readJSON(ctx context.Context, address string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return errors.New("ingress_unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("ingress_unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (32<<10)+1))
	if err != nil || len(raw) > 32<<10 {
		return errors.New("ingress_invalid_response")
	}
	return json.Unmarshal(raw, out)
}
func (t *Transport) candidates(ctx context.Context, canonical string, e *entry) (Manifest, []Endpoint) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	if !now.Before(e.refreshAt) {
		e.refreshAt = now.Add(time.Minute)
		var m Manifest
		// This request always goes directly to the user-supplied canonical URL,
		// without cookies or any device/account credential. Backups cannot renew it.
		if t.readJSON(ctx, canonical+"/ingress/manifest", &m) == nil && validManifest(m, canonical, time.Now()) {
			if e.manifest.HubInstanceID == "" || m.HubInstanceID != e.manifest.HubInstanceID || m.IdentityPublicKey != e.manifest.IdentityPublicKey || m.Revision >= e.manifest.Revision {
				e.manifest = m
				retained := map[string]time.Time{}
				for _, endpoint := range m.Endpoints {
					retained[endpoint.URL] = e.failed[endpoint.URL]
				}
				e.failed = retained
				e.refreshAt = now.Add(time.Hour)
				t.persist(m)
			}
		} else {
			e.failed[canonical] = now.Add(30 * time.Second)
		}
	}
	m := e.manifest
	if !validManifest(m, canonical, time.Now()) {
		return Manifest{}, nil
	}
	list := append([]Endpoint{}, m.Endpoints...)
	sort.SliceStable(list, func(i, j int) bool {
		left, right := time.Now().Before(e.failed[list[i].URL]), time.Now().Before(e.failed[list[j].URL])
		if left != right {
			return !left
		}
		return list[i].Priority < list[j].Priority
	})
	return m, list
}
func (t *Transport) verify(ctx context.Context, m Manifest, address string) error {
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return errors.New("ingress_identity_unavailable")
	}
	n := hex.EncodeToString(nonce[:])
	var p proof
	if t.readJSON(ctx, address+"/ingress/identity?nonce="+n, &p) != nil {
		return errors.New("ingress_identity_unavailable")
	}
	now := time.Now().UnixMilli()
	if m.ExpiresAt <= now || p.Version != 1 || p.HubInstanceID != m.HubInstanceID || p.Nonce != n || p.IssuedAt < now-30000 || p.IssuedAt > now+5000 || p.ExpiresAt <= now || p.ExpiresAt-p.IssuedAt != 30000 {
		return errors.New("ingress_identity_mismatch")
	}
	key, _ := base64.StdEncoding.DecodeString(m.IdentityPublicKey)
	sig, err := base64.StdEncoding.DecodeString(p.Signature)
	message := []byte(fmt.Sprintf("salcara-ingress-proof-v1\n%s\n%s\n%d\n%d", p.HubInstanceID, p.Nonce, p.IssuedAt, p.ExpiresAt))
	if err != nil || !ed25519.Verify(key, message, sig) {
		return errors.New("ingress_identity_mismatch")
	}
	return nil
}
func (t *Transport) failed(e *entry, address string) {
	e.mu.Lock()
	e.failed[address] = time.Now().Add(30 * time.Second)
	e.mu.Unlock()
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, err
	}
	canonical, _ := req.Context().Value(canonicalKey{}).(string)
	requestOrigin := *req.URL
	requestOrigin.Path = "/salcara-hub/v1"
	requestOrigin.RawPath = ""
	requestOrigin.RawQuery = ""
	requestOrigin.ForceQuery = false
	if !validURL(canonical) || !strings.HasPrefix(req.URL.Path, "/salcara-hub/v1/") || normalizeURL(requestOrigin.String()) != canonical {
		return t.base.RoundTrip(req)
	}
	e := t.state(canonical)
	routeCtx, cancel := context.WithTimeout(req.Context(), 6*time.Second)
	defer cancel()
	m, candidates := t.candidates(routeCtx, canonical, e)
	if len(candidates) <= 1 {
		return t.base.RoundTrip(req)
	}
	var last error
	canonicalAttempted := false
	for _, endpoint := range candidates {
		if routeCtx.Err() != nil {
			break
		}
		// The original HTTPS address is already the user's trust root. Only a
		// backup needs a signed challenge before receiving credentials.
		if endpoint.URL != canonical {
			if err := t.verify(routeCtx, m, endpoint.URL); err != nil {
				last = err
				t.failed(e, endpoint.URL)
				continue
			}
		}
		next := req.Clone(req.Context())
		nextURL := *req.URL
		target, _ := url.Parse(endpoint.URL)
		nextURL.Scheme = target.Scheme
		nextURL.Host = target.Host
		next.URL = &nextURL
		next.Host = ""
		// Suppress net/http's connection-reuse replay even when callers supplied
		// an Idempotency-Key or GetBody. Application-level receipts own retries.
		if next.Method != http.MethodGet && next.Method != http.MethodHead {
			next.GetBody = nil
		}
		if endpoint.URL == canonical {
			canonicalAttempted = true
		}
		resp, err := t.base.RoundTrip(next)
		if err != nil || resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504 {
			t.failed(e, endpoint.URL)
		}
		if (err != nil || resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504) && req.Method == http.MethodGet && req.Body == nil {
			if resp != nil {
				resp.Body.Close()
			}
			last = errors.New("ingress_unavailable")
			continue
		}
		return resp, err
	}
	// Expired route-selection budgets or an older Hub without the optional
	// identity endpoint cannot lock users out of their original trusted URL.
	// No mutation has been sent when control reaches this fallback.
	if !canonicalAttempted && req.Context().Err() == nil {
		next := req.Clone(req.Context())
		if next.Method != http.MethodGet && next.Method != http.MethodHead {
			next.GetBody = nil
		}
		return t.base.RoundTrip(next)
	}
	if last == nil {
		last = errors.New("ingress_unavailable")
	}
	if req.Body != nil {
		req.Body.Close()
	}
	return nil, last
}
func (t *Transport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
