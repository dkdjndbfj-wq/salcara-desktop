package console

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/toolcfg"
)

func codexProfileFixture(t *testing.T) (*Server, http.Handler, string, map[string][]byte) {
	t.Helper()
	s, h, _ := managedLaunchFixture(t)
	dir := filepath.Dir(toolcfg.CodexConfigPath())
	original := map[string][]byte{
		"config.toml": []byte("# synthetic original\nmodel = \"original-model\"\n[projects.\"C:/fixture\"]\ntrust_level = \"trusted\"\n"),
		"auth.json":   []byte("{\"OPENAI_API_KEY\":\"synthetic-original-secret\"}\n"),
	}
	for name, data := range original {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return s, h, dir, original
}

func readCodexProfileView(t *testing.T, s *Server, h http.Handler) codexProfileView {
	t.Helper()
	w := localRequest(s, h, "GET", "/api/local/codex-api/profile", nil)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	for _, secret := range []string{"synthetic-original-secret", "synthetic-summary-secret", "sp-salcara-"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("status exposed a secret")
		}
	}
	var v codexProfileView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func codexProfileApplyBody(t *testing.T, v codexProfileView) map[string]any {
	t.Helper()
	if len(v.Clients) == 0 || len(v.Clients[0].Models) == 0 {
		t.Fatal("fixture has no profile clients")
	}
	return map[string]any{"clientId": v.Clients[0].ID, "clientRevision": v.Clients[0].Revision, "profileRevision": v.Profile.Revision, "model": v.Clients[0].Models[0]}
}

func applyCodexProfile(t *testing.T, s *Server, h http.Handler) codexProfileView {
	t.Helper()
	v := readCodexProfileView(t, s, h)
	w := localRequest(s, h, "POST", "/api/local/codex-api/profile/apply", codexProfileApplyBody(t, v))
	if w.Code != 200 || strings.Contains(w.Body.String(), "sp-salcara-") {
		t.Fatalf("apply %d: %s", w.Code, w.Body)
	}
	return readCodexProfileView(t, s, h)
}

func assertCodexProfileFiles(t *testing.T, dir string, want map[string][]byte) {
	t.Helper()
	for name, expected := range want {
		actual, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(actual, expected) {
			t.Fatalf("%s contents were not preserved", name)
		}
	}
}

func TestCodexServiceProfileApplyRestoreOwnsOnlyItsBackup(t *testing.T) {
	s, h, dir, original := codexProfileFixture(t)
	before := s.d.Store.Get()
	v := readCodexProfileView(t, s, h)
	if v.Profile.State != "idle" || v.Profile.HasBackup || v.Active {
		t.Fatal("initial status claims ownership")
	}
	assertCodexProfileFiles(t, dir, original)
	v = applyCodexProfile(t, s, h)
	if !v.Active || !v.Profile.HasBackup || v.Profile.Generation == "" {
		t.Fatal("applied profile not active")
	}
	c := s.d.Store.Get()
	if !reflect.DeepEqual(before.CodexAPI.Clients, c.CodexAPI.Clients) || !reflect.DeepEqual(before.ToolAPIApplied, c.ToolAPIApplied) {
		t.Fatal("profile changed manual keys or Workbench binding")
	}
	var auth struct {
		Key string `json:"OPENAI_API_KEY"`
	}
	data, _ := os.ReadFile(filepath.Join(dir, "auth.json"))
	if json.Unmarshal(data, &auth) != nil || auth.Key == "" || auth.Key != c.CodexServiceProfileCredential() {
		t.Fatal("profile credential not written")
	}
	text, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	if !strings.Contains(string(text), "/codex-api/profile/v1") || !strings.Contains(string(text), "trust_level") {
		t.Fatal("profile route or unrelated configuration missing")
	}
	if _, err := os.Stat(filepath.Join(s.d.Local.Dir, "default-backups", "codex.json")); !os.IsNotExist(err) {
		t.Fatal("profile borrowed Workbench backup")
	}
	w := localRequest(s, h, "POST", "/api/local/codex-api/profile/restore", map[string]any{"revision": v.Profile.Revision, "generation": v.Profile.Generation})
	if w.Code != 200 || s.d.Store.Get().CodexServiceProfileCredential() != "" {
		t.Fatalf("restore %d: %s", w.Code, w.Body)
	}
	assertCodexProfileFiles(t, dir, original)
}

func TestCodexServiceProfileStaleApplyCannotRevokeCurrentGeneration(t *testing.T) {
	s, h, _, _ := codexProfileFixture(t)
	before := readCodexProfileView(t, s, h)
	stale := codexProfileApplyBody(t, before)
	current := applyCodexProfile(t, s, h)
	key := s.d.Store.Get().CodexServiceProfileCredential()
	w := localRequest(s, h, "POST", "/api/local/codex-api/profile/apply", stale)
	if w.Code != 409 || s.d.Store.Get().CodexServiceProfileCredential() != key || !readCodexProfileView(t, s, h).Active {
		t.Fatal("stale apply retired the current profile")
	}
	w = localRequest(s, h, "POST", "/api/local/codex-api/profile/restore", map[string]any{"revision": before.Profile.Revision, "generation": current.Profile.Generation})
	if w.Code != 409 || s.d.Store.Get().CodexServiceProfileCredential() != key {
		t.Fatal("wrong revision revoked the current generation")
	}
	next := applyCodexProfile(t, s, h)
	if next.Profile.Generation == current.Profile.Generation {
		t.Fatal("reapply reused generation")
	}
	w = localRequest(s, h, "POST", "/api/local/codex-api/profile/restore", map[string]any{"revision": current.Profile.Revision, "generation": current.Profile.Generation})
	if w.Code != 409 || !readCodexProfileView(t, s, h).Active {
		t.Fatal("old restore touched newer generation")
	}
}

func TestCodexServiceProfileStopAndRestartRestore(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-process", true: "after-restart"}[restart], func(t *testing.T) {
			s, h, dir, original := codexProfileFixture(t)
			applyCodexProfile(t, s, h)
			if restart {
				s.d.Store.SetRuntimeGatewayKey("synthetic-new-process")
				s = New(s.d)
				h = s.Handler()
				t.Cleanup(s.CloseUsageLedger)
				if s.d.Store.Get().CodexServiceProfileCredential() != "" || s.d.Store.Get().CodexAPIActive() {
					t.Fatal("restart revived credential")
				}
			}
			w := localRequest(s, h, "POST", "/api/local/codex-api/enabled", map[string]any{"enabled": false})
			if w.Code != 200 || s.d.Store.Get().CodexAPIActive() {
				t.Fatalf("stop %d: %s", w.Code, w.Body)
			}
			assertCodexProfileFiles(t, dir, original)
		})
	}
}

