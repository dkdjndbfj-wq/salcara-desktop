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

func TestProjectlessOverAuthenticatedDirectWebSocket(t *testing.T) {
	s, _ := newTestServer(t)
	hub := hubclient.New(hubclient.Options{Store: s.d.Store})
	s.d.Hub = hub
	a := &directWorkspaceLifecycleAgent{sessionAgent: &sessionAgent{kind: "codex"}}
	a.emit = func(key, cwd string) {
		a.info = protocol.SessionInfo{SessionKey: key, Tool: "codex", Cwd: cwd, ControlSurface: "cli", Status: "idle"}
	}
	hub.SetManager(consoleCatalogManager{agent: a})
	phoneID, phoneHash := "projectless-phone", directrelay.HashPhoneSecret("isolated-projectless-secret")
	if err := s.d.Store.Update(func(cfg *config.Config) error {
		cfg.ActivateRemoteMode("direct")
		cfg.DirectPhoneID = phoneID
		cfg.DirectPhoneHash = phoneHash
		cfg.Projects = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var server *httptest.Server
	relay := directrelay.New(directrelay.Options{DeviceID: "projectless-pc", PairedPhoneID: phoneID, PairedPhoneHash: phoneHash, SecureURLs: func() []string { return []string{"wss" + strings.TrimPrefix(server.URL, "https") + "/direct/ws"} }})
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
		id := fmt.Sprintf("projectless-%d", i)
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
	cmd := map[string]any{"type": "session.start", "tool": "codex", "controlSurface": "cli", "cwd": "", "projectless": true, "workspaceMode": "work", "prompt": "synthetic independent conversation", "operationId": config.NewUUID()}
	started := call(cmd)
	if started["error"] != nil {
		t.Fatal(started)
	}
	receipt := started["result"].(map[string]any)
	if len(receipt) != 1 || receipt["sessionKey"] != a.info.SessionKey || a.starts != 1 {
		t.Fatal("start receipt changed", receipt, a.starts)
	}
	if retry := call(cmd); retry["error"] != nil || retry["result"].(map[string]any)["sessionKey"] != receipt["sessionKey"] || a.starts != 1 {
		t.Fatal("transport replay duplicated", retry)
	}
	list := call(map[string]any{"type": "sessions.list", "tool": "codex", "controlSurface": "cli"})
	if list["error"] != nil {
		t.Fatal(list)
	}
	row := list["result"].(map[string]any)["sessions"].([]any)[0].(map[string]any)
	if row["projectless"] != true || row["workspaceMode"] != "work" || row["cwd"] != a.info.Cwd {
		t.Fatal("metadata missing", row)
	}
	if len(s.d.Store.Get().Projects) != 0 {
		t.Fatal("implicit global project grant")
	}
	if err = os.WriteFile(filepath.Join(a.info.Cwd, "note.md"), []byte("saved independent work"), 0600); err != nil {
		t.Fatal(err)
	}
	read := call(map[string]any{"type": "workspace.files.read", "sessionKey": a.info.SessionKey, "controlSurface": "cli", "expectedCwd": a.info.Cwd, "path": "note.md"})
	if read["error"] != nil || read["result"].(map[string]any)["text"] != "saved independent work" {
		t.Fatal("owned read failed", read)
	}
	delete(cmd, "projectless")
	cmd["operationId"] = config.NewUUID()
	if reply := call(cmd); reply["error"] == nil || a.starts != 1 {
		t.Fatal("ordinary empty cwd accepted", reply)
	}
	if hub.Status().Queued != 0 {
		t.Fatal("direct operation leaked to Hub")
	}
}
