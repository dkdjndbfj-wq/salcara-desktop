package hubclient

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
)

func projectFileCommand(typ, path, cwd string) map[string]any {
	return map[string]any{"type": "workspace.project.files." + typ, "tool": "codex", "cwd": cwd, "expectedCwd": cwd, "controlSurface": "cli", "path": path}
}

func TestWorkspaceProjectFilesReadConfiguredProjectWithoutAnySession(t *testing.T) {
	c, base, root := workspaceFixture(t)
	workspaceWrite(t, root, "src/main.ts", []byte("exact source\nline two"))
	base.sessions = nil
	a := &workspaceMetadataAgent{fakeAgent: base}
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	result, err := c.Dispatch(context.Background(), projectFileCommand("read", "src/main.ts", root))
	if err != nil || result.(workspaceFileText).Text != "exact source\nline two" {
		t.Fatal("project-only read failed", err)
	}
	page, err := c.Dispatch(context.Background(), projectFileCommand("list", "src", root))
	if err != nil || len(page.(workspaceFilesPage).Entries) != 1 {
		t.Fatal("project-only listing failed", err)
	}
	if len(a.ids)+a.directoryCalls+len(base.opened)+len(base.started)+len(base.sent) > 0 {
		t.Fatal("project read invented/adopted a native session")
	}
	d := c.Device(context.Background())
	raw, _ := json.Marshal(d)
	if !strings.Contains(string(raw), "workspace.project.files.v1") {
		t.Fatal("independent capability missing")
	}
}

func TestWorkspaceProjectFilesRejectsUnauthorisedAndAmbiguousRequests(t *testing.T) {
	for _, variant := range []string{"unknown-project", "subdirectory", "expected-cwd", "surface", "session", "unknown-tool", "unknown-field", "traversal", "absolute", "secret", "binary", "oversize", "cursor-read"} {
		t.Run(variant, func(t *testing.T) {
			c, a, root := workspaceFixture(t)
			workspaceWrite(t, root, "main.ts", []byte("safe"))
			cmd := projectFileCommand("read", "main.ts", root)
			switch variant {
			case "unknown-project":
				cmd["cwd"] = t.TempDir()
				cmd["expectedCwd"] = cmd["cwd"]
			case "subdirectory":
				sub := filepath.Join(root, "src")
				os.Mkdir(sub, 0700)
				cmd["cwd"] = sub
				cmd["expectedCwd"] = sub
			case "expected-cwd":
				cmd["expectedCwd"] = t.TempDir()
			case "surface":
				cmd["controlSurface"] = "desktop"
			case "session":
				cmd["sessionKey"] = workspaceTestKey
			case "unknown-tool":
				cmd["tool"] = "not-installed"
			case "unknown-field":
				cmd["execute"] = true
			case "traversal":
				cmd["path"] = "../main.ts"
			case "absolute":
				cmd["path"] = filepath.Join(root, "main.ts")
			case "secret":
				workspaceWrite(t, root, ".env", []byte("synthetic"))
				cmd["path"] = ".env"
			case "binary":
				workspaceWrite(t, root, "main.ts", []byte{0, 1, 2})
			case "oversize":
				workspaceWrite(t, root, "main.ts", []byte(strings.Repeat("x", workspaceFileBytes+1)))
			case "cursor-read":
				cmd["cursor"] = "anything"
			}
			if got, err := c.Dispatch(context.Background(), cmd); err == nil || got != nil {
				t.Fatal("invalid project request disclosed data", variant)
			}
			if len(a.opened)+len(a.started)+len(a.sent) > 0 {
				t.Fatal("invalid read had execution side effects")
			}
		})
	}
}

func TestWorkspaceProjectFilesRechecksPairingAndCancellation(t *testing.T) {
	c, _, root := workspaceFixture(t)
	workspaceWrite(t, root, "main.ts", []byte("private fixture"))
	if err := c.o.Store.Update(func(cfg *config.Config) error {
		cfg.PhoneBindingID = strings.Repeat("a", 64)
		cfg.PhoneHash = strings.Repeat("b", 64)
		cfg.RemoteDeviceOnly = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg := c.o.Store.Get()
	c.uploadAuthorization = eventIdentity(cfg)
	ctx := context.WithValue(context.Background(), commandGuardKey{}, commandGuard{deviceID: cfg.DeviceID, identity: config.RemoteIdentity(cfg), bindingID: cfg.PhoneBindingID, phoneHash: cfg.PhoneHash})
	if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.PhoneHash = "revoked"; return nil }); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Dispatch(ctx, projectFileCommand("read", "main.ts", root)); err == nil || got != nil {
		t.Fatal("revoked pairing disclosed project")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := c.Dispatch(cancelled, projectFileCommand("read", "main.ts", root)); err == nil || got != nil {
		t.Fatal("cancelled project read returned text")
	}
}
