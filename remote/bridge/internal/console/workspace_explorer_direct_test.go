package console

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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

func TestExplorerOverAuthenticatedDirectWebSocket(t *testing.T) {
	s, _ := newTestServer(t)
	hub := hubclient.New(hubclient.Options{Store: s.d.Store})
	s.d.Hub = hub
	a := &directWorkspaceLifecycleAgent{sessionAgent: &sessionAgent{kind: "codex"}}
	a.emit = func(key, cwd string) {
		a.info = protocol.SessionInfo{SessionKey: key, Tool: "codex", Cwd: cwd, ControlSurface: "cli", Status: "idle"}
	}
	hub.SetManager(consoleCatalogManager{agent: a})
	phoneID, phoneHash := "explorer-phone", directrelay.HashPhoneSecret("isolated-explorer-secret")
	if err := s.d.Store.Update(func(cfg *config.Config) error {
		cfg.ActivateRemoteMode("direct")
		cfg.DirectPhoneID, cfg.DirectPhoneHash, cfg.Projects = phoneID, phoneHash, nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	if err := os.Mkdir(filepath.Join(parent, ".hidden"), 0700); err != nil {
		t.Fatal(err)
	}
	root := filepath.VolumeName(parent) + string(filepath.Separator)
	rel, _ := filepath.Rel(root, parent)
	rel = filepath.ToSlash(rel)
	command := func(action string) map[string]any {
		return map[string]any{"type": "workspace.explorer", "controlSurface": "cli", "tool": "codex", "workspaceMode": "work", "action": action, "cwd": root, "path": rel}
	}
	var server *httptest.Server
	relay := directrelay.New(directrelay.Options{DeviceID: "explorer-pc", PairedPhoneID: phoneID, PairedPhoneHash: phoneHash, SecureURLs: func() []string { return []string{"wss" + strings.TrimPrefix(server.URL, "https") + "/direct/ws"} }})
	server = httptest.NewTLSServer(relay.Handler())
	t.Cleanup(func() { relay.Close(); server.Close() })
	dialer := websocket.Dialer{TLSClientConfig: server.Client().Transport.(*http.Transport).TLSClientConfig}
	conn, _, err := dialer.Dial("wss"+strings.TrimPrefix(server.URL, "https")+"/direct/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err = conn.WriteJSON(map[string]string{"type": "auth", "phoneId": phoneID, "phoneHash": phoneHash}); err != nil {
		t.Fatal(err)
	}
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
		id := fmt.Sprintf("explorer-%d", i)
		if err := conn.WriteJSON(map[string]any{"type": "command", "requestId": id, "command": cmd}); err != nil {
			t.Fatal(err)
		}
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		var reply map[string]any
		if err := conn.ReadJSON(&reply); err != nil || reply["requestId"] != id {
			t.Fatal(reply, err)
		}
		return reply
	}
	roots := command("list")
	roots["cwd"], roots["path"] = "", ""
	if reply := call(roots); reply["error"] != nil || len(reply["result"].(map[string]any)["roots"].([]any)) == 0 {
		t.Fatal(reply)
	}
	listed := call(command("list"))
	if listed["error"] != nil {
		t.Fatal(listed)
	}
	entries := listed["result"].(map[string]any)["directory"].(map[string]any)["entries"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["name"] != ".hidden" || len(s.d.Store.Get().Projects) != 0 {
		t.Fatal("list lost hidden names or granted the parent", listed)
	}
	mkdir := command("mkdir")
	mkdir["name"] = ".created-work"
	created := call(mkdir)
	if created["error"] != nil {
		t.Fatal(created)
	}
	project := created["result"].(map[string]any)["project"].(map[string]any)["path"].(string)
	if project != filepath.Join(parent, ".created-work") || a.starts != 0 {
		t.Fatal("wrong created project or implicit task", created)
	}
	if reply := call(mkdir); reply["error"] == nil {
		t.Fatal("mkdir overwrote/adopted existing directory", reply)
	}
	started := call(map[string]any{"type": "session.start", "tool": "codex", "controlSurface": "cli", "cwd": project, "workspaceMode": "work", "prompt": "synthetic explorer task", "operationId": config.NewUUID()})
	if started["error"] != nil || a.starts != 1 || a.info.Cwd != project {
		t.Fatal("created grant unusable", started)
	}
	if err = os.WriteFile(filepath.Join(project, "note.md"), []byte("exact project"), 0600); err != nil {
		t.Fatal(err)
	}
	read := call(map[string]any{"type": "workspace.files.read", "sessionKey": a.info.SessionKey, "controlSurface": "cli", "expectedCwd": project, "path": "note.md"})
	if read["error"] != nil || read["result"].(map[string]any)["text"] != "exact project" {
		t.Fatal(read)
	}
	if err = s.d.Store.Update(func(cfg *config.Config) error { cfg.DirectPhoneID, cfg.DirectPhoneHash = "", ""; return nil }); err != nil {
		t.Fatal(err)
	}
	if reply := call(command("list")); reply["error"] == nil {
		t.Fatal("revoked pairing could browse", reply)
	}
	if err = relay.Revoke(); err != nil {
		t.Fatal(err)
	}
	var revoked map[string]any
	if err = conn.ReadJSON(&revoked); err != nil || revoked["type"] != "revoked" {
		t.Fatal(revoked, err)
	}
	if hub.Status().Queued != 0 {
		t.Fatal("Direct explorer leaked to Hub")
	}
}
