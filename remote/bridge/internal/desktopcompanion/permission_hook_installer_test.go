package desktopcompanion

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/pelletier/go-toml/v2"
)

func TestPermissionHookInstallPreservesUserHooksWithoutGrantingTrust(t *testing.T) {
	original := "# user hooks\r\nmodel = \"original\"\r\n[features]\r\nhooks = false\r\n[[hooks.PermissionRequest]]\r\nmatcher = \"Bash\"\r\n[[hooks.PermissionRequest.hooks]]\r\ntype = \"command\"\r\ncommand = \"user-policy.exe\"\r\ntimeout = 15\r\n[[hooks.Stop]]\r\n[[hooks.Stop.hooks]]\r\ntype = \"command\"\r\ncommand = \"user-stop.exe\"\r\n"
	s, o := fixture(t, original)
	if st, err := s.Install(context.Background()); err != nil || !st.Installed {
		t.Fatal(st, err)
	}
	installed := configBytes(t, o)
	if !bytes.HasPrefix(installed, []byte(original)) {
		t.Fatal("existing hook/feature bytes changed")
	}
	p, _, _, err := s.prepare(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := toml.Unmarshal(installed, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["features"].(map[string]any)["hooks"] != false || len(doc["hooks"].(map[string]any)["PermissionRequest"].([]any)) != 2 || permissionHookCount(doc, p) != 1 {
		t.Fatal("hook trust/features changed or user permission hook lost")
	}
	groups := doc["hooks"].(map[string]any)["PermissionRequest"].([]any)
	if !reflect.DeepEqual(groups[1], expectedPermissionHook(p)) {
		t.Fatal("permission relay is not the bounded synchronous hook")
	}
	if bytes.Contains(installed, []byte("trusted")) || bytes.Contains(installed, []byte("allow_managed_hooks_only")) {
		t.Fatal("installer invented hook trust/policy")
	}
	later := "\n[[hooks.PermissionRequest]]\nmatcher = \"Edit\"\n[[hooks.PermissionRequest.hooks]]\ntype = \"command\"\ncommand = \"later-policy.exe\"\n"
	if err := os.WriteFile(p.config, append(installed, []byte(later)...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := string(configBytes(t, o)); got != original+"\n"+later {
		t.Fatal("uninstall did not preserve other hooks exactly")
	}
}

func TestPermissionHookEditsAreNotSilentlyRepairedOrUninstalled(t *testing.T) {
	for _, edit := range []string{"timeout", "async", "command", "format", "missing", "duplicate", "hookHash", "mcpMissing"} {
		t.Run(edit, func(t *testing.T) {
			s, o := fixture(t, "model = \"original\"\n")
			if _, err := s.Install(context.Background()); err != nil {
				t.Fatal(err)
			}
			p, _, _, err := s.prepare(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			current := string(configBytes(t, o))
			switch edit {
			case "timeout":
				current = strings.Replace(current, "timeout = 120", "timeout = 999", 1)
			case "async":
				current = strings.Replace(current, "async = false", "async = true", 1)
			case "command":
				current = strings.Replace(current, "permission-hook.mjs", "foreign-hook.mjs", 1)
			case "format":
				current = strings.Replace(current, "async = false", "async=false", 1)
			case "missing":
				current = strings.TrimSuffix(current, hookTOMLVersion(p, Version))
			case "duplicate":
				current += hookTOMLVersion(p, Version)
			case "mcpMissing":
				current = strings.Replace(current, entryTOML(p), "", 1)
			case "hookHash":
				b, err := os.ReadFile(p.state)
				if err != nil {
					t.Fatal(err)
				}
				var own ownership
				if err := json.Unmarshal(b, &own); err != nil {
					t.Fatal(err)
				}
				own.HookHash = "changed"
				b, _ = json.Marshal(own)
				if err := os.WriteFile(p.state, b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(p.config, []byte(current), 0600); err != nil {
				t.Fatal(err)
			}
			for _, check := range []func(context.Context) (Status, error){s.Preview, s.Install, s.PreviewUninstall, s.Uninstall} {
				if _, err := check(context.Background()); err == nil {
					t.Fatal("changed hook/record was accepted")
				}
				if string(configBytes(t, o)) != current {
					t.Fatal("changed configuration was overwritten")
				}
			}
		})
	}
}

func TestLegacy03And01UninstallWithoutNewHook(t *testing.T) {
	for _, version := range []string{"0.3.0", "0.1.0"} {
		t.Run(version, func(t *testing.T) {
			s, o := fixture(t, "model = \"original\"\n")
			if _, err := s.Install(context.Background()); err != nil {
				t.Fatal(err)
			}
			p, _, _, err := s.prepare(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(p.state)
			var own ownership
			if err := json.Unmarshal(b, &own); err != nil {
				t.Fatal(err)
			}
			own.Version, own.HookHash = version, ""
			p.payload = filepath.Join(p.root, "v"+version)
			own.EntryHash = entryHash(expectedEntryVersion(p, version))
			current := "model = \"original\"\n\n# Salcara desktop companion installer: " + own.ID + "\n" + entryTOMLVersion(p, version) + "\n[features]\nhooks = false\n"
			if err := os.WriteFile(p.config, []byte(current), 0600); err != nil {
				t.Fatal(err)
			}
			b, _ = json.Marshal(own)
			if err := os.WriteFile(p.state, b, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Uninstall(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := string(configBytes(t, o)); got != "model = \"original\"\n\n\n[features]\nhooks = false\n" {
				t.Fatal("legacy uninstall damaged user settings", got)
			}
		})
	}
}

func TestSealedInlineHooksRejectedWithoutWriting(t *testing.T) {
	original := "model = \"original\"\nhooks = { PermissionRequest = [] }\n"
	s, o := fixture(t, original)
	for _, check := range []func(context.Context) (Status, error){s.Preview, s.Install} {
		if _, err := check(context.Background()); err == nil {
			t.Fatal("sealed inline hooks table accepted")
		}
	}
	if string(configBytes(t, o)) != original {
		t.Fatal("sealed hooks were modified")
	}
	if _, err := os.Stat(o.DataDir); !os.IsNotExist(err) {
		t.Fatal("rejected hooks staged installation state")
	}
}

func TestHookCommandQuotesAbsolutePathsWithSpaces(t *testing.T) {
	p := paths{node: filepath.Join(t.TempDir(), "Node Runtime $token 'quoted'", "node.exe"), payload: filepath.Join(t.TempDir(), "Plugin Payload `literal` %unchanged%")}
	command := permissionHookCommand(p)
	if !strings.HasPrefix(command, "'") || !strings.Contains(command, "' '") || !strings.Contains(command, "'\\''quoted'\\''") {
		t.Fatal("POSIX command paths were not independently quoted")
	}
	full := permissionHookCommandWindows(p)
	parts := strings.Fields(full)
	if len(parts) != 5 || parts[0] != "C:/Windows/System32/WindowsPowerShell/v1.0/powershell.exe" || parts[1] != "-NoProfile" || parts[2] != "-NonInteractive" || parts[3] != "-EncodedCommand" {
		t.Fatal("Windows wrapper exposes shell-interpreted paths")
	}
	raw, err := base64.StdEncoding.DecodeString(parts[4])
	if err != nil || len(raw)%2 != 0 {
		t.Fatal("invalid Windows script encoding")
	}
	points := make([]uint16, len(raw)/2)
	for i := range points {
		points[i] = binary.LittleEndian.Uint16(raw[i*2:])
	}
	script := string(utf16.Decode(points))
	if !strings.Contains(script, "'quoted''") || !strings.Contains(script, "ReadToEnd(); $inputJson | & '") || !strings.Contains(script, "UTF8Encoding]::new($false)") {
		t.Fatal("Windows wrapper did not preserve path literals or UTF-8 input")
	}
}

func TestWindowsPermissionHookWrapperFixturePreservesUTF8AndNoBOM(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows PowerShell wrapper")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node runtime unavailable")
	}
	root := filepath.Join(t.TempDir(), "hook fixture $literal 'single' %NOT_EXPANDED%")
	if err := os.MkdirAll(filepath.Join(root, "src"), 0700); err != nil {
		t.Fatal(err)
	}
	fixtureScript := `import process from 'node:process';let input='';for await (const chunk of process.stdin)input+=chunk;const body=JSON.parse(input);process.stdout.write(JSON.stringify({hookSpecificOutput:{hookEventName:body.hook_event_name,decision:{behavior:'deny',message:body.tool_input.description}}}));`
	if err := os.WriteFile(filepath.Join(root, "src", "permission-hook.mjs"), []byte(fixtureScript), 0600); err != nil {
		t.Fatal(err)
	}
	parts := strings.Fields(permissionHookCommandWindows(paths{node: node, payload: root}))
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"PermissionRequest","tool_input":{"description":"审批：中文 🌟 $data 'quoted'"}}`)
	output, err := cmd.Output()
	if err != nil {
		t.Fatal("isolated Windows wrapper failed", err)
	}
	want := `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"审批：中文 🌟 $data 'quoted'"}}}`
	if strings.TrimSpace(string(output)) != want || bytes.HasPrefix(output, []byte{0xef, 0xbb, 0xbf}) {
		t.Fatal("wrapper corrupted approval JSON or wrote a BOM", string(output))
	}
}
