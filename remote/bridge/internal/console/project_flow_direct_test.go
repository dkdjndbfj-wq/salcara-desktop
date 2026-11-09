package console

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/directrelay"
	"salcara/bridge/internal/hubclient"
	"salcara/bridge/internal/protocol"
)

func TestProjectFlowOverAuthenticatedDirectWebSocket(t *testing.T) {
	s, _ := newTestServer(t)
	hub := hubclient.New(hubclient.Options{Store: s.d.Store})
	s.d.Hub = hub
	var started []string
	a := &directWorkspaceLifecycleAgent{sessionAgent: &sessionAgent{kind: "codex"}, emit: func(_, cwd string) { started = append(started, cwd) }}
	hub.SetManager(consoleCatalogManager{agent: a})
	phoneID, phoneHash := "project-phone", directrelay.HashPhoneSecret("isolated-project-secret")
	if err := s.d.Store.Update(func(cfg *config.Config) error {
		cfg.ActivateRemoteMode("direct")
		cfg.DirectPhoneID, cfg.DirectPhoneHash = phoneID, phoneHash
		cfg.Projects = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var server *httptest.Server
	relay := directrelay.New(directrelay.Options{DeviceID: "project-pc", PairedPhoneID: phoneID, PairedPhoneHash: phoneHash, SecureURLs: func() []string { return []string{"wss" + strings.TrimPrefix(server.URL, "https") + "/direct/ws"} }})
	server = httptest.NewTLSServer(relay.Handler())
	t.Cleanup(func() { relay.Close(); server.Close() })
	dialer := websocket.Dialer{TLSClientConfig: server.Client().Transport.(*http.Transport).TLSClientConfig}
	conn, _, err := dialer.Dial("wss"+strings.TrimPrefix(server.URL, "https")+"/direct/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.WriteJSON(map[string]string{"type": "auth", "phoneId": phoneID, "phoneHash": phoneHash})
	var auth map[string]any
	if err = conn.ReadJSON(&auth); err != nil || auth["type"] != "auth_ok" {
		t.Fatal(auth, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.runDirectCommands(ctx, relay)
	i := 0
	call := func(cmd map[string]any) map[string]any {
		t.Helper()
		i++
		id := fmt.Sprint("project-", i)
		if err := conn.WriteJSON(map[string]any{"type": "command", "requestId": id, "command": cmd}); err != nil {
			t.Fatal(err)
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var frame map[string]any
		if err := conn.ReadJSON(&frame); err != nil || frame["requestId"] != id {
			t.Fatal(frame, err)
		}
		return frame
	}
	command := func(action, cwd, rel string) map[string]any {
		return map[string]any{"type": "workspace.projects", "action": action, "tool": "codex", "workspaceMode": "work", "controlSurface": "cli", "cwd": cwd, "path": rel}
	}
	empty := call(command("list", "", ""))
	if empty["error"] != nil {
		t.Fatal(empty)
	}
	if roots := empty["result"].(map[string]any)["roots"].([]any); len(roots) != 0 {
		t.Fatal(roots)
	}
	root := t.TempDir()
	if err = s.d.Store.Update(func(cfg *config.Config) error {
		cfg.Projects = []protocol.Project{{Path: root, Name: "Allowed"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	listed := call(command("list", root, ""))
	if listed["error"] != nil {
		t.Fatal(listed)
	}
	create := command("mkdir", root, "")
	create["name"] = "chosen"
	created := call(create)
	if created["error"] != nil {
		t.Fatal(created)
	}
	cwd := created["result"].(map[string]any)["project"].(map[string]any)["path"].(string)
	if cwd != filepath.Join(root, "chosen") {
		t.Fatal(cwd)
	}
	start := call(map[string]any{"type": "session.start", "tool": "codex", "controlSurface": "cli", "workspaceMode": "work", "cwd": cwd, "prompt": "synthetic exact project"})
	if start["error"] != nil || len(started) != 1 || started[0] != cwd {
		t.Fatal(start, started)
	}
	if _, err = hub.Dispatch(context.Background(), map[string]any{"type": "session.start", "tool": "codex", "cwd": t.TempDir(), "prompt": "must reject"}); err == nil {
		t.Fatal("outside cwd admitted")
	}
	if len(started) != 1 {
		t.Fatal("rejected request changed cwd")
	}
}
