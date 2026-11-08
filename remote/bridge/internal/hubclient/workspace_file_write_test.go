package hubclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func workspaceSaveCommand(root, rel, text, revision string) map[string]any {
	cmd := workspaceCommand("write", rel, root)
	cmd["text"], cmd["expectedRevision"] = text, revision
	return cmd
}

type workspaceWriteRaceAgent struct {
	*fakeAgent
	metadata  protocol.SessionInfo
	onRecheck func()
	calls     int
}

func (a *workspaceWriteRaceAgent) SessionMetadata(context.Context, string) (protocol.SessionInfo, error) {
	a.calls++
	if a.calls == 2 {
		a.onRecheck()
	}
	return a.metadata, nil
}

func TestWorkspaceFileWriteRechecksAfterStaging(t *testing.T) {
	for _, mode := range []string{"external-edit", "project-revoked", "session-moved"} {
		t.Run(mode, func(t *testing.T) {
			c, base, root := workspaceFixture(t)
			const original = "original\n"
			workspaceWrite(t, root, "safe.txt", []byte(original))
			a := &workspaceWriteRaceAgent{fakeAgent: base, metadata: protocol.SessionInfo{SessionKey: workspaceTestKey, Cwd: root, ControlSurface: "cli"}}
			a.onRecheck = func() {
				switch mode {
				case "external-edit":
					workspaceWrite(t, root, "safe.txt", []byte("computer edit\n"))
				case "project-revoked":
					if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.Projects = nil; return nil }); err != nil {
						t.Fatal(err)
					}
				case "session-moved":
					a.metadata.Cwd = filepath.Dir(root)
				}
			}
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			if _, err := c.dispatchWorkspaceFileWrite(context.Background(), workspaceSaveCommand(root, "safe.txt", "phone edit\n", workspaceFileRevision([]byte(original)))); err == nil {
				t.Fatal("late change allowed write")
			}
			data, _ := os.ReadFile(filepath.Join(root, "safe.txt"))
			want := original
			if mode == "external-edit" {
				want = "computer edit\n"
			}
			if string(data) != want {
				t.Fatal("late change was overwritten")
			}
			entries, _ := os.ReadDir(root)
			if len(entries) != 1 {
				t.Fatal("staged file leaked after abort")
			}
		})
	}
}

func TestWorkspaceFileWriteRootLevelAndEmptyText(t *testing.T) {
	c, _, root := workspaceFixture(t)
	workspaceWrite(t, root, "main.ts", []byte("old\n"))
	result, err := c.dispatchWorkspaceFileWrite(context.Background(), workspaceSaveCommand(root, "main.ts", "", workspaceFileRevision([]byte("old\n"))))
	if err != nil {
		t.Fatal(err)
	}
	if file := result.(workspaceFileText); file.Text != "" || file.Size != 0 || file.Revision != workspaceFileRevision(nil) {
		t.Fatalf("bad empty save: %#v", file)
	}
	data, err := os.ReadFile(filepath.Join(root, "main.ts"))
	if err != nil || len(data) != 0 {
		t.Fatal("empty save not on disk")
	}
}

func TestWorkspaceFileWritePersistsAndRejectsConflictingSave(t *testing.T) {
	c, _, root := workspaceFixture(t)
	workspaceWrite(t, root, "src/example.ts", []byte("export const value = 1;\r\n"))
	read, err := c.dispatchWorkspaceFiles(context.Background(), workspaceCommand("read", "src/example.ts", root))
	if err != nil {
		t.Fatal(err)
	}
	before := read.(workspaceFileText)
	const text = "export const value = 2;\r\n// 中文\r\n"
	saved, err := c.dispatchWorkspaceFileWrite(context.Background(), workspaceSaveCommand(root, before.Path, text, before.Revision))
	if err != nil {
		t.Fatal(err)
	}
	after := saved.(workspaceFileText)
	if after.Text != text || after.Revision != workspaceFileRevision([]byte(text)) || after.Size != int64(len(text)) {
		t.Fatalf("invalid saved response: %#v", after)
	}
	onDisk, err := os.ReadFile(filepath.Join(root, "src", "example.ts"))
	if err != nil || string(onDisk) != text {
		t.Fatal("save was not persisted")
	}
	_, err = c.dispatchWorkspaceFileWrite(context.Background(), workspaceSaveCommand(root, before.Path, "stale overwrite", before.Revision))
	if !errors.Is(err, errWorkspaceFileConflict) {
		t.Fatalf("stale save accepted: %v", err)
	}
	onDisk, _ = os.ReadFile(filepath.Join(root, "src", "example.ts"))
	if string(onDisk) != text {
		t.Fatal("conflict overwrote the file")
	}
	entries, _ := os.ReadDir(filepath.Join(root, "src"))
	if len(entries) != 1 {
		t.Fatal("staged file leaked")
	}
}

func TestWorkspaceFileWriteRejectsUnsafeInputsAndRevokedProject(t *testing.T) {
	for _, condition := range []string{"traversal", "new-file", "binary", "oversize", "revision", "cwd", "revoked"} {
		t.Run(condition, func(t *testing.T) {
			c, _, root := workspaceFixture(t)
			const original = "original\n"
			workspaceWrite(t, root, "safe.txt", []byte(original))
			cmd := workspaceSaveCommand(root, "safe.txt", "new", workspaceFileRevision([]byte(original)))
			switch condition {
			case "traversal":
				cmd["path"] = "../safe.txt"
			case "new-file":
				cmd["path"] = "new.txt"
			case "binary":
				cmd["text"] = "bad\x00text"
			case "oversize":
				cmd["text"] = strings.Repeat("x", workspaceFileBytes+1)
			case "revision":
				cmd["expectedRevision"] = ""
			case "cwd":
				cmd["expectedCwd"] = filepath.Dir(root)
			case "revoked":
				if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.Projects = nil; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := c.dispatchWorkspaceFileWrite(context.Background(), cmd); err == nil {
				t.Fatal("invalid write accepted")
			}
			data, _ := os.ReadFile(filepath.Join(root, "safe.txt"))
			if string(data) != original {
				t.Fatal("rejected write mutated original")
			}
			entries, _ := os.ReadDir(root)
			if len(entries) != 1 {
				t.Fatal("unexpected file created")
			}
		})
	}
}
