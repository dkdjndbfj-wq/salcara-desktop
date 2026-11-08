package toolcfg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func serviceProfileFixture(t *testing.T, exists bool) *CodexServiceProfile {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "codex-home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	if exists {
		for name, data := range map[string]string{"config.toml": "# original comments\nmodel = 'original-model'\n[features]\nplugins = true\n", "auth.json": "{\"auth_mode\":\"chatgpt\",\"fixture\":\"synthetic-original\"}\n"} {
			if err := os.WriteFile(filepath.Join(home, name), []byte(data), 0640); err != nil {
				t.Fatal(err)
			}
		}
	}
	m, err := NewCodexServiceProfile(filepath.Join(root, "service-state"), home)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func serviceProfileParams(model string) CodexServiceProfileParams {
	return CodexServiceProfileParams{BaseURL: "http://127.0.0.1:48999/service/v1", Key: "synthetic-service-" + model, Model: model}
}
func serviceProfileAllow() error { return nil }
func serviceProfileReading(t *testing.T, m *CodexServiceProfile) CodexServiceProfileStatus {
	t.Helper()
	status, err := m.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return status
}
func serviceProfileFiles(t *testing.T, home string) []serviceProfileFile {
	t.Helper()
	d, err := openServiceProfileDir(home, false)
	if err != nil {
		t.Fatal(err)
	}
	defer d.close()
	var files []serviceProfileFile
	for _, name := range serviceProfileNames {
		f, err := d.read(name, serviceProfileFileLimit)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}
func serviceProfileApply(t *testing.T, m *CodexServiceProfile, model string) CodexServiceProfileStatus {
	t.Helper()
	before := serviceProfileReading(t, m)
	after, err := m.Apply(context.Background(), before.Revision, serviceProfileParams(model), serviceProfileAllow)
	if err != nil || after.State != "applied" || !after.HasBackup || after.Generation == "" {
		t.Fatalf("apply: %+v %v", after, err)
	}
	return after
}

func TestCodexServiceProfilePreservesExactOriginalAndSeparateBackups(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "existing"}[exists], func(t *testing.T) {
			m := serviceProfileFixture(t, exists)
			original := serviceProfileFiles(t, m.home)
			legacy := filepath.Join(m.home, "config.toml"+BackupSuffix)
			if err := os.WriteFile(legacy, []byte("unrelated-workbench-backup"), 0600); err != nil {
				t.Fatal(err)
			}
			if found, err := m.HasRecord(); found || err != nil {
				t.Fatalf("uninitialized record: %v %v", found, err)
			}
			initial := serviceProfileReading(t, m)
			if initial.State != "idle" || initial.HasBackup || initial.Generation != "" {
				t.Fatalf("initial: %+v", initial)
			}
			if found, err := m.HasRecord(); found || err != nil {
				t.Fatal("preview became a takeover", found, err)
			}
			applied := serviceProfileApply(t, m, "model-a")
			if found, err := m.HasRecord(); !found || err != nil {
				t.Fatal("takeover record missing", found, err)
			}
			if err := RequireCodexProfileUnowned(m.home); !errors.Is(err, ErrCodexProfileOwned) {
				t.Fatal("service ownership not fenced", err)
			}
			files := serviceProfileFiles(t, m.home)
			if !bytes.Contains(files[0].Data, []byte("model-a")) || !bytes.Contains(files[1].Data, []byte("synthetic-service-model-a")) || !files[2].Exists {
				t.Fatal("expected API profile missing")
			}
			public, _ := json.Marshal(applied)
			if strings.Contains(string(public), "synthetic") || strings.Contains(string(public), m.home) || strings.Contains(string(public), serviceProfileHash(files[1].Data)) {
				t.Fatal("status leaked credential, path or content hash")
			}
			restored, err := m.Restore(context.Background(), applied.Revision, applied.Generation, serviceProfileAllow)
			if err != nil || restored.State != "idle" || restored.HasBackup {
				t.Fatalf("restore: %+v %v", restored, err)
			}
			if !equalServiceProfileProofs(serviceProfileProofs(original), serviceProfileProofs(serviceProfileFiles(t, m.home))) {
				t.Fatal("original bytes, existence or permissions not restored exactly")
			}
			if found, err := m.HasRecord(); found || err != nil {
				t.Fatal("completed restore remained active", found, err)
			}
			if err := RequireCodexProfileUnowned(m.home); err != nil {
				t.Fatal(err)
			}
			if raw, err := os.ReadFile(legacy); err != nil || string(raw) != "unrelated-workbench-backup" {
				t.Fatal("legacy backup was touched")
			}
			calls := 0
			again, err := m.Restore(context.Background(), applied.Revision, applied.Generation, func() error { calls++; return errors.New("must not revoke twice") })
			if err != nil || again.Revision != restored.Revision || calls != 0 {
				t.Fatalf("restore duplicate: %+v %v calls=%d", again, err, calls)
			}
		})
	}
}

