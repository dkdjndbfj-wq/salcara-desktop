package desktopcompanion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestUninstallPreservesLaterSettingsBytesHistoryPayloadAndBackups(t *testing.T) {
	original := "# user's original bytes\r\nmodel = \"original\"\r\n[model_providers.relay]\r\nbase_url = \"https://private.test/v1\"\r\nexperimental_bearer_token = \"fixture-secret\"\r\n[mcp_servers.existing]\r\ncommand = \"existing.exe\"\r\n"
	s, o := fixture(t, original)
	if _, err := s.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	installed := configBytes(t, o)
	tail := "\n# user added after installation\r\n[model_providers.new_relay]\r\nbase_url = \"https://new.test/v1\"\r\n[mcp_servers.new_server]\r\ncommand = \"another.exe\"\r\n[features]\r\nplugins = true\r\n[profiles.new_profile]\r\nmodel = \"grok-fixture\"\r\n"
	current := append(append([]byte{}, installed...), []byte(tail)...)
	if err := os.WriteFile(filepath.Join(o.CodexHome, "config.toml"), current, 0600); err != nil {
		t.Fatal(err)
	}
	p, _, _, err := s.prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := os.ReadFile(p.state)
	if err != nil {
		t.Fatal(err)
	}
	history := filepath.Join(o.CodexHome, "history.jsonl")
	if err := os.WriteFile(history, []byte("fixture original history"), 0600); err != nil {
		t.Fatal(err)
	}
	st, err := s.PreviewUninstall(context.Background())
	if err != nil || !st.Installed || !st.UninstallAvailable || st.Available || st.RestartRequired {
		t.Fatalf("uninstall preview: %+v %v", st, err)
	}
	if !bytes.Equal(configBytes(t, o), current) {
		t.Fatal("uninstall preview rewrote config")
	}
	st, err = s.Uninstall(context.Background())
	if err != nil || st.Installed || st.UninstallAvailable || !st.RestartRequired || !strings.Contains(st.Message, "保留") {
		t.Fatalf("uninstall: %+v %v", st, err)
	}
	want := original + "\n" + tail
	if got := string(configBytes(t, o)); got != want {
		t.Fatal("uninstall did not preserve every nonowned configuration byte")
	}
	var doc map[string]any
	if err := toml.Unmarshal(configBytes(t, o), &doc); err != nil {
		t.Fatal(err)
	}
	if _, exists := doc["mcp_servers"].(map[string]any)[ServerName]; exists {
		t.Fatal("owned MCP entry still exists")
	}
	if _, err := os.Stat(p.state); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("active ownership record was not moved")
	}
	archives, err := filepath.Glob(filepath.Join(p.root, "archives", "*.json"))
	if err != nil || len(archives) != 1 {
		t.Fatal("recoverable ownership archive missing")
	}
	archived, err := os.ReadFile(archives[0])
	if err != nil || !bytes.Equal(archived, state) {
		t.Fatal("archive was not exact ownership record")
	}
	backups, err := filepath.Glob(filepath.Join(p.root, "backups", "*.toml"))
	if err != nil || len(backups) != 2 {
		t.Fatal("install and uninstall backups not both retained")
	}
	for _, name := range backups {
		if strings.HasPrefix(filepath.Base(name), "uninstall-") {
			b, e := os.ReadFile(name)
			if e != nil || !bytes.Equal(b, current) {
				t.Fatal("uninstall backup does not contain exact current config")
			}
		}
	}
	if b, err := os.ReadFile(history); err != nil || string(b) != "fixture original history" {
		t.Fatal("history changed")
	}
	if _, _, err := readBundle(p.payload); err != nil {
		t.Fatal("retained payload changed")
	}
	if _, err := os.Stat(o.NodePath); err != nil {
		t.Fatal("shared runtime removed")
	}
	encoded, _ := json.Marshal(st)
	if bytes.Contains(encoded, []byte("fixture-secret")) || bytes.Contains(encoded, []byte(o.CodexHome)) {
		t.Fatal("uninstall status leaked private config or path")
	}
	before := configBytes(t, o)
	st, err = s.Uninstall(context.Background())
	if err != nil || st.Installed || st.RestartRequired || !bytes.Equal(before, configBytes(t, o)) {
		t.Fatal("repeated uninstall was not a no-op")
	}
	st, err = s.Install(context.Background())
	if err != nil || !st.Installed || !st.UninstallAvailable || !bytes.HasPrefix(configBytes(t, o), before) {
		t.Fatalf("reinstall failed or rewrote user settings: %+v %v", st, err)
	}
}

