package toolcfg

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const historyInstallUUID = "01234567-89ab-4cde-8fab-0123456789ab"
const historyOrgUUID = "23456789-abcd-4ef0-8abc-0123456789ab"

func fixtureHistoryIdentity(t *testing.T, short bool, explicitOrg bool) (string, string) {
	t.Helper()
	root := t.TempDir()
	existing3P(t, root)
	file3P(t, root, "ant-did", base64.StdEncoding.EncodeToString([]byte(historyInstallUUID)))
	profile := map[string]any{"inferenceProvider": "gateway", "inferenceGatewayBaseUrl": "https://fixture.test", "inferenceGatewayApiKey": "fixture-private-key"}
	org := claudeHistoryPlaceholderOrg
	if explicitOrg {
		org = historyOrgUUID
		profile["deploymentOrganizationUuid"] = org
	}
	data, _ := json.Marshal(profile)
	file3P(t, root, "configLibrary/"+test3PID+".json", string(data))
	account := historyInstallUUID
	if short {
		account = account[:8]
		org = org[:8]
	}
	path := filepath.Join(root, "local-agent-mode-sessions", account, org)
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	return root, path
}

func TestResolveClaudeDesktopHistoryRootUsesNativeIdentityNotTelemetry(t *testing.T) {
	for _, short := range []bool{false, true} {
		for _, explicitOrg := range []bool{false, true} {
			root, want := fixtureHistoryIdentity(t, short, explicitOrg)
			got, err := ResolveClaudeDesktopHistoryRoot(root)
			if err != nil || got != want {
				t.Fatalf("native scope: short=%v explicitOrg=%v got=%q want=%q err=%v", short, explicitOrg, got, want, err)
			}
			// The inference gateway's derived telemetry ID must not become the
			// storage organization, nor may resolver expose or change its Key.
			before, _ := os.ReadFile(filepath.Join(root, "configLibrary", test3PID+".json"))
			_, err = ResolveClaudeDesktopHistoryRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(filepath.Join(root, "configLibrary", test3PID+".json"))
			if string(before) != string(after) {
				t.Fatal("identity read wrote configuration")
			}
		}
	}
}

func TestResolveClaudeDesktopHistoryRootFailsClosed(t *testing.T) {
	for _, mode := range []string{"1p", "bad install ID", "missing install ID", "bad org", "null org", "managed bootstrap", "hybrid pointer", "full and short account", "short account collision", "short org collision", "mixed layout", "missing current account"} {
		t.Run(mode, func(t *testing.T) {
			short := strings.HasPrefix(mode, "short")
			root, path := fixtureHistoryIdentity(t, short, true)
			switch mode {
			case "1p":
				file3P(t, root, "claude_desktop_config.json", `{"deploymentMode":"1p"}`)
			case "bad install ID":
				file3P(t, root, "ant-did", base64.StdEncoding.EncodeToString([]byte("../../other")))
			case "missing install ID":
				if err := os.Remove(filepath.Join(root, "ant-did")); err != nil {
					t.Fatal(err)
				}
			case "bad org":
				file3P(t, root, "configLibrary/"+test3PID+".json", `{"deploymentOrganizationUuid":"../../other"}`)
			case "null org":
				file3P(t, root, "configLibrary/"+test3PID+".json", `{"deploymentOrganizationUuid":null}`)
			case "managed bootstrap":
				file3P(t, root, "configLibrary/"+test3PID+".json", `{"bootstrapUrl":"https://fixture.test/v1/desktop/orgs/`+historyOrgUUID+`/bootstrap"}`)
			case "hybrid pointer":
				file3P(t, root, "configLibrary/_meta.json", `{"hybridPointer":true}`)
			case "full and short account":
				if err := os.MkdirAll(filepath.Join(root, "local-agent-mode-sessions", historyInstallUUID[:8], historyOrgUUID[:8]), 0700); err != nil {
					t.Fatal(err)
				}
			case "short account collision":
				if err := os.MkdirAll(filepath.Join(root, "local-agent-mode-sessions", "01234567-ffff-4cde-8fab-0123456789ab"), 0700); err != nil {
					t.Fatal(err)
				}
			case "short org collision":
				if err := os.MkdirAll(filepath.Join(filepath.Dir(path), "23456789-ffff-4ef0-8abc-0123456789ab"), 0700); err != nil {
					t.Fatal(err)
				}
			case "mixed layout":
				if err := os.Rename(path, filepath.Join(filepath.Dir(path), historyOrgUUID[:8])); err != nil {
					t.Fatal(err)
				}
			case "missing current account":
				if err := os.Rename(filepath.Dir(path), filepath.Join(root, "local-agent-mode-sessions", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := ResolveClaudeDesktopHistoryRoot(root); err == nil || got != "" {
				t.Fatalf("unverified identity accepted: %q %v", got, err)
			}
		})
	}
}

func TestResolveClaudeDesktopHistoryRootRejectsLinkedCurrentNamespace(t *testing.T) {
	root, path := fixtureHistoryIdentity(t, false, true)
	out := t.TempDir()
	// Replace only a freshly-created synthetic account/org directory. The
	// resolver must reject the reparse point before inspecting any records.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", `New-Item -ItemType Junction -Path $env:SALCARA_HISTORY_IDENTITY_TEST_LINK -Target $env:SALCARA_HISTORY_IDENTITY_TEST_TARGET -ErrorAction Stop | Out-Null`)
		cmd.Env = append(os.Environ(), "SALCARA_HISTORY_IDENTITY_TEST_LINK="+path, "SALCARA_HISTORY_IDENTITY_TEST_TARGET="+out)
		if data, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture junction: %v %s", err, data)
		}
	} else if err := os.Symlink(out, path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Error(err)
		}
	})
	if got, err := ResolveClaudeDesktopHistoryRoot(root); err == nil || got != "" {
		t.Fatalf("linked namespace enabled: %q %v", got, err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatal("target altered", err)
	}
}
