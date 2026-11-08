package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
)

type fixtureOfficialError struct {
	status int
	code   string
}

func (e fixtureOfficialError) Error() string         { return "private fixture body " + upstreamTestKey }
func (e fixtureOfficialError) StatusCode() int       { return e.status }
func (e fixtureOfficialError) ExecutionCode() string { return e.code }

func TestOfficialFailuresPreserveStatusAndSafeCapacityCondition(t *testing.T) {
	for _, scenario := range []fixtureOfficialError{{503, "model_capacity"}, {429, ""}, {401, ""}, {503, "unknown-private-condition"}, {200, "model_capacity"}} {
		t.Run(fmt.Sprintf("%d/%s", scenario.status, scenario.code), func(t *testing.T) {
			h := fixtureGateway(t, "codex", "responses", "", "application/json", nil)
			account, _ := h.Source("codex")
			account.SourceType, account.CredentialRef = "official_account", "synthetic-credential"
			h.Source = func(string) (config.LocalAccount, bool) { return account, true }
			h.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
				return nil, fmt.Errorf("wrapped private fixture: %w", scenario)
			}
			w := callGateway(h, "codex", "responses", `{"model":"chosen-model","input":"fixture"}`)
			wantStatus := scenario.status
			if scenario.status < 400 {
				wantStatus = 502
			}
			wantCapacity := scenario.code == "model_capacity" && scenario.status >= 400
			if w.Code != wantStatus || strings.Contains(w.Body.String(), "model_capacity") != wantCapacity || strings.Contains(w.Body.String(), "private") || strings.Contains(w.Body.String(), upstreamTestKey) || strings.Contains(w.Body.String(), "unknown-private-condition") {
				t.Fatalf("unsafe or incorrect official failure: %d %s", w.Code, w.Body)
			}
		})
	}
}
