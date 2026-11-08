package hubclient

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

// Opt-in: the caller supplies an isolated, already-installed React/Vite fixture.
// This test never installs dependencies or edits a product project. Its sole
// temporary TSX change is restored even when compilation/readiness fails.
func TestProjectPreviewInstalledViteReactTSX(t *testing.T) {
	root := os.Getenv("SALCARA_TEST_VITE_PROJECT")
	if root == "" {
		t.Skip("set SALCARA_TEST_VITE_PROJECT to the isolated installed fixture")
	}
	root = filepath.Clean(root)
	c, agent := surfaceClient(t)
	t.Cleanup(c.closeWorkspaceProjectPreviews)
	if err := c.o.Store.Update(func(cfg *config.Config) error {
		cfg.Projects = []protocol.Project{{Path: root, Name: "Isolated Vite integration fixture"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	agent.sessions = []protocol.SessionInfo{{SessionKey: workspaceTestKey, Cwd: root, ControlSurface: "cli"}}
	command := func(action string) map[string]any {
		return map[string]any{"type": "workspace.project.preview", "action": action, "sessionKey": workspaceTestKey, "controlSurface": "cli", "expectedCwd": root}
	}
	inspect := command("inspect")
	inspect["sourcePath"] = "src/App.tsx"
	result, err := c.Dispatch(context.Background(), inspect)
	if err != nil {
		t.Fatal(err)
	}
	inspection := result.(projectPreviewInspection)
	if !inspection.Ready || inspection.ProjectPath != "" || inspection.Run != nil {
		t.Fatalf("installed fixture inspection: %+v", inspection)
	}
	start := command("start")
	start["projectPath"], start["script"], start["manifestRevision"], start["operationId"] = "", "dev", inspection.ManifestRevision, projectPreviewFixtureID
	result, err = c.Dispatch(context.Background(), start)
	if err != nil {
		t.Fatal(err)
	}
	initial := result.(projectPreviewResult).Run
	if initial == nil {
		t.Fatal("missing start receipt")
	}
	status := command("status")
	status["projectPath"], status["operationId"] = "", projectPreviewFixtureID
	var ready projectPreviewState
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		result, err = c.Dispatch(context.Background(), status)
		if err != nil {
			t.Fatal(err)
		}
		ready = *result.(projectPreviewResult).Run
		if ready.Status == "running" {
			break
		}
		if ready.Status == "failed" {
			t.Fatalf("Vite startup failed: %+v", ready)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if ready.Status != "running" || ready.RunID != initial.RunID {
		t.Fatalf("Vite readiness/reconnect failed: %+v", ready)
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
	fetch := func(suffix string) string {
		t.Helper()
		response, err := client.Get(ready.URL + suffix)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil || response.StatusCode != 200 {
			t.Fatalf("Vite module: %d %v %s", response.StatusCode, err, body)
		}
		return string(body)
	}
	if html := fetch("/"); !strings.Contains(html, "/src/App.tsx") {
		t.Fatal("Vite did not serve the React entrypoint")
	}
	first := fetch("/src/App.tsx")
	if !strings.Contains(first, "Salcara real TSX version one") || !strings.Contains(strings.ToLower(first), "react") || strings.Contains(first, "<h1>") {
		t.Fatalf("TSX was not compiled to a React module: %s", first)
	}
	file := filepath.Join(root, "src", "App.tsx")
	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(original), "Salcara real TSX version one") {
		t.Fatal("fixture is not the explicitly authorized integration source")
	}
	t.Cleanup(func() {
		if err := os.WriteFile(file, original, 0600); err != nil {
			t.Errorf("restore integration source: %v", err)
		}
	})
	changed := strings.Replace(string(original), "Salcara real TSX version one", "Salcara real TSX version two", 1)
	if err := os.WriteFile(file, []byte(changed), 0600); err != nil {
		t.Fatal(err)
	}
	updated := false
	for until := time.Now().Add(5 * time.Second); time.Now().Before(until); {
		body := fetch("/src/App.tsx?t=" + strconv.FormatInt(time.Now().UnixMilli(), 10))
		if strings.Contains(body, "Salcara real TSX version two") && !strings.Contains(body, "Salcara real TSX version one") {
			updated = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !updated {
		t.Fatal("Vite did not invalidate/recompile changed TSX")
	}
	stop := command("stop")
	stop["projectPath"], stop["runId"] = "", ready.RunID
	result, err = c.Dispatch(context.Background(), stop)
	if err != nil || result.(projectPreviewResult).Run.Status != "stopped" {
		t.Fatal("Vite stop failed", err)
	}
	for until := time.Now().Add(5 * time.Second); time.Now().Before(until); {
		response, err := client.Get(ready.URL)
		if err != nil {
			t.Log("Verified real Vite: TSX compilation, updated module, operation recovery, process shutdown")
			return
		}
		response.Body.Close()
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("Vite server remained alive after stop")
}
