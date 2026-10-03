package console

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/desktopcompanion"
)

type fakeCompanion struct {
	previews, installs            int
	uninstallPreviews, uninstalls int
	err                           error
}

func (f *fakeCompanion) Preview(context.Context) (desktopcompanion.Status, error) {
	f.previews++
	return desktopcompanion.Status{Available: true, CatalogOnly: true, Version: "test", Message: "只读验证"}, f.err
}
func (f *fakeCompanion) Install(context.Context) (desktopcompanion.Status, error) {
	f.installs++
	return desktopcompanion.Status{Available: true, Installed: true, CatalogOnly: true, RestartRequired: true, Version: "test", Message: "已安装只读验证；未重启"}, f.err
}

func (f *fakeCompanion) PreviewUninstall(context.Context) (desktopcompanion.Status, error) {
	f.uninstallPreviews++
	return desktopcompanion.Status{Installed: true, UninstallAvailable: true, CatalogOnly: true, Version: "test", Message: "可卸载只读验证"}, f.err
}
func (f *fakeCompanion) Uninstall(context.Context) (desktopcompanion.Status, error) {
	f.uninstalls++
	return desktopcompanion.Status{Installed: false, UninstallAvailable: false, CatalogOnly: true, RestartRequired: true, Version: "test", Message: "已卸载；备份和资源保留"}, f.err
}

func TestCompanionUninstallRequiresLocalAuthAndExplicitConfirmation(t *testing.T) {
	s, h := newTestServer(t)
	f := &fakeCompanion{}
	s.d.Companion = f
	path, host := "/api/local/desktop-companion/uninstall", "127.0.0.1:47831"
	hdr := map[string]string{"Cookie": CookieName + "=" + s.Token(), "Content-Type": "application/json", "Origin": "http://127.0.0.1:47831"}
	if w := do(h, "POST", path, host, nil, `{"confirmed":true}`); w.Code != http.StatusUnauthorized {
		t.Fatal(w.Code)
	}
	bad := map[string]string{"Cookie": hdr["Cookie"], "Content-Type": "application/json", "Origin": "https://evil.test"}
	if w := do(h, "POST", path, host, bad, `{"confirmed":true}`); w.Code != http.StatusForbidden {
		t.Fatal(w.Code)
	}
	if w := do(h, "POST", path, "evil.test:47831", hdr, `{"confirmed":true}`); w.Code != http.StatusForbidden {
		t.Fatal(w.Code)
	}
	for _, body := range []string{`{}`, `{"confirmed":false}`, `{"confirmed":"yes"}`, `{"confirmed":true,"configPath":"C:/evil"}`, `{"confirmed":true} {}`} {
		if w := do(h, "POST", path, host, hdr, body); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", body, w.Code)
		}
	}
	if f.uninstalls != 0 {
		t.Fatal("uninstaller called without confirmation")
	}
	w := do(h, "GET", path, host, hdr, "")
	if w.Code != http.StatusOK || f.uninstalls != 0 || f.uninstallPreviews != 1 {
		t.Fatal("preview changed installation")
	}
	w = do(h, "POST", path, host, hdr, `{"confirmed":true}`)
	if w.Code != http.StatusOK || f.uninstalls != 1 || f.installs != 0 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	for _, field := range []string{`"installed":false`, `"desktopControl":false`, `"remoteSend":false`} {
		if !strings.Contains(w.Body.String(), field) {
			t.Fatal("incorrect capability or uninstall state")
		}
	}
}

func TestCompanionInstallRequiresLocalAuthAndExplicitConfirmation(t *testing.T) {
	s, h := newTestServer(t)
	f := &fakeCompanion{}
	s.d.Companion = f
	path, host := "/api/local/desktop-companion/install", "127.0.0.1:47831"
	hdr := map[string]string{"Cookie": CookieName + "=" + s.Token(), "Content-Type": "application/json", "Origin": "http://127.0.0.1:47831"}
	if w := do(h, "POST", path, host, nil, `{"confirmed":true}`); w.Code != http.StatusUnauthorized {
		t.Fatal(w.Code)
	}
	bad := map[string]string{"Cookie": hdr["Cookie"], "Content-Type": "application/json", "Origin": "https://evil.test"}
	if w := do(h, "POST", path, host, bad, `{"confirmed":true}`); w.Code != http.StatusForbidden {
		t.Fatal(w.Code)
	}
	if w := do(h, "POST", path, "evil.test:47831", hdr, `{"confirmed":true}`); w.Code != http.StatusForbidden {
		t.Fatal(w.Code)
	}
	for _, body := range []string{`{}`, `{"confirmed":false}`, `{"confirmed":"yes"}`, `{"confirmed":true,"nodePath":"evil.exe"}`} {
		if w := do(h, "POST", path, host, hdr, body); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", body, w.Code)
		}
	}
	if f.installs != 0 {
		t.Fatal("installer called before authorization")
	}
	w := do(h, "POST", path, host, hdr, `{"confirmed":true}`)
	if w.Code != http.StatusOK || f.installs != 1 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	for _, field := range []string{`"installed":true`, `"desktopControl":false`, `"remoteSend":false`} {
		if !strings.Contains(w.Body.String(), field) {
			t.Fatal("installation implied desktop capability")
		}
	}
	if len(s.d.Store.Get().LocalAccounts) != 0 {
		t.Fatal("installation required a model API")
	}
}