func TestCodexServiceProfileUpdatesKeepFirstBackupAndRejectOldGenerations(t *testing.T) {
	m := serviceProfileFixture(t, true)
	original := serviceProfileFiles(t, m.home)
	a := serviceProfileApply(t, m, "model-a")
	b := serviceProfileApply(t, m, "model-b")
	c := serviceProfileApply(t, m, "model-a")
	if a.Generation == b.Generation || a.Generation == c.Generation || a.Revision == c.Revision {
		t.Fatal("A-B-A revived an old generation")
	}
	for _, prior := range []CodexServiceProfileStatus{a, b} {
		calls := 0
		if _, err := m.Restore(context.Background(), prior.Revision, c.Generation, func() error { calls++; return nil }); !errors.Is(err, ErrCodexProfileStale) || calls != 0 {
			t.Fatal("stale revision revoked active generation", err, calls)
		}
		if _, err := m.Restore(context.Background(), c.Revision, prior.Generation, func() error { calls++; return nil }); !errors.Is(err, ErrCodexProfileStale) || calls != 0 {
			t.Fatal("stale generation revoked active generation", err, calls)
		}
	}
	if _, err := m.Restore(context.Background(), c.Revision, c.Generation, serviceProfileAllow); err != nil {
		t.Fatal(err)
	}
	if !equalServiceProfileProofs(serviceProfileProofs(original), serviceProfileProofs(serviceProfileFiles(t, m.home))) {
		t.Fatal("update overwrote first original")
	}
	newest := serviceProfileApply(t, m, "model-a")
	if _, err := m.Restore(context.Background(), c.Revision, c.Generation, serviceProfileAllow); !errors.Is(err, ErrCodexProfileStale) {
		t.Fatal("completed old restore revived", err)
	}
	if serviceProfileReading(t, m).Generation != newest.Generation {
		t.Fatal("stale restore modified newest profile")
	}
}

func TestCodexServiceProfileExternalEditsRejectWholeRestoreBeforeGuard(t *testing.T) {
	for _, name := range []string{"auth.json", "config.toml", serviceProfileMarkerName} {
		t.Run(name, func(t *testing.T) {
			m := serviceProfileFixture(t, true)
			serviceProfileApply(t, m, "model-a")
			if err := os.WriteFile(filepath.Join(m.home, name), []byte("external-edit"), 0600); err != nil {
				t.Fatal(err)
			}
			before := serviceProfileFiles(t, m.home)
			status := serviceProfileReading(t, m)
			if status.State != "conflict" {
				t.Fatal("external edit accepted", status)
			}
			calls := 0
			if _, err := m.Restore(context.Background(), status.Revision, status.Generation, func() error { calls++; return nil }); !errors.Is(err, ErrCodexProfileChanged) || calls != 0 {
				t.Fatal("restore admitted external edit or revoked guard", err, calls)
			}
			if !equalServiceProfileProofs(serviceProfileProofs(before), serviceProfileProofs(serviceProfileFiles(t, m.home))) {
				t.Fatal("partial restoration overwrote other file")
			}
		})
	}
}

