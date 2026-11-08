package console

import (
	"net/http"
	"strings"
	"testing"
)

func TestSessionControlTemporaryOptionsReachDispatchButNeverGrantPermissions(t *testing.T) {
	server, handler := newTestServer(t)
	server.d.Hub = nil // a distinctive dispatch error, after schema admission
	headers := map[string]string{"Cookie": CookieName + "=" + server.Token(), "Content-Type": "application/json"}
	body := `{"type":"session.side-question.ask","tool":"codex","sessionKey":"codex:thread","question":"explain","expectedRevision":"snapshot-1","operationId":"11111111-2222-4333-8444-555555555555","previousOperationId":"11111111-2222-4333-8444-666666666666","model":"model-a","effort":"high","attachments":["11111111-2222-4333-8444-777777777777"]}`
	response := do(handler, http.MethodPost, "/api/session/control", "127.0.0.1:47831", headers, body)
	if !strings.Contains(response.Body.String(), "程序还在启动") {
		t.Fatalf("temporary options rejected before dispatch: %s", response.Body.String())
	}
	for _, extra := range []string{`,"permissionMode":"full_access"`, `,"cwd":"C:/private"`, `,"images":[{"Mime":"image/png","Data":"YWJj"}]`} {
		response = do(handler, http.MethodPost, "/api/session/control", "127.0.0.1:47831", headers, strings.TrimSuffix(body, "}")+extra+"}")
		if !strings.Contains(response.Body.String(), "会话操作参数无效") {
			t.Fatalf("unexpected capability accepted: %s", response.Body.String())
		}
	}
}

func TestSessionControlRejectsUnknownCommandsAndFieldsBeforeDispatch(t *testing.T) {
	server, handler := newTestServer(t)
	server.d.Hub = nil
	for _, body := range []string{
		`{"type":"anything","sessionKey":"codex:thread"}`,
		`{"type":"session.context","sessionKey":"codex:thread","cwd":"C:/private"}`,
		`{"type":"session.compact","sessionKey":"codex:thread","operationId":"x"}`,
		`{"type":"sessions.archived","tool":"codex","offset":0}`,
	} {
		response := do(handler, http.MethodPost, "/api/session/control", "127.0.0.1:47831", map[string]string{
			"Cookie": CookieName + "=" + server.Token(), "Content-Type": "application/json",
		}, body)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status=%d body=%s", body, response.Code, response.Body.String())
		}
	}
}

func TestSessionControlRequiresSessionNamespaceAndRejectsSurfaceOverride(t *testing.T) {
	server, handler := newTestServer(t)
	server.d.Hub = nil
	hdr := map[string]string{"Cookie": CookieName + "=" + server.Token(), "Content-Type": "application/json"}
	for _, body := range []string{
		`{"type":"session.context","sessionKey":"no-prefix"}`,
		`{"type":"session.context","sessionKey":"codex:thread","tool":"claude"}`,
		`{"type":"session.context","sessionKey":"codex:thread","controlSurface":"desktop"}`,
	} {
		response := do(handler, http.MethodPost, "/api/session/control", "127.0.0.1:47831", hdr, body)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status=%d body=%s", body, response.Code, response.Body.String())
		}
	}
}

func TestSessionControlRequiresLocalConsoleAuthentication(t *testing.T) {
	_, handler := newTestServer(t)
	response := do(handler, http.MethodPost, "/api/session/control", "127.0.0.1:47831", map[string]string{"Content-Type": "application/json"}, `{"type":"session.actions","sessionKey":"codex:thread"}`)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized request status=%d", response.Code)
	}
}
