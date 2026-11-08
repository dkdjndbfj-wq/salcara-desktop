package console

import (
	"net/http"
	"testing"

	"salcara/bridge/internal/config"
)

func TestImageSettingsAreStrictAndDisableSurvivesSaveFailure(t *testing.T) {
	s, h := newTestServer(t)
	seedCodexAPIRuntime(t, s)
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.CodexAPI.ImagesMode = "enabled"
		c.CodexAPI.MaxConcurrentImageRequests = 4
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, value := range []map[string]any{{"mode": "bogus", "maxConcurrentRequests": 2}, {"mode": "enabled", "maxConcurrentRequests": 17}, {"mode": nil, "maxConcurrentRequests": 2}, {"Mode": "disabled", "maxConcurrentRequests": 2}} {
		if w := localRequest(s, h, http.MethodPost, "/api/local/codex-api/images", value); w.Code != 400 {
			t.Fatal("invalid image settings accepted", w.Code)
		}
	}
	before := s.d.Store.Get().CodexAPI.AuthEpoch
	blockCodexAPIConfigSaves(t, s)
	w := localRequest(s, h, http.MethodPost, "/api/local/codex-api/images", map[string]any{"mode": "disabled", "maxConcurrentRequests": 1})
	current := s.d.Store.Get()
	if w.Code != 500 || current.CodexAPI.ImagesMode != "disabled" || current.CodexAPI.AuthEpoch == before || current.CodexAPI.MaxConcurrentImageRequests != 1 || !current.CodexAPIActive() {
		t.Fatal("save failure revived image permissions or stopped text", w.Code)
	}
}