func TestCodexServiceProfileRevisionBindsInitialContentsAndUnknownOwner(t *testing.T) {
	m := serviceProfileFixture(t, true)
	initial := serviceProfileReading(t, m)
	if err := os.WriteFile(filepath.Join(m.home, "auth.json"), []byte("new-original-auth"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Apply(context.Background(), initial.Revision, serviceProfileParams("a"), serviceProfileAllow); !errors.Is(err, ErrCodexProfileStale) {
		t.Fatal("initial reading did not bind bytes", err)
	}
	serviceProfileApply(t, m, "model-a")
	other, err := NewCodexServiceProfile(filepath.Join(t.TempDir(), "other-state"), m.home)
	if err != nil {
		t.Fatal(err)
	}
	status := serviceProfileReading(t, other)
	if status.State != "conflict" || status.HasBackup {
		t.Fatal("foreign owner adopted", status)
	}
	if _, err := other.Apply(context.Background(), status.Revision, serviceProfileParams("b"), serviceProfileAllow); err == nil {
		t.Fatal("foreign service overwrote managed files")
	}
}

func TestCodexServiceProfileRollbackAndGuardRevocation(t *testing.T) {
	for _, at := range []string{"journal", serviceProfileMarkerName, "auth.json", "config.toml", "commit"} {
		t.Run(at, func(t *testing.T) {
			m := serviceProfileFixture(t, true)
			original := serviceProfileFiles(t, m.home)
			status := serviceProfileReading(t, m)
			m.checkpoint = func(point string) error {
				if point == at {
					return ErrCodexProfileWrite
				}
				return nil
			}
			if _, err := m.Apply(context.Background(), status.Revision, serviceProfileParams("a"), serviceProfileAllow); err == nil {
				t.Fatal("injected write failure accepted")
			}
			if !equalServiceProfileProofs(serviceProfileProofs(original), serviceProfileProofs(serviceProfileFiles(t, m.home))) {
				t.Fatal("failed apply did not rollback full original")
			}
			if serviceProfileReading(t, m).State != "idle" {
				t.Fatal("successful rollback left journal")
			}
		})
	}
	m := serviceProfileFixture(t, true)
	original := serviceProfileFiles(t, m.home)
	status := serviceProfileReading(t, m)
	var allowed atomic.Bool
	allowed.Store(true)
	m.checkpoint = func(point string) error {
		if point == "auth.json" {
			allowed.Store(false)
		}
		return nil
	}
	guard := func() error {
		if !allowed.Load() {
			return errors.New("secret must not leak")
		}
		return nil
	}
	after, err := m.Apply(context.Background(), status.Revision, serviceProfileParams("a"), guard)
	if !errors.Is(err, ErrCodexProfileAuthorization) || strings.Contains(after.Error, "secret") {
		t.Fatal("live guard missed or leaked", after, err)
	}
	if !equalServiceProfileProofs(serviceProfileProofs(original), serviceProfileProofs(serviceProfileFiles(t, m.home))) {
		t.Fatal("revoked apply not rolled back")
	}
}

func TestCodexServiceProfileCrashJournalRecoversOnlyKnownBytes(t *testing.T) {
	for _, at := range []string{"journal", serviceProfileMarkerName, "auth.json", "config.toml", "commit"} {
		t.Run(at, func(t *testing.T) {
			m := serviceProfileFixture(t, true)
			original := serviceProfileFiles(t, m.home)
			initial := serviceProfileReading(t, m)
			crash := new(int)
			m.checkpoint = func(point string) error {
				if point == at {
					panic(crash)
				}
				return nil
			}
			func() {
				defer func() {
					if got := recover(); got != crash {
						t.Fatalf("unexpected crash: %v", got)
					}
				}()
				_, _ = m.Apply(context.Background(), initial.Revision, serviceProfileParams("a"), serviceProfileAllow)
			}()
			fresh, err := NewCodexServiceProfile(filepath.Dir(filepath.Dir(m.state)), m.home)
			if err != nil {
				t.Fatal(err)
			}
			status := serviceProfileReading(t, fresh)
			if status.State != "recovery_required" || status.Generation == "" || !status.HasBackup {
				t.Fatal("interruption not recorded", status)
			}
			if _, err := fresh.Apply(context.Background(), status.Revision, serviceProfileParams("b"), serviceProfileAllow); !errors.Is(err, ErrCodexProfileRecovery) {
				t.Fatal("interrupted operation allowed new apply", err)
			}
			if _, err := fresh.Restore(context.Background(), status.Revision, status.Generation, serviceProfileAllow); err != nil {
				t.Fatal(err)
			}
			if !equalServiceProfileProofs(serviceProfileProofs(original), serviceProfileProofs(serviceProfileFiles(t, m.home))) {
				t.Fatal("crash recovery lost original")
			}
		})
	}
	m := serviceProfileFixture(t, true)
	status := serviceProfileReading(t, m)
	m.checkpoint = func(at string) error {
		if at == "auth.json" {
			if err := os.WriteFile(filepath.Join(m.home, "config.toml"), []byte("external-after-partial"), 0600); err != nil {
				t.Fatal(err)
			}
			return ErrCodexProfileWrite
		}
		return nil
	}
	if _, err := m.Apply(context.Background(), status.Revision, serviceProfileParams("a"), serviceProfileAllow); !errors.Is(err, ErrCodexProfileRecovery) {
		t.Fatal("unprovable rollback reported success", err)
	}
	before := serviceProfileFiles(t, m.home)
	m.checkpoint = nil
	status = serviceProfileReading(t, m)
	if status.State != "conflict" {
		t.Fatal("external partial state accepted", status)
	}
	if _, err := m.Restore(context.Background(), status.Revision, status.Generation, serviceProfileAllow); !errors.Is(err, ErrCodexProfileChanged) {
		t.Fatal("recovery overwrote external file", err)
	}
	if !equalServiceProfileProofs(serviceProfileProofs(before), serviceProfileProofs(serviceProfileFiles(t, m.home))) {
		t.Fatal("conflicting recovery changed files")
	}
}

func TestCodexServiceProfileConcurrentManagersUseSingleCAS(t *testing.T) {
	m := serviceProfileFixture(t, true)
	other, err := NewCodexServiceProfile(filepath.Dir(filepath.Dir(m.state)), m.home)
	if err != nil {
		t.Fatal(err)
	}
	status := serviceProfileReading(t, m)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, manager := range []*CodexServiceProfile{m, other} {
		wg.Add(1)
		go func(manager *CodexServiceProfile) {
			defer wg.Done()
			_, err := manager.Apply(context.Background(), status.Revision, serviceProfileParams("a"), serviceProfileAllow)
			results <- err
		}(manager)
	}
	wg.Wait()
	close(results)
	success, stale := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrCodexProfileStale) {
			stale++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatal("CAS admitted two concurrent writers", success, stale)
	}
}

func TestCodexServiceProfileOSLockAndProcessExit(t *testing.T) {
	m := serviceProfileFixture(t, false)
	release, err := LockCodexProfile(context.Background(), m.home)
	if err != nil {
		t.Fatal(err)
	}
	run := func(want string) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestCodexServiceProfileLockChild$", "-test.count=1")
		cmd.Env = append(os.Environ(), "SALCARA_TEST_PROFILE_HOME="+m.home, "SALCARA_TEST_PROFILE_LOCK="+want)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("lock helper: %v %s", err, out)
		}
	}
	run("blocked")
	release()
	release()
	run("exit-held")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err = LockCodexProfile(ctx, m.home)
	if err != nil {
		t.Fatal("crashed process retained lock", err)
	}
	release()
}

