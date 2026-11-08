package hubclient

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func mutationCommand(root, action, path string) map[string]any {
	return map[string]any{"type": "workspace.files.mutate", "sessionKey": workspaceTestKey, "controlSurface": "cli", "expectedCwd": root, "action": action, "path": path}
}

func TestWorkspaceFileMutationLifecycleAndNoOverwrite(t *testing.T) {
	c, _, root := workspaceFixture(t)
	cmd := mutationCommand(root, "create", "main.ts")
	cmd["text"] = "export const value = 1;\n"
	raw, err := c.dispatchWorkspaceFileMutation(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	created := raw.(workspaceFileMutationReply)
	if created.File == nil || created.File.Text != cmd["text"] {
		t.Fatalf("create not confirmed: %#v", created)
	}
	if _, err = c.dispatchWorkspaceFileMutation(context.Background(), cmd); err == nil {
		t.Fatal("create overwrote existing file")
	}
	cmd = mutationCommand(root, "rename", "main.ts")
	cmd["destination"] = "next.ts"
	cmd["expectedRevision"] = created.File.Revision
	raw, err = c.dispatchWorkspaceFileMutation(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	renamed := raw.(workspaceFileMutationReply)
	if _, err = os.Stat(filepath.Join(root, "main.ts")); !os.IsNotExist(err) {
		t.Fatal("source remains")
	}
	if renamed.File == nil || renamed.File.Path != "next.ts" || renamed.File.Text != created.File.Text {
		t.Fatal("rename lost content")
	}
	cmd = mutationCommand(root, "delete", "next.ts")
	cmd["expectedRevision"] = created.File.Revision
	if _, err = c.dispatchWorkspaceFileMutation(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("delete or staging cleanup failed")
	}
}

func TestWorkspaceFileMutationConflictsAndUnsafeTargets(t *testing.T) {
	for _, mode := range []string{"stale-delete", "stale-rename", "existing-rename", "cross-directory", "sensitive-create", "traversal", "extra-field"} {
		t.Run(mode, func(t *testing.T) {
			c, _, root := workspaceFixture(t)
			workspaceWrite(t, root, "main.ts", []byte("current\n"))
			workspaceWrite(t, root, "other.ts", []byte("preserve\n"))
			cmd := mutationCommand(root, "rename", "main.ts")
			cmd["destination"] = "next.ts"
			cmd["expectedRevision"] = workspaceFileRevision([]byte("current\n"))
			switch mode {
			case "stale-delete":
				cmd = mutationCommand(root, "delete", "main.ts")
				cmd["expectedRevision"] = workspaceFileRevision([]byte("old\n"))
			case "stale-rename":
				cmd["expectedRevision"] = workspaceFileRevision([]byte("old\n"))
			case "existing-rename":
				cmd["destination"] = "other.ts"
			case "cross-directory":
				cmd["destination"] = "folder/next.ts"
			case "sensitive-create":
				cmd = mutationCommand(root, "create", ".env")
				cmd["text"] = "SECRET=x"
			case "traversal":
				cmd["path"] = "../main.ts"
			case "extra-field":
				cmd["force"] = true
			}
			if _, err := c.dispatchWorkspaceFileMutation(context.Background(), cmd); err == nil {
				t.Fatal("unsafe mutation accepted")
			}
			main, _ := os.ReadFile(filepath.Join(root, "main.ts"))
			other, _ := os.ReadFile(filepath.Join(root, "other.ts"))
			if string(main) != "current\n" || string(other) != "preserve\n" {
				t.Fatal("conflict altered contents")
			}
		})
	}
}

func TestWorkspaceFileMutationRechecksBeforeChangingFiles(t *testing.T) {
	for _, action := range []string{"create", "delete", "rename"} {
		for _, mode := range []string{"external-edit", "project-revoked", "session-moved"} {
			t.Run(action+"/"+mode, func(t *testing.T) {
				c, base, root := workspaceFixture(t)
				workspaceWrite(t, root, "main.ts", []byte("original\n"))
				a := &workspaceWriteRaceAgent{fakeAgent: base, metadata: protocol.SessionInfo{SessionKey: workspaceTestKey, Cwd: root, ControlSurface: "cli"}}
				a.onRecheck = func() {
					switch mode {
					case "external-edit":
						if action == "create" {
							workspaceWrite(t, root, "created.ts", []byte("computer edit\n"))
						} else {
							workspaceWrite(t, root, "main.ts", []byte("computer edit\n"))
						}
					case "project-revoked":
						if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.Projects = nil; return nil }); err != nil {
							t.Fatal(err)
						}
					case "session-moved":
						a.metadata.Cwd = filepath.Dir(root)
					}
				}
				c.SetManager(&fakeManager{list: []agents.Agent{a}})
				cmd := mutationCommand(root, action, "main.ts")
				if action == "create" {
					cmd["path"] = "created.ts"
					cmd["text"] = "phone\n"
				} else {
					cmd["expectedRevision"] = workspaceFileRevision([]byte("original\n"))
					if action == "rename" {
						cmd["destination"] = "next.ts"
					}
				}
				if _, err := c.dispatchWorkspaceFileMutation(context.Background(), cmd); err == nil {
					t.Fatal("late ownership/content change accepted")
				}
				text, _ := os.ReadFile(filepath.Join(root, "main.ts"))
				want := "original\n"
				if mode == "external-edit" && action != "create" {
					want = "computer edit\n"
				}
				if string(text) != want {
					t.Fatal("original altered")
				}
				if _, err := os.Stat(filepath.Join(root, "next.ts")); !os.IsNotExist(err) {
					t.Fatal("rename published despite conflict")
				}
			})
		}
	}
}
