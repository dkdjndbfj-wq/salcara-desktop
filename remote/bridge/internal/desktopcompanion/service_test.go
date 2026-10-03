package desktopcompanion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func fixture(t *testing.T, config string) (*Service, Options) {
	t.Helper()
	root := t.TempDir()
	o := Options{BundleDir: filepath.Join(root, "bundle"), DataDir: filepath.Join(root, "data"), CodexHome: filepath.Join(root, "codex"), NodePath: filepath.Join(root, "node.exe")}
	for _, path := range []string{filepath.Join(o.BundleDir, "src"), filepath.Join(o.BundleDir, "node_modules", "do-not-copy"), o.CodexHome} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for path, data := range map[string]string{filepath.Join(o.BundleDir, "package.json"): `{"name":"salcara-desktop-companion","version":"` + Version + `"}`, filepath.Join(o.BundleDir, "LICENSE"): "MIT fixture", filepath.Join(o.BundleDir, "src", "index.mjs"): "// fake fixture: never execute", filepath.Join(o.BundleDir, "src", "permission-hook.mjs"): "// fake permission hook: never execute", filepath.Join(o.BundleDir, "src", "probe.mjs"): "// fake catalog-only module", filepath.Join(o.BundleDir, "src", "secret.txt"): "must not copy", filepath.Join(o.BundleDir, "node_modules", "do-not-copy", "data"): "must not copy", o.NodePath: "fake runtime: never execute"} {
		if err := os.WriteFile(path, []byte(data), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if config != "" {
		if err := os.WriteFile(filepath.Join(o.CodexHome, "config.toml"), []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return New(o), o
}
func configBytes(t *testing.T, o Options) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(o.CodexHome, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPreviewReadOnlyAndInstallPreservesSettingsAndHistory(t *testing.T) {
	original := "# user comment\r\nmodel = \"original-model\"\r\nmodel_provider = \"private-relay\"\r\n[model_providers.private-relay]\r\nbase_url = \"https://private.test/v1\"\r\nexperimental_bearer_token = \"fixture-secret\"\r\n[mcp_servers.existing]\r\ncommand = \"existing.exe\"\r\nargs = [\"already-configured\"]\r\n"
	s, o := fixture(t, original)
	retained := map[string]string{}
	for _, name := range []string{"auth.json", "history.jsonl", "state_5.sqlite", "sessions/original.jsonl", "skills/existing/SKILL.md"} {
		path := filepath.Join(o.CodexHome, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		data := "fixture-original-" + name
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		retained[path] = data
	}
	st, err := s.Preview(context.Background())
	if err != nil || !st.Available || st.Installed || st.RestartRequired || st.CatalogOnly {
		t.Fatalf("preview: %+v %v", st, err)
	}
	if _, err = os.Stat(o.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preview created installation state")
	}
	if string(configBytes(t, o)) != original {
		t.Fatal("preview changed original config")
	}
	st, err = s.Install(context.Background())
	if err != nil || !st.Installed || !st.RestartRequired || !st.Available {
		t.Fatalf("install: %+v %v", st, err)
	}
	b := configBytes(t, o)
	if !bytes.HasPrefix(b, []byte(original)) {
		t.Fatal("original TOML bytes were rewritten")
	}
	var doc map[string]any
	if toml.Unmarshal(b, &doc) != nil {
		t.Fatal("installed config is invalid TOML")
	}
	entry := doc["mcp_servers"].(map[string]any)[ServerName].(map[string]any)
	if entry["command"] != o.NodePath || !filepath.IsAbs(entry["cwd"].(string)) || entry["default_tools_approval_mode"] != "prompt" || !reflect.DeepEqual(entry["enabled_tools"], []any{ServerName, ConnectTool}) || entry["tool_timeout_sec"] != int64(2592060) || !reflect.DeepEqual(entry["env_vars"], []any{"CODEX_APP_TOOLS_PIPE_PATH", "CODEX_ELECTRON_RESOURCES_PATH"}) {
		t.Fatal("unsafe or nonabsolute stdio configuration")
	}
	if !reflect.DeepEqual(entry["args"], []any{filepath.Join(o.DataDir, "desktop-companion", "v"+Version, "src", "index.mjs")}) {
		t.Fatal("wrong owned entrypoint")
	}
	for path, want := range retained {
		got, e := os.ReadFile(path)
		if e != nil || string(got) != want {
			t.Fatal("original auth, history, DB or skill changed")
		}
	}
	for _, name := range []string{"node_modules", "src/secret.txt", "scripts"} {
		if _, e := os.Stat(filepath.Join(o.DataDir, "desktop-companion", "v"+Version, filepath.FromSlash(name))); !errors.Is(e, os.ErrNotExist) {
			t.Fatal("nonallowlisted payload was copied")
		}
	}
	backups, e := filepath.Glob(filepath.Join(o.DataDir, "desktop-companion", "backups", "*.toml"))
	if e != nil || len(backups) != 1 {
		t.Fatal("private backup missing")
	}
	backup, e := os.ReadFile(backups[0])
	if e != nil || string(backup) != original {
		t.Fatal("original backup not exact")
	}
	encoded, _ := json.Marshal(st)
	if bytes.Contains(encoded, []byte("fixture-secret")) || bytes.Contains(encoded, []byte(o.CodexHome)) || bytes.Contains(encoded, []byte(o.NodePath)) {
		t.Fatal("status leaked configuration or runtime paths")
	}
	st, err = s.Preview(context.Background())
	if err != nil || !st.Installed {
		t.Fatalf("owned preview: %+v %v", st, err)
	}
	st, err = s.Install(context.Background())
	if err != nil || !st.Installed || !bytes.Equal(configBytes(t, o), b) {
		t.Fatal("idempotent install modified config")
	}
}

func TestForeignSameNameAndInvalidTOMLRejectedWithoutMutation(t *testing.T) {
	for _, original := range []string{"[mcp_servers.salcara_desktop_probe]\ncommand = \"someone-else.exe\"\n", "model = \"fixture-secret\"\nmodel = \"duplicate\"\n", "model = [\"fixture-secret\"\n"} {
		t.Run(original[:min(len(original), 20)], func(t *testing.T) {
			s, o := fixture(t, original)
			for _, run := range []func(context.Context) (Status, error){s.Preview, s.Install} {
				st, err := run(context.Background())
				if err == nil || st.Installed || st.Available {
					t.Fatal("foreign or invalid config accepted")
				}
				if strings.Contains(err.Error(), "fixture-secret") {
					t.Fatal("parser error leaked input")
				}
			}
			if string(configBytes(t, o)) != original {
				t.Fatal("rejected config modified")
			}
			if _, err := os.Stat(o.DataDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("rejected operation staged a payload")
			}
		})
	}
}

func TestConcurrentConfigEditRefusesReplacement(t *testing.T) {
	s, o := fixture(t, "model = \"original\"\n")
	later := "model = \"user-edited\"\n[features]\nplugins = true\n"
	s.beforeCommit = func() {
		if err := os.WriteFile(filepath.Join(o.CodexHome, "config.toml"), []byte(later), 0600); err != nil {
			t.Fatal(err)
		}
	}
	st, err := s.Install(context.Background())
	if err == nil || st.Installed || !strings.Contains(err.Error(), "已改变") {
		t.Fatal("concurrent edit was not rejected")
	}
	if string(configBytes(t, o)) != later {
		t.Fatal("concurrent user edit overwritten")
	}
}

func TestInstallFailureRollsBackOnlyUnchangedOwnedConfig(t *testing.T) {
	for _, laterEdit := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "preserve-user-edit"}[laterEdit], func(t *testing.T) {
			original := "model = \"original\"\n"
			s, o := fixture(t, original)
			var later []byte
			s.afterConfigCommit = func() error {
				if laterEdit {
					later = append(configBytes(t, o), []byte("\n[features]\nplugins = true\n")...)
					if err := os.WriteFile(filepath.Join(o.CodexHome, "config.toml"), later, 0600); err != nil {
						t.Fatal(err)
					}
				}
				return errors.New("fixture failure must not leak")
			}
			st, err := s.Install(context.Background())
			if err == nil || st.Installed || strings.Contains(err.Error(), "fixture failure") {
				t.Fatal("failed transaction reported success or leaked error")
			}
			got := configBytes(t, o)
			if laterEdit && !bytes.Equal(got, later) || !laterEdit && string(got) != original {
				t.Fatal("unsafe transaction rollback")
			}
		})
	}
}

func TestAbsentOriginalConfigAndFailedInstallRestoreAbsence(t *testing.T) {
	s, o := fixture(t, "")
	s.afterConfigCommit = func() error { return errors.New("fixture") }
	if st, err := s.Install(context.Background()); err == nil || st.Installed {
		t.Fatal("fixture failed transaction succeeded")
	}
	if _, err := os.Stat(filepath.Join(o.CodexHome, "config.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("absent original config was not restored to absence")
	}
}

func TestOwnedEntryOrPayloadEditedRefusesRepair(t *testing.T) {
	for _, edit := range []string{"entry", "payload"} {
		t.Run(edit, func(t *testing.T) {
			s, o := fixture(t, "model = \"original\"\n")
			if _, err := s.Install(context.Background()); err != nil {
				t.Fatal(err)
			}
			if edit == "entry" {
				b := strings.Replace(string(configBytes(t, o)), "default_tools_approval_mode = \"prompt\"", "default_tools_approval_mode = \"approve\"", 1)
				if err := os.WriteFile(filepath.Join(o.CodexHome, "config.toml"), []byte(b), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(filepath.Join(o.DataDir, "desktop-companion", "v"+Version, "src", "index.mjs"), []byte("// later-user-edit"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before := configBytes(t, o)
			for _, run := range []func(context.Context) (Status, error){s.Preview, s.Install} {
				if st, err := run(context.Background()); err == nil || st.Installed {
					t.Fatal("edited owned data automatically repaired")
				}
			}
			if !bytes.Equal(configBytes(t, o), before) {
				t.Fatal("edited config overwritten")
			}
		})
	}
}

func TestUnavailableRuntimeBundleAndForeignDataRootFailClosed(t *testing.T) {
	for _, what := range []string{"node", "entrypoint", "permission-hook", "version", "root"} {
		t.Run(what, func(t *testing.T) {
			s, o := fixture(t, "model = \"original\"\n")
			switch what {
			case "node":
				s.o.NodePath = filepath.Join(t.TempDir(), "missing-node.exe")
			case "entrypoint":
				if err := os.Remove(filepath.Join(o.BundleDir, "src", "index.mjs")); err != nil {
					t.Fatal(err)
				}
			case "permission-hook":
				if err := os.Remove(filepath.Join(o.BundleDir, "src", "permission-hook.mjs")); err != nil {
					t.Fatal(err)
				}
			case "version":
				if err := os.WriteFile(filepath.Join(o.BundleDir, "package.json"), []byte(`{"version":"9.0.0"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "root":
				if err := os.MkdirAll(filepath.Join(o.DataDir, "desktop-companion"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			for _, run := range []func(context.Context) (Status, error){s.Preview, s.Install} {
				if st, err := run(context.Background()); err == nil || st.Installed || st.Available {
					t.Fatal("unavailable runtime/bundle accepted")
				}
			}
			if string(configBytes(t, o)) != "model = \"original\"\n" {
				t.Fatal("failed prerequisite modified config")
			}
		})
	}
}

func TestCancelledOperationAndSymlinkSourceDoNotWrite(t *testing.T) {
	s, o := fixture(t, "model = \"original\"\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if st, err := s.Install(ctx); err == nil || st.Installed {
		t.Fatal("canceled install succeeded")
	}
	path := filepath.Join(o.BundleDir, "src", "probe.mjs")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(o.NodePath, path); err != nil {
		t.Skip("symlink creation not permitted in this test environment")
	}
	if st, err := s.Install(context.Background()); err == nil || st.Installed {
		t.Fatal("symlink payload accepted")
	}
	if string(configBytes(t, o)) != "model = \"original\"\n" {
		t.Fatal("unsafe payload modified config")
	}
}

func TestInlineSealedMCPTableRejectedInPreviewBeforeWriting(t *testing.T) {
	original := "model = \"original\"\nmcp_servers = { existing = { command = \"existing.exe\" } }\n"
	s, o := fixture(t, original)
	for _, run := range []func(context.Context) (Status, error){s.Preview, s.Install} {
		if st, err := run(context.Background()); err == nil || st.Available || st.Installed {
			t.Fatal("sealed table was reported installable")
		}
	}
	if string(configBytes(t, o)) != original {
		t.Fatal("sealed table was rewritten")
	}
	if _, err := os.Stat(o.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("sealed table rejection wrote state")
	}
}

func TestCodexHomeEnvironmentUsesOnlyIsolatedFixture(t *testing.T) {
	s, o := fixture(t, "model = \"original\"\n")
	t.Setenv("CODEX_HOME", o.CodexHome)
	s.o.CodexHome = ""
	st, err := s.Install(context.Background())
	if err != nil || !st.Installed {
		t.Fatal("CODEX_HOME override was not respected")
	}
	if !bytes.HasPrefix(configBytes(t, o), []byte("model = \"original\"\n")) {
		t.Fatal("environment fixture original config changed")
	}
}

func TestRenamedBundledNodeAndBrandNewConfig(t *testing.T) {
	s, o := fixture(t, "")
	bundled := filepath.Join(filepath.Dir(o.NodePath), "SalcaraProbeNode.exe")
	if err := os.Rename(o.NodePath, bundled); err != nil {
		t.Fatal(err)
	}
	s.o.NodePath = bundled
	s.o.CodexHome = filepath.Join(t.TempDir(), "new-codex-home")
	st, err := s.Preview(context.Background())
	if err != nil || !st.Available || st.Installed {
		t.Fatal("bundled renamed Node prerequisite was not detected")
	}
	if _, err := os.Stat(s.o.CodexHome); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preview created a new Codex home")
	}
	st, err = s.Install(context.Background())
	if err != nil || !st.Installed {
		t.Fatalf("new config install: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(s.o.CodexHome, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if toml.Unmarshal(b, &doc) != nil || len(doc) != 2 {
		t.Fatal("new config included unexpected settings")
	}
	entry := doc["mcp_servers"].(map[string]any)[ServerName].(map[string]any)
	if entry["command"] != bundled {
		t.Fatal("bundled absolute Node was not used")
	}
}

func TestForeignOwnershipRecordIsNeverOverwritten(t *testing.T) {
	s, o := fixture(t, "model = \"original\"\n")
	p, _, _, err := s.prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = ensurePrivateRoot(p.root); err != nil {
		t.Fatal(err)
	}
	if err = privateDir(filepath.Dir(p.state)); err != nil {
		t.Fatal(err)
	}
	if err = writeExclusive(p.state, []byte(`{"owner":"foreign-fixture"}`)); err != nil {
		t.Fatal(err)
	}
	for _, run := range []func(context.Context) (Status, error){s.Preview, s.Install} {
		if st, err := run(context.Background()); err == nil || st.Available || st.Installed {
			t.Fatal("foreign ownership state accepted")
		}
	}
	state, err := os.ReadFile(p.state)
	if err != nil || string(state) != `{"owner":"foreign-fixture"}` {
		t.Fatal("foreign state overwritten")
	}
	if string(configBytes(t, o)) != "model = \"original\"\n" {
		t.Fatal("foreign state changed user config")
	}
}