func TestCodexServiceProfileExternalEditStopsWithoutOverwriting(t *testing.T) {
	s, h, dir, _ := codexProfileFixture(t)
	applyCodexProfile(t, s, h)
	custom := []byte("# external update\nmodel = \"external\"\n")
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), custom, 0600); err != nil {
		t.Fatal(err)
	}
	auth, _ := os.ReadFile(filepath.Join(dir, "auth.json"))
	w := localRequest(s, h, "POST", "/api/local/codex-api/enabled", map[string]any{"enabled": false})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "API 服务已停止") || s.d.Store.Get().CodexAPIActive() || s.d.Store.Get().CodexServiceProfileCredential() != "" {
		t.Fatalf("stop conflict %d: %s", w.Code, w.Body)
	}
	assertCodexProfileFiles(t, dir, map[string][]byte{"config.toml": custom, "auth.json": auth})
}

func TestCodexServiceProfileSourceChangesCannotReusePreview(t *testing.T) {
	s, h, dir, original := codexProfileFixture(t)
	v := readCodexProfileView(t, s, h)
	body := codexProfileApplyBody(t, v)
	if err := s.d.Store.Update(func(c *config.Config) error { c.LocalAccounts[0].Key = "synthetic-replaced-source"; return nil }); err != nil {
		t.Fatal(err)
	}
	w := localRequest(s, h, "POST", "/api/local/codex-api/profile/apply", body)
	if w.Code != 409 || s.d.Store.Get().CodexServiceProfileCredential() != "" {
		t.Fatal("stale source applied")
	}
	assertCodexProfileFiles(t, dir, original)
}