func TestUninstallDoesNotDependOnBundleRuntimeOrPayload(t *testing.T) {
	for _, missing := range []string{"bundle", "runtime", "payload", "all"} {
		t.Run(missing, func(t *testing.T) {
			s, o := fixture(t, "model = \"original\"\n")
			if _, err := s.Install(context.Background()); err != nil {
				t.Fatal(err)
			}
			if missing == "bundle" || missing == "all" {
				s.o.BundleDir = filepath.Join(t.TempDir(), "missing-bundle")
			}
			if missing == "runtime" || missing == "all" {
				if err := os.Remove(o.NodePath); err != nil {
					t.Fatal(err)
				}
				s.o.NodePath = filepath.Join(t.TempDir(), "missing-node.exe")
			}
			if missing == "payload" || missing == "all" {
				if err := os.Rename(filepath.Join(o.DataDir, "desktop-companion", "v"+Version), filepath.Join(o.DataDir, "retained-payload")); err != nil {
					t.Fatal(err)
				}
			}
			if st, err := s.PreviewUninstall(context.Background()); err != nil || !st.UninstallAvailable {
				t.Fatalf("missing dependency prevented uninstall preview: %+v %v", st, err)
			}
			if st, err := s.Uninstall(context.Background()); err != nil || st.Installed {
				t.Fatalf("missing dependency prevented uninstall: %+v %v", st, err)
			}
		})
	}
}