func TestCodexServiceProfileLockChild(t *testing.T) {
	home, mode := os.Getenv("SALCARA_TEST_PROFILE_HOME"), os.Getenv("SALCARA_TEST_PROFILE_LOCK")
	if home == "" || mode == "" {
		t.Skip("subprocess only")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	release, err := LockCodexProfile(ctx, home)
	if mode == "blocked" {
		if !errors.Is(err, context.DeadlineExceeded) {
			if release != nil {
				release()
			}
			t.Fatal("other process entered lock", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	os.Exit(0) // Deliberately skip release: the kernel must release this lease.
}

func TestCodexServiceProfileFinalPublicationCannotReturnConflictSuccess(t *testing.T) {
	for _, action := range []string{"apply", "restore"} {
		for _, boundary := range []string{"commit", "published"} {
			t.Run(action+"/"+boundary, func(t *testing.T) {
				m := serviceProfileFixture(t, true)
				status := serviceProfileReading(t, m)
				if action == "restore" {
					status = serviceProfileApply(t, m, "a")
				}
				m.checkpoint = func(at string) error {
					if at == boundary {
						return os.WriteFile(filepath.Join(m.home, "config.toml"), []byte("external-final-edit"), 0600)
					}
					return nil
				}
				var result CodexServiceProfileStatus
				var err error
				if action == "apply" {
					result, err = m.Apply(context.Background(), status.Revision, serviceProfileParams("a"), serviceProfileAllow)
				} else {
					result, err = m.Restore(context.Background(), status.Revision, status.Generation, serviceProfileAllow)
				}
				if err == nil {
					t.Fatalf("conflicting terminal receipt: %+v", result)
				}
				if raw, err := os.ReadFile(filepath.Join(m.home, "config.toml")); err != nil || string(raw) != "external-final-edit" {
					t.Fatal("final external edit overwritten")
				}
			})
		}
	}
}

func TestCodexServiceProfileRestoreFailureAndCrashRetainRecoverableOwnership(t *testing.T) {
	for _, crash := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "restart"}[crash], func(t *testing.T) {
			m := serviceProfileFixture(t, true)
			original := serviceProfileFiles(t, m.home)
			serviceProfileApply(t, m, "a")
			applied := serviceProfileApply(t, m, "b")
			current := serviceProfileFiles(t, m.home)
			signal := new(int)
			m.checkpoint = func(at string) error {
				if at == "auth.json" {
					if crash {
						panic(signal)
					}
					return ErrCodexProfileWrite
				}
				return nil
			}
			if crash {
				func() {
					defer func() {
						if recover() != signal {
							t.Fatal("expected simulated process crash")
						}
					}()
					_, _ = m.Restore(context.Background(), applied.Revision, applied.Generation, serviceProfileAllow)
				}()
			} else {
				if _, err := m.Restore(context.Background(), applied.Revision, applied.Generation, serviceProfileAllow); err == nil {
					t.Fatal("restore failure accepted")
				}
				if !equalServiceProfileProofs(serviceProfileProofs(current), serviceProfileProofs(serviceProfileFiles(t, m.home))) {
					t.Fatal("failed restore lost current owned profile")
				}
			}
			m.checkpoint = nil
			status := serviceProfileReading(t, m)
			if crash && status.State != "recovery_required" {
				t.Fatal("restore journal lost", status)
			}
			if _, err := m.Restore(context.Background(), status.Revision, status.Generation, serviceProfileAllow); err != nil {
				t.Fatal(err)
			}
			if !equalServiceProfileProofs(serviceProfileProofs(original), serviceProfileProofs(serviceProfileFiles(t, m.home))) {
				t.Fatal("restore recovery lost first original")
			}
		})
	}
}

func TestCodexServiceProfileBoundsLinksAndCorruptManifestFailClosed(t *testing.T) {
	for _, variant := range []string{"oversized", "directory", "hardlink", "symlink", "manifest-link", "manifest-corrupt"} {
		t.Run(variant, func(t *testing.T) {
			m := serviceProfileFixture(t, true)
			serviceProfileReading(t, m)
			outside := filepath.Join(t.TempDir(), "outside.json")
			if err := os.WriteFile(outside, []byte("outside-synthetic-secret"), 0600); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(m.home, "auth.json")
			switch variant {
			case "oversized":
				if err := os.WriteFile(target, bytes.Repeat([]byte("x"), serviceProfileFileLimit+1), 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
			case "manifest-corrupt":
				target = filepath.Join(m.state, serviceProfileManifestName)
				if err := os.WriteFile(target, []byte(`{"version":1,"state":"applied","original":"invalid"}`), 0600); err != nil {
					t.Fatal(err)
				}
			default:
				if variant == "manifest-link" {
					target = filepath.Join(m.state, serviceProfileManifestName)
				}
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				if variant == "hardlink" {
					if err := os.Link(outside, target); err != nil {
						t.Skip("hardlinks unavailable:", err)
					}
				} else if err := os.Symlink(outside, target); err != nil {
					t.Skip("symlinks unavailable:", err)
				}
			}
			if status, err := m.Status(context.Background()); err == nil || status.Error == "" || strings.Contains(status.Error, "outside") {
				t.Fatal("unsafe path or record accepted", status, err)
			}
			if raw, err := os.ReadFile(outside); err != nil || string(raw) != "outside-synthetic-secret" {
				t.Fatal("outside file modified")
			}
		})
	}
}

func TestCodexServiceProfileHasRecordNeverReadsTargetCredentials(t *testing.T) {
	for _, applied := range []bool{false, true} {
		m := serviceProfileFixture(t, true)
		serviceProfileReading(t, m)
		if applied {
			serviceProfileApply(t, m, "a")
		}
		path := filepath.Join(m.home, "auth.json")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if found, err := m.HasRecord(); err != nil || found != applied {
			t.Fatal("metadata-only query opened malformed target", found, err)
		}
	}
}

func TestCodexServiceProfileRejectsUnauthorisedAndAmbiguousInputsWithoutChanges(t *testing.T) {
	for _, source := range []string{"model = [", "profile = 'named'\n", "model_provider = 'ollama'\n", "model_provider = 'custom'\n[model_providers.custom.auth]\ncommand = 'helper'\n", "model_provider = 'custom'\n[model_providers.custom.http_headers]\nAuthorization = 'unrelated'\n"} {
		m := serviceProfileFixture(t, true)
		if err := os.WriteFile(filepath.Join(m.home, "config.toml"), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
		before := serviceProfileFiles(t, m.home)
		status := serviceProfileReading(t, m)
		if _, err := m.Apply(context.Background(), status.Revision, serviceProfileParams("a"), serviceProfileAllow); !errors.Is(err, ErrCodexProfileInvalid) {
			t.Fatal("ambiguous native config accepted", err)
		}
		if !equalServiceProfileProofs(serviceProfileProofs(before), serviceProfileProofs(serviceProfileFiles(t, m.home))) {
			t.Fatal("invalid apply modified files")
		}
	}
	m := serviceProfileFixture(t, true)
	status := serviceProfileReading(t, m)
	before := serviceProfileFiles(t, m.home)
	if _, err := m.Apply(context.Background(), status.Revision, serviceProfileParams("a"), nil); !errors.Is(err, ErrCodexProfileAuthorization) {
		t.Fatal("nil authorization accepted", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Apply(ctx, status.Revision, serviceProfileParams("a"), serviceProfileAllow); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled apply accepted", err)
	}
	if !equalServiceProfileProofs(serviceProfileProofs(before), serviceProfileProofs(serviceProfileFiles(t, m.home))) {
		t.Fatal("unauthorized apply modified files")
	}
}
