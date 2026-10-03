package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
)

type desktopModelPage struct {
	Data []struct {
		ID    string `json:"id"`
		Label string `json:"display_name"`
	} `json:"data"`
	More  bool   `json:"has_more"`
	First string `json:"first_id"`
	Last  string `json:"last_id"`
}

func getDesktopModels(h *Handler, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = "127.0.0.1:10000"
	r.Header.Set("Authorization", "Bearer "+localTestKey)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decodeDesktopPage(t *testing.T, w *httptest.ResponseRecorder) desktopModelPage {
	t.Helper()
	var page desktopModelPage
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &page) != nil {
		t.Fatalf("model page failed: %d %s", w.Code, w.Body.String())
	}
	return page
}

func TestClaudeDesktopNativeDiscoveryQueryAndScopedPagination(t *testing.T) {
	h := fixtureGateway(t, "claude", "chat", `{}`, "application/json", func(*http.Request, map[string]any) {
		t.Error("model pagination issued a generation request")
	})
	models := []string{"deepseek-chat", "grok-fixture", "claude-sonnet-4-6"}
	desktopCatalog(h, models...)
	all := decodeDesktopPage(t, getDesktopModels(h, "/gateway/claude-desktop/v1/models?limit=1000"))
	if len(all.Data) != 3 || all.More || all.First != all.Data[0].ID || all.Last != all.Data[2].ID {
		t.Fatal("native initial discovery does not include the full catalog", all)
	}
	for i, expected := range models {
		query := "limit=1"
		if i > 0 {
			query += "&after_id=" + url.QueryEscape(config.ClaudeDesktopModelRoute(models[i-1]))
		}
		page := decodeDesktopPage(t, getDesktopModels(h, "/gateway/claude-desktop/v1/models?"+query))
		if len(page.Data) != 1 || page.Data[0].ID != config.ClaudeDesktopModelRoute(expected) || page.Data[0].Label != expected || page.More != (i < len(models)-1) || page.First != page.Last {
			t.Fatal("incorrect native model page", i, page)
		}
	}
	empty := decodeDesktopPage(t, getDesktopModels(h, "/gateway/claude-desktop/v1/models?after_id="+url.QueryEscape(all.Last)))
	if len(empty.Data) != 0 || empty.More || empty.First != "" || empty.Last != "" {
		t.Fatal("cursor at final model should return an empty final page", empty)
	}
}

func TestClaudeDesktopPaginationCursorCanUseOnDemandCatalog(t *testing.T) {
	h := fixtureGateway(t, "claude", "chat", `{}`, "application/json", nil)
	desktopCatalog(h)
	calls := 0
	h.RefreshModels = func(context.Context, string, config.LocalAccount) ([]string, error) {
		calls++
		return []string{"deepseek-chat", "grok-fixture"}, nil
	}
	page := decodeDesktopPage(t, getDesktopModels(h, "/gateway/claude-desktop/v1/models?limit=1&after_id="+url.QueryEscape(config.ClaudeDesktopModelRoute("deepseek-chat"))))
	if calls != 1 || len(page.Data) != 1 || page.Data[0].Label != "grok-fixture" || page.More {
		t.Fatal("cursor was not resolved against the freshly loaded catalog", calls, page)
	}
}

func TestClaudeDesktopModelsRejectsInvalidQueryFieldsAndForeignCursor(t *testing.T) {
	for _, query := range []string{
		"limit=0", "limit=1001", "limit=-1", "limit=%2B1", "limit=", "limit=x", "limit=1.0",
		"limit=1&limit=2", "%6cimit=1&limit=2", "limit=%GG", "limit=1;after_id=x",
		"after_id=", "after_id=foreign", "after_id=deepseek-chat", "after_id=%0A",
		"after_id=one&after_id=two", "before_id=one", "cursor=one", "unknown=one",
	} {
		t.Run(query, func(t *testing.T) {
			h := fixtureGateway(t, "claude", "chat", `{}`, "application/json", func(*http.Request, map[string]any) {
				t.Error("invalid discovery query sent a generation request")
			})
			desktopCatalog(h, "deepseek-chat")
			w := getDesktopModels(h, "/gateway/claude-desktop/v1/models?"+query)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("accepted invalid model query: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestClaudeDesktopOnlyModelsGETAllowsQuery(t *testing.T) {
	h := fixtureGateway(t, "claude", "chat", `{}`, "application/json", nil)
	desktopCatalog(h, "deepseek-chat")
	for _, path := range []string{
		"/gateway/codex/v1/models?limit=1000",
		"/gateway/claude/v1/models?limit=1000",
		"/gateway/remote-claude/v1/models?limit=1000",
		"/gateway/claude-desktop/v1/messages?limit=1000",
	} {
		if w := getDesktopModels(h, path); w.Code != http.StatusNotFound {
			t.Fatal("query parameters escaped the desktop models GET boundary", path, w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/gateway/claude-desktop/v1/models?limit=1000", strings.NewReader(`{}`))
	r.RemoteAddr = "127.0.0.1:10000"
	r.Header.Set("Authorization", "Bearer "+localTestKey)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatal("POST unexpectedly allows model pagination query", w.Code)
	}
}
