package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/localusage"
)

func auditUsageOverview(t *testing.T, upstream http.Handler) (*Server, func(bool) *httptest.ResponseRecorder) {
	t.Helper()
	// Only synthetic provider observations are read, never native usage files.
	localUsageOnce.Do(func() {})
	localUsage = &localusage.Scanner{ClaudeHome: t.TempDir(), CodexHome: t.TempDir()}
	provider := httptest.NewServer(upstream)
	t.Cleanup(provider.Close)
	s, h := newTestServer(t)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.LocalAccounts = []config.LocalAccount{{ID: "usage-card", Name: "old account", Kind: "api", BaseURL: provider.URL, Key: "synthetic-old-key"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	host := "127.0.0.1:47831"
	ck := do(h, "GET", "/", host, nil, "").Result().Cookies()[0]
	return s, func(force bool) *httptest.ResponseRecorder {
		path := "/api/usage/overview?days=7"
		if force {
			path += "&refresh=1"
		}
		return do(h, "GET", path, host, map[string]string{"Cookie": ck.Name + "=" + ck.Value}, "")
	}
}

func auditOverviewRemaining(t *testing.T, w *httptest.ResponseRecorder) (float64, string) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("overview status=%d body=%s", w.Code, w.Body.String())
	}
	var out struct {
		Sources []usageSource `json:"sources"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out.Sources) != 1 || !out.Sources[0].OK {
		t.Fatalf("unexpected synthetic overview: %s", w.Body.String())
	}
	remaining, ok := out.Sources[0].Usage["remaining"].(float64)
	if !ok {
		t.Fatal("synthetic remaining observation missing")
	}
	return remaining, out.Sources[0].Name
}

func auditRotateUsageKey(t *testing.T, s *Server) {
	t.Helper()
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.LocalAccounts[0].Key, c.LocalAccounts[0].Name = "synthetic-new-key", "new account"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAuditUsageOverviewDoesNotReusePreviousCredentialCache(t *testing.T) {
	var hits atomic.Int32
	s, request := auditUsageOverview(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") == "Bearer synthetic-new-key" {
			_, _ = w.Write([]byte(`{"remaining":79}`))
		} else {
			_, _ = w.Write([]byte(`{"remaining":13}`))
		}
	}))
	if old, _ := auditOverviewRemaining(t, request(false)); old != 13 {
		t.Fatal("initial credential observation missing")
	}
	auditRotateUsageKey(t, s)
	remaining, name := auditOverviewRemaining(t, request(false))
	if remaining != 79 || name != "new account" || hits.Load() != 2 {
		t.Fatalf("new credential reused previous account usage: remaining=%v name=%q calls=%d", remaining, name, hits.Load())
	}
}

func TestAuditUsageOverviewLateOldQueryCannotReplaceCurrentObservation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	s, request := auditUsageOverview(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer synthetic-old-key" {
			close(entered)
			<-release
			_, _ = w.Write([]byte(`{"remaining":13}`))
			return
		}
		_, _ = w.Write([]byte(`{"remaining":79}`))
	}))
	oldDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { oldDone <- request(true) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("synthetic old query did not start")
	}
	auditRotateUsageKey(t, s)
	if current, _ := auditOverviewRemaining(t, request(true)); current != 79 {
		t.Fatal("current credential query failed")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case old := <-oldDone:
		if old.Code != http.StatusConflict || old.Body.String() == "" {
			t.Fatalf("old credential result was published instead of retired: %d %s", old.Code, old.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("old query did not finish")
	}
	remaining, name := auditOverviewRemaining(t, request(false))
	if remaining != 79 || name != "new account" {
		t.Fatalf("late old query replaced current account usage: remaining=%v name=%q", remaining, name)
	}
}

func TestAuditUsageOverviewRetiresCredentialABARead(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	s, request := auditUsageOverview(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = w.Write([]byte(`{"remaining":13}`))
	}))
	oldDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { oldDone <- request(true) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("synthetic query did not start")
	}
	auditRotateUsageKey(t, s)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.LocalAccounts[0].Key, c.LocalAccounts[0].Name = "synthetic-old-key", "old account"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case old := <-oldDone:
		if old.Code != http.StatusConflict {
			t.Fatalf("credential restoration revived retired usage read: %d %s", old.Code, old.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("old query did not finish")
	}
}

func TestAuditUsageOverviewRefreshesNameWithoutInvalidatingUnrelatedEdits(t *testing.T) {
	var hits atomic.Int32
	s, request := auditUsageOverview(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"remaining":13}`))
	}))
	auditOverviewRemaining(t, request(false))
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.DeviceName = "unrelated display"
		c.LocalAccounts[0].Models = []string{"new catalog model"}
		c.LocalAccounts[0].LastUsedAt = 10
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, name := auditOverviewRemaining(t, request(false)); hits.Load() != 1 || name != "old account" {
		t.Fatal("unrelated settings/catalog change invalidated credential observation", hits.Load(), name)
	}
	if err := s.d.Store.Update(func(c *config.Config) error { c.LocalAccounts[0].Name = "renamed account"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, name := auditOverviewRemaining(t, request(false)); name != "renamed account" {
		t.Fatal("renamed source returned old display metadata", name)
	}
}