func TestCodexServiceProfileDoesNotStackWorkbenchBackup(t *testing.T) {
	s, h, dir, original := codexProfileFixture(t)
	v := readCodexProfileView(t, s, h)
	backup := filepath.Join(s.d.Local.Dir, "default-backups", "codex.json")
	if err := os.MkdirAll(filepath.Dir(backup), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup, []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}
	w := localRequest(s, h, "POST", "/api/local/codex-api/profile/apply", codexProfileApplyBody(t, v))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "工作台") {
		t.Fatalf("backup guard %d: %s", w.Code, w.Body)
	}
	assertCodexProfileFiles(t, dir, original)
	if err := os.Remove(backup); err != nil {
		t.Fatal(err)
	}
	applyCodexProfile(t, s, h)
	a := s.d.Store.Get().LocalAccounts[0]
	a.Kind = "codex"
	a.Model = "gpt-fixture"
	if err := s.d.Local.ApplyDefault(a); err == nil {
		t.Fatal("Workbench applied over service ownership")
	}
	if err := s.d.Local.RestoreDefault("codex"); err == nil {
		t.Fatal("Workbench restored service-owned profile")
	}
}

func TestCodexServiceProfileConcurrentApplySingleWinner(t *testing.T) {
	s, h, _, _ := codexProfileFixture(t)
	body := codexProfileApplyBody(t, readCodexProfileView(t, s, h))
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- localRequest(s, h, "POST", "/api/local/codex-api/profile/apply", body).Code
		}()
	}
	wg.Wait()
	close(codes)
	success := 0
	for code := range codes {
		if code == 200 {
			success++
		} else if code != 409 {
			t.Fatalf("unexpected %d", code)
		}
	}
	if success != 1 || !readCodexProfileView(t, s, h).Active {
		t.Fatal("duplicate apply did not have one winner")
	}
}

func TestCodexServiceProfileAdmissionDuringAPIWork(t *testing.T) {
	s, h, dir, original := codexProfileFixture(t)
	body := codexProfileApplyBody(t, readCodexProfileView(t, s, h))
	release, err := s.apiService.BeginManagedLaunch()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	w := localRequest(s, h, "POST", "/api/local/codex-api/profile/apply", body)
	if w.Code != 409 {
		t.Fatal("parallel configuration operation admitted")
	}
	assertCodexProfileFiles(t, dir, original)
}

func TestCodexServiceProfileEmptyStopLeavesNativeFilesUntouched(t *testing.T) {
	s, h, dir, original := codexProfileFixture(t)
	w := localRequest(s, h, "POST", "/api/local/codex-api/enabled", map[string]any{"enabled": false})
	if w.Code != 200 {
		t.Fatalf("stop %d: %s", w.Code, w.Body)
	}
	assertCodexProfileFiles(t, dir, original)
	p, err := toolcfg.NewCodexServiceProfile(s.d.Local.Dir, dir)
	if err != nil {
		t.Fatal(err)
	}
	if exists, err := p.HasRecord(); err != nil || exists {
		t.Fatal("empty stop claimed native profile")
	}
}

