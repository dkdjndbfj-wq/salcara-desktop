//go:build windows

package agents

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

func claudeWorkerPermissionToken(t *testing.T, p *claudeProc) string {
	t.Helper()
	data, err := os.ReadFile(p.mcpFile)
	if err != nil {
		t.Fatal("cannot read owned synthetic helper configuration", err)
	}
	var config struct {
		Servers map[string]struct {
			Args []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal("cannot decode owned synthetic helper configuration", err)
	}
	args := config.Servers["salcara"].Args
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--token" {
			return args[i+1]
		}
	}
	t.Fatal("owned synthetic helper capability missing")
	return ""
}

func claudePermissionHTTP(a *claudeAgent, token, id, tool, input string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(PermissionRequest{Session: id, ToolName: tool, Input: json.RawMessage(input)})
	r := httptest.NewRequest(http.MethodPost, PermissionPath, strings.NewReader(string(body)))
	r.Header.Set("X-Salcara-Token", token)
	w := httptest.NewRecorder()
	a.permissionHandler(w, r)
	return w
}

func TestClaudePermissionRejectsPreviousWorkerCapability(t *testing.T) {
	a, dir := newClaudeLifecycleAgent(t, "normal")
	t.Cleanup(a.Close)
	rec := &recorder{}
	a.sink = rec.sink
	id := newUUID()
	old, err := a.spawn(context.Background(), id, dir, "", "auto_all", false)
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.procs[id] = old
	a.policies[id] = "auto_all"
	a.mu.Unlock()
	oldToken := claudeWorkerPermissionToken(t, old)
	a.stop(old)
	select {
	case <-old.done:
	case <-time.After(4 * time.Second):
		t.Fatal("old synthetic worker did not settle")
	}
	current, err := a.spawn(context.Background(), id, dir, "", "auto_all", true)
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.procs[id] = current
	a.mu.Unlock()
	currentToken := claudeWorkerPermissionToken(t, current)
	if got := claudePermissionHTTP(a, oldToken, id, "Bash", `{}`).Code; got != http.StatusForbidden {
		t.Fatal("previous worker capability was accepted by its replacement", got)
	}
	if got := claudePermissionHTTP(a, a.settings().LocalToken, id, "Bash", `{}`).Code; got != http.StatusForbidden {
		t.Fatal("global console capability was accepted as an active worker permission", got)
	}
	// A valid worker capability authenticates this particular native prompt;
	// it does not auto-authorize the operation, even under legacy auto_all.
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() { response <- claudePermissionHTTP(a, currentToken, id, "Bash", `{}`) }()
	pending := rec.wait(t, "current worker prompt", func(e protocol.Event) bool { return e.Type == "approval.request" })
	if !a.Respond(pending.ApprovalID, "allow", "") {
		t.Fatal("current worker prompt could not be answered")
	}
	select {
	case got := <-response:
		var decision PermissionDecision
		if got.Code != http.StatusOK || json.Unmarshal(got.Body.Bytes(), &decision) != nil || decision.Behavior != "allow" {
			t.Fatal("current worker capability was rejected")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("current worker prompt did not finish after its answer")
	}
	if got := claudePermissionHTTP(a, currentToken, newUUID(), "Bash", `{}`).Code; got != http.StatusForbidden {
		t.Fatal("worker capability crossed sessions", got)
	}
}