func TestUninstallForeignOrEditedEntriesRefusedWithoutMutation(t *testing.T) {
	for _, edit := range []string{"foreign", "policy", "formatting", "comment", "marker", "nested-table", "record", "duplicate-marker"} {
		t.Run(edit, func(t *testing.T) {
			s, o := fixture(t, "model = \"original\"\n")
			if edit != "foreign" {
				if _, err := s.Install(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			current := string(configBytes(t, o))
			switch edit {
			case "foreign":
				current += "[mcp_servers.salcara_desktop_probe]\ncommand = \"foreign.exe\"\n"
			case "policy":
				current = strings.Replace(current, "default_tools_approval_mode = \"prompt\"", "default_tools_approval_mode = \"approve\"", 1)
			case "formatting":
				current = strings.Replace(current, "enabled = true", "enabled=true", 1)
			case "comment":
				current = strings.Replace(current, "enabled = true", "# user note\nenabled = true", 1)
			case "marker":
				current = strings.Replace(current, "# Salcara desktop companion installer:", "# changed owner:", 1)
			case "nested-table":
				current += "[mcp_servers.salcara_desktop_probe.env]\nUSER_SETTING = \"keep\"\n"
			case "record":
				p, _, _, err := s.prepare(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p.state, []byte(`{"owner":"foreign"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "duplicate-marker":
				for _, line := range strings.Split(current, "\n") {
					if strings.HasPrefix(line, "# Salcara desktop companion installer: ") {
						current += line + "\n"
						break
					}
				}
			}
			if err := os.WriteFile(filepath.Join(o.CodexHome, "config.toml"), []byte(current), 0600); err != nil {
				t.Fatal(err)
			}
			for _, run := range []func(context.Context) (Status, error){s.PreviewUninstall, s.Uninstall} {
				if st, err := run(context.Background()); err == nil || st.UninstallAvailable {
					t.Fatal("foreign or user-edited entry was accepted")
				}
			}
			if string(configBytes(t, o)) != current {
				t.Fatal("rejected uninstall rewrote user config")
			}
			if archives, _ := filepath.Glob(filepath.Join(o.DataDir, "desktop-companion", "archives", "*.json")); len(archives) != 0 {
				t.Fatal("rejected uninstall archived ownership")
			}
		})
	}
}

func TestUninstallUsesASTNotFakeHeadersInsideStrings(t *testing.T) {
	s, o := fixture(t, "model = \"original\"\n")
	if _, err := s.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	installed := string(configBytes(t, o))
	block := installed[strings.Index(installed, "# Salcara desktop companion installer:"):]
	tail := "\n[profiles.fake]\nnote = '''\n" + block + "'''\n"
	if err := os.WriteFile(filepath.Join(o.CodexHome, "config.toml"), []byte(installed+tail), 0600); err != nil {
		t.Fatal(err)
	}
	if st, err := s.Uninstall(context.Background()); err != nil || st.Installed {
		t.Fatalf("fake header inside multiline string confused removal: %+v %v", st, err)
	}
	if got := string(configBytes(t, o)); !strings.HasSuffix(got, tail) {
		t.Fatal("uninstall modified the user's multiline string")
	}
}

func TestUninstallNoFinalOriginalNewlineKeepsLaterTableValid(t *testing.T) {
	s, o := fixture(t, "model = \"original\"")
	if _, err := s.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	tail := "[features]\nplugins = true\n"
	if err := os.WriteFile(filepath.Join(o.CodexHome, "config.toml"), append(configBytes(t, o), []byte(tail)...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := string(configBytes(t, o)); got != "model = \"original\"\n"+tail {
		t.Fatal("original no-newline settings or later table were damaged")
	}
}

func TestUninstallConcurrentConfigAndRecordChangesRefused(t *testing.T) {
	for _, what := range []string{"config", "record"} {
		t.Run(what, func(t *testing.T) {
			s, o := fixture(t, "model = \"original\"\n")
			if _, err := s.Install(context.Background()); err != nil {
				t.Fatal(err)
			}
			p, _, _, err := s.prepare(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			before := configBytes(t, o)
			later := append(append([]byte{}, before...), []byte("\n[features]\nplugins = true\n")...)
			s.beforeCommit = func() {
				path, data := p.config, later
				if what == "record" {
					path, data = p.state, []byte(`{"owner":"later-user-record"}`)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Uninstall(context.Background()); err == nil || !strings.Contains(err.Error(), "已改变") {
				t.Fatal("concurrent change was not rejected")
			}
			want := before
			if what == "config" {
				want = later
			}
			if !bytes.Equal(configBytes(t, o), want) {
				t.Fatal("concurrent data was overwritten")
			}
		})
	}
}

func TestUninstallFailureRollsBackOnlyUnchangedConfig(t *testing.T) {
	for _, laterEdit := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "preserve-later-edit"}[laterEdit], func(t *testing.T) {
			s, o := fixture(t, "model = \"original\"\n")
			if _, err := s.Install(context.Background()); err != nil {
				t.Fatal(err)
			}
			before := configBytes(t, o)
			var later []byte
			s.afterUninstallConfigCommit = func() error {
				if laterEdit {
					later = append(configBytes(t, o), []byte("\n[features]\nplugins = true\n")...)
					if err := os.WriteFile(filepath.Join(o.CodexHome, "config.toml"), later, 0600); err != nil {
						t.Fatal(err)
					}
				}
				return errors.New("fixture error must not be exposed")
			}
			if _, err := s.Uninstall(context.Background()); err == nil || strings.Contains(err.Error(), "fixture error") {
				t.Fatal("failed uninstall reported success or leaked raw failure")
			}
			want := before
			if laterEdit {
				want = later
			}
			if !bytes.Equal(configBytes(t, o), want) {
				t.Fatal("rollback overwrote later user modification")
			}
		})
	}
}

func TestUninstallRecordChangedAfterCommitIsNeverOverwritten(t *testing.T) {
	s, o := fixture(t, "model = \"original\"\n")
	if _, err := s.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	p, _, _, err := s.prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	before := configBytes(t, o)
	later := []byte(`{"owner":"later-user-record"}`)
	s.beforeUninstallArchive = func() {
		if err := os.WriteFile(p.state, later, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Uninstall(context.Background()); err == nil {
		t.Fatal("modified record was archived")
	}
	if !bytes.Equal(configBytes(t, o), before) {
		t.Fatal("failed archive did not restore unchanged config")
	}
	if got, err := os.ReadFile(p.state); err != nil || !bytes.Equal(got, later) {
		t.Fatal("modified ownership record was overwritten")
	}
}

func TestUninstallAbsentOrCanceledNeverCreatesDirectories(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "canceled"}[canceled], func(t *testing.T) {
			root := t.TempDir()
			o := Options{DataDir: filepath.Join(root, "absent-data"), CodexHome: filepath.Join(root, "absent-home")}
			s := New(o)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if canceled {
				cancel()
			}
			for _, run := range []func(context.Context) (Status, error){s.PreviewUninstall, s.Uninstall, s.Uninstall} {
				st, err := run(ctx)
				if canceled && err == nil || !canceled && err != nil || st.Installed || st.UninstallAvailable || st.RestartRequired {
					t.Fatalf("no-op uninstall: %+v %v", st, err)
				}
			}
			for _, path := range []string{o.DataDir, o.CodexHome} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("uninstalled or canceled operation created a directory")
				}
			}
		})
	}
}

func TestUninstallCancellationImmediatelyBeforeCommitPreservesConfig(t *testing.T) {
	s, o := fixture(t, "model = \"original\"\n")
	if _, err := s.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := configBytes(t, o)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.beforeCommit = cancel
	if _, err := s.Uninstall(ctx); err == nil {
		t.Fatal("late cancellation was ignored")
	}
	if !bytes.Equal(configBytes(t, o), before) {
		t.Fatal("cancelled uninstall modified config")
	}
	if st, err := s.PreviewUninstall(context.Background()); err != nil || !st.Installed || !st.UninstallAvailable {
		t.Fatal("cancelled uninstall disabled or archived active installation")
	}
}

func TestUninstallAlreadyRemovedEntryArchivesOnlyOwnedRecord(t *testing.T) {
	s, o := fixture(t, "model = \"original\"\n")
	if _, err := s.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	current := []byte("model = \"user-edited\"\n[features]\nplugins = true\n")
	if err := os.WriteFile(filepath.Join(o.CodexHome, "config.toml"), current, 0600); err != nil {
		t.Fatal(err)
	}
	if st, err := s.Uninstall(context.Background()); err != nil || st.Installed || st.RestartRequired {
		t.Fatalf("already removed entry: %+v %v", st, err)
	}
	if !bytes.Equal(configBytes(t, o), current) {
		t.Fatal("already absent MCP entry rewrote current config")
	}
	archives, err := filepath.Glob(filepath.Join(o.DataDir, "desktop-companion", "archives", "*.json"))
	if err != nil || len(archives) != 1 {
		t.Fatal("stale owned record was not archived")
	}
}