func TestCodexServiceProfileInvalidModelAndCancelledRequest(t *testing.T) {
	s, h, dir, original := codexProfileFixture(t)
	body := codexProfileApplyBody(t, readCodexProfileView(t, s, h))
	body["model"] = "missing-from-catalog"
	if w := localRequest(s, h, "POST", "/api/local/codex-api/profile/apply", body); w.Code != 409 {
		t.Fatal("unknown model accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.codexProfile.Status(ctx); err == nil {
		t.Fatal("cancelled profile request ignored")
	}
	assertCodexProfileFiles(t, dir, original)
}

func TestCodexServiceProfileArmCallbackChangeCannotReportSuccess(t *testing.T) {
	s, h, _, _ := codexProfileFixture(t)
	body := codexProfileApplyBody(t, readCodexProfileView(t, s, h))
	changed := false
	s.d.Store.OnChange(func(_, c config.Config) {
		if changed || c.CodexServiceProfile.Lease == "" {
			return
		}
		changed = true
		if err := s.d.Store.Update(func(next *config.Config) error {
			next.LocalAccounts[0].Models = []string{"replacement-model"}
			return nil
		}); err != nil {
			t.Error(err)
		}
	})
	w := localRequest(s, h, "POST", "/api/local/codex-api/profile/apply", body)
	if !changed || w.Code != 409 || s.d.Store.Get().CodexServiceProfileCredential() != "" {
		t.Fatalf("late source change accepted: %d", w.Code)
	}
}

func TestCodexServiceProfileFailedArmRestoresOriginal(t *testing.T) {
	s, h, dir, original := codexProfileFixture(t)
	applyCodexProfile(t, s, h)
	body := codexProfileApplyBody(t, readCodexProfileView(t, s, h))
	changed := false
	s.d.Store.OnChange(func(old, now config.Config) {
		if changed || old.CodexServiceProfile.Lease == "" || now.CodexServiceProfile.Lease != "" {
			return
		}
		changed = true
		if err := s.d.Store.Update(func(c *config.Config) error { c.LocalAccounts[0].Key = "synthetic-revoked-during-arm"; return nil }); err != nil {
			t.Error(err)
		}
	})
	w := localRequest(s, h, "POST", "/api/local/codex-api/profile/apply", body)
	if !changed || w.Code != 409 || !strings.Contains(w.Body.String(), "原配置已恢复") || s.d.Store.Get().CodexServiceProfileCredential() != "" {
		t.Fatalf("failed arm cleanup %d: %s", w.Code, w.Body)
	}
	assertCodexProfileFiles(t, dir, original)
}

func TestCodexServiceProfileStopSaveFailureStillRestores(t *testing.T) {
	s, h, dir, original := codexProfileFixture(t)
	applyCodexProfile(t, s, h)
	path := s.d.Store.Path()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	w := localRequest(s, h, "POST", "/api/local/codex-api/enabled", map[string]any{"enabled": false})
	if w.Code != 500 || s.d.Store.Get().CodexAPIActive() || s.d.Store.Get().CodexServiceProfileCredential() != "" {
		t.Fatal("failed save revived service")
	}
	assertCodexProfileFiles(t, dir, original)
}

func TestCodexServiceProfileGracefulCloseRestoresAndNeverRearms(t *testing.T) {
	s, h, dir, original := codexProfileFixture(t)
	applyCodexProfile(t, s, h)
	s.CloseAPIListeners()
	if s.d.Store.Get().CodexAPIActive() || s.d.Store.Get().CodexServiceProfileCredential() != "" {
		t.Fatal("closed server retained live credentials")
	}
	assertCodexProfileFiles(t, dir, original)
	s.CloseAPIListeners()
	assertCodexProfileFiles(t, dir, original)
	s.d.Store.OnChange(func(_, now config.Config) {
		if now.CodexAPIActive() {
			t.Error("closed server briefly published active state")
		}
	})
	w := localRequest(s, h, "POST", "/api/local/codex-api/enabled", map[string]any{"enabled": true})
	if w.Code != 503 || s.d.Store.Get().CodexAPIActive() {
		t.Fatal("closed server rearmed")
	}
	for _, route := range []string{"network", "images"} {
		if w := localRequest(s, h, "POST", "/api/local/codex-api/"+route, map[string]any{}); w.Code != 503 {
			t.Fatalf("closed server accepted %s settings: %d", route, w.Code)
		}
	}
	if got := networkRequest(s.apiService, "/codex-api/v1/models", "127.0.0.1:12345", "synthetic-summary-secret").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("closed root service still accepts requests: %d", got)
	}
}

func TestCodexServiceProfileGracefulClosePreservesExternalEdits(t *testing.T) {
	s, h, dir, _ := codexProfileFixture(t)
	applyCodexProfile(t, s, h)
	external := []byte("# synthetic user edit after apply\n")
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), external, 0600); err != nil {
		t.Fatal(err)
	}
	auth, _ := os.ReadFile(filepath.Join(dir, "auth.json"))
	s.CloseAPIListeners()
	if s.d.Store.Get().CodexAPIActive() || s.d.Store.Get().CodexServiceProfileCredential() != "" {
		t.Fatal("conflict kept service active")
	}
	assertCodexProfileFiles(t, dir, map[string][]byte{"config.toml": external, "auth.json": auth})
}
