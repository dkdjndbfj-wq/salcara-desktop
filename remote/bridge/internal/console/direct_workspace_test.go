package console

import "testing"

func TestDirectWorkspaceToolsRemainExplicitlySessionScoped(t *testing.T) {
	for _, typ := range []string{"workspace.project.preview", "workspace.artifacts", "workspace.files.write", "workspace.files.mutate", "workspace.git.read", "workspace.git.mutate", "workspace.preview.file", "workspace.terminal.open", "workspace.terminal.read", "workspace.terminal.input", "workspace.terminal.resize", "workspace.terminal.close", "workspace.preview.open", "workspace.preview.frame", "workspace.preview.input", "workspace.preview.close"} {
		t.Run(typ, func(t *testing.T) {
			cmd := map[string]any{"type": typ, "sessionKey": "codex:workspace", "controlSurface": "cli"}
			if err := directCommandAllowed(cmd); err != nil {
				t.Fatalf("paired CLI workspace command rejected: %v", err)
			}
			cmd["controlSurface"] = "desktop"
			if err := directCommandAllowed(cmd); err == nil {
				t.Fatal("CLI direct channel selected desktop authority")
			}
			cmd["controlSurface"] = "cli"
			delete(cmd, "sessionKey")
			if err := directCommandAllowed(cmd); err == nil {
				t.Fatal("workspace command admitted without exact session")
			}
		})
	}
}
