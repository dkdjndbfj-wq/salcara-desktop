package console

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"salcara/bridge/internal/hubclient"
)

func TestRequiredUpdatePolicyAuthenticationAndAdmission(t *testing.T) {
	s, h := newTestServer(t)
	s.d.Version = "1.6.4"
	s.d.Hub = hubclient.New(hubclient.Options{Version: "1.6.4", Store: s.d.Store})
	s.d.Quit = func() { t.Error("policy must never quit the core") }
	host := "127.0.0.1:47831"
	headers := map[string]string{"Cookie": CookieName + "=" + s.Token(), "Content-Type": "application/json", "Origin": "http://" + host}
	body := `{"pid":` + strconv.Itoa(os.Getpid()) + `,"version":"1.6.4","required":true,"minimumSupportedVersion":"1.6.5","targetVersion":"1.7.0","reason":"协议已更新"}`
	if w := do(h, "POST", "/api/update/policy", host, nil, body); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated policy: %d", w.Code)
	}
	badOrigin := map[string]string{"Cookie": headers["Cookie"], "Content-Type": "application/json", "Origin": "https://untrusted.example"}
	if w := do(h, "POST", "/api/update/policy", host, badOrigin, body); w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin policy: %d", w.Code)
	}
	if w := do(h, "POST", "/api/update/policy", host, headers, strings.Replace(body, `"version":"1.6.4"`, `"version":"1.6.3"`, 1)); w.Code != http.StatusConflict {
		t.Fatalf("wrong core identity: %d", w.Code)
	}
	if w := do(h, "POST", "/api/update/policy", host, headers, body); w.Code != http.StatusOK {
		t.Fatalf("verified policy: %d %s", w.Code, w.Body.String())
	}
	w := do(h, "GET", "/api/update/policy", host, headers, "")
	var status struct {
		PID     int                    `json:"pid"`
		Version string                 `json:"version"`
		Policy  hubclient.UpdatePolicy `json:"policy"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || !status.Policy.Required || status.PID != os.Getpid() || status.Version != "1.6.4" {
		t.Fatalf("wrong policy status: %s", w.Body.String())
	}
	for _, route := range []string{"/api/local/switch", "/api/local/launch", "/api/local/resume"} {
		w := do(h, "POST", route, host, headers, `{}`)
		if w.Code != http.StatusUpgradeRequired || !strings.Contains(w.Body.String(), "update_required") {
			t.Fatalf("new work %s not blocked: %d %s", route, w.Code, w.Body.String())
		}
	}
	if w := do(h, "GET", "/api/approvals", host, headers, ""); w.Code != http.StatusOK {
		t.Fatalf("approval reading blocked: %d", w.Code)
	}
	badFloor := strings.Replace(body, `"minimumSupportedVersion":"1.6.5"`, `"minimumSupportedVersion":"9.0.0"`, 1)
	if w := do(h, "POST", "/api/update/policy", host, headers, badFloor); w.Code != http.StatusBadRequest || !s.d.Hub.UpdatePolicy().Required {
		t.Fatal("invalid policy altered the restriction")
	}
	clear := `{"pid":` + strconv.Itoa(os.Getpid()) + `,"version":"1.6.4","required":false}`
	if w := do(h, "POST", "/api/update/policy", host, headers, clear); w.Code != http.StatusOK || s.d.Hub.UpdatePolicy().Required {
		t.Fatal("verified policy withdrawal did not restore admission")
	}
}