func TestCompanionPreviewNeverInstalls(t *testing.T) {
	s, h := newTestServer(t)
	f := &fakeCompanion{}
	s.d.Companion = f
	w := do(h, "GET", "/api/local/desktop-companion", "localhost:47831", map[string]string{"Cookie": CookieName + "=" + s.Token()}, "")
	if w.Code != http.StatusOK || f.previews != 1 || f.installs != 0 || !strings.Contains(w.Body.String(), `"desktopControl":false`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestCompanionErrorsDoNotReflectCredentialsOrPaths(t *testing.T) {
	s, h := newTestServer(t)
	f := &fakeCompanion{err: errors.New("private-fixture-key C:\\private-fixture-path")}
	s.d.Companion = f
	hdr := map[string]string{"Cookie": CookieName + "=" + s.Token(), "Content-Type": "application/json"}
	for _, target := range []struct{ method, path string }{{"GET", "/api/local/desktop-companion"}, {"POST", "/api/local/desktop-companion/install"}, {"GET", "/api/local/desktop-companion/uninstall"}, {"POST", "/api/local/desktop-companion/uninstall"}} {
		w := do(h, target.method, target.path, "localhost:47831", hdr, `{"confirmed":true}`)
		if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), "private-fixture") {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
}

func TestCompanionMissingBundleServiceFailsClosed(t *testing.T) {
	s, h := newTestServer(t)
	w := do(h, "POST", "/api/local/desktop-companion/install", "localhost:47831", map[string]string{"Cookie": CookieName + "=" + s.Token(), "Content-Type": "application/json"}, `{"confirmed":true}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatal(w.Code)
	}
}

type fakeCompanionControl struct {
	fakeCompanion
	connection   desktopcompanion.NativeConnection
	disconnects  int
	controlError error
}

func (f *fakeCompanionControl) NativeStatus(context.Context) (desktopcompanion.NativeConnection, error) {
	return f.connection, f.controlError
}
func (f *fakeCompanionControl) NativeDisconnect(context.Context) error {
	f.disconnects++
	return f.controlError
}

func TestCompanionControlReportsOnlyLiveValidScopeAndNoSecrets(t *testing.T) {
	s, h := newTestServer(t)
	key := "codex:01a0ae56-e9f6-7933-9ad4-5e08cd7874e5"
	f := &fakeCompanionControl{connection: desktopcompanion.NativeConnection{Active: true, ExpiresAt: time.Now().Add(time.Minute).UnixMilli(), SessionKeys: []string{key}}}
	s.d.Companion = f
	path, host := "/api/local/desktop-companion/control", "localhost:47831"
	hdr := map[string]string{"Cookie": CookieName + "=" + s.Token()}
	if w := do(h, "GET", path, host, nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatal(w.Code)
	}
	w := do(h, "GET", path, host, hdr, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"desktopControl":true`) || !strings.Contains(w.Body.String(), `"activationRequired":false`) || !strings.Contains(w.Body.String(), key) {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, bad := range []desktopcompanion.NativeConnection{
		{Active: true, ExpiresAt: time.Now().Add(-time.Second).UnixMilli(), SessionKeys: []string{key}},
		{Active: true, ExpiresAt: time.Now().Add(time.Minute).UnixMilli(), SessionKeys: []string{"fixture-private-token"}},
		{Active: true, ExpiresAt: time.Now().Add(31 * 24 * time.Hour).UnixMilli(), SessionKeys: []string{key}},
	} {
		f.connection = bad
		w = do(h, "GET", path, host, hdr, "")
		if !strings.Contains(w.Body.String(), `"desktopControl":false`) || strings.Contains(w.Body.String(), "fixture-private") {
			t.Fatal(w.Body.String())
		}
	}
}

func TestCompanionDisconnectRequiresLocalAuthConfirmationAndDoesNotTouchInstaller(t *testing.T) {
	s, h := newTestServer(t)
	f := &fakeCompanionControl{}
	s.d.Companion = f
	path, host := "/api/local/desktop-companion/disconnect", "localhost:47831"
	hdr := map[string]string{"Cookie": CookieName + "=" + s.Token(), "Content-Type": "application/json"}
	if w := do(h, "POST", path, host, nil, `{"confirmed":true}`); w.Code != http.StatusUnauthorized {
		t.Fatal(w.Code)
	}
	if w := do(h, "POST", path, host, hdr, `{"confirmed":false}`); w.Code != http.StatusBadRequest {
		t.Fatal(w.Code)
	}
	w := do(h, "POST", path, host, hdr, `{"confirmed":true}`)
	if w.Code != http.StatusOK || f.disconnects != 1 || f.installs != 0 || f.uninstalls != 0 || !strings.Contains(w.Body.String(), `"disconnected":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
	f.controlError = errors.New("fixture-private-path-and-key")
	w = do(h, "POST", path, host, hdr, `{"confirmed":true}`)
	if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), "fixture-private") {
		t.Fatal(w.Code, w.Body.String())
	}
}
