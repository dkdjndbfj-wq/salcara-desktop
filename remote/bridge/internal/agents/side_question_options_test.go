package agents

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestSideQuestionRichInputsKeepReadOnlyTransport(t *testing.T) {
	for _, family := range []string{"codex", "claude"} {
		t.Run(family, func(t *testing.T) {
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if family == "codex" {
					_, _ = w.Write([]byte(codexSideAnswer))
				} else {
					writeSideAnswer(w, "Image answer")
				}
			}))
			defer server.Close()
			input := sideQuestionInput{Snapshot: SideQuestionSnapshot{Revision: "snapshot", Messages: []SideQuestionMessage{{ID: "u1", Role: "user", Text: "parent"}}}, Question: "Explain this image", Model: "selected-model", Images: []protocol.SideQuestionImage{{Mime: "image/png", Data: "YWJj"}}}
			var err error
			if family == "codex" {
				input.Effort = "high"
				_, _, err = (codexSideQuestionConnection{root: server.URL + "/gateway/remote-codex", key: "fixture", model: "default-model"}).ask(context.Background(), input)
			} else {
				_, _, err = (claudeSideQuestionConnection{root: server.URL + "/gateway/remote-claude", key: "fixture", model: "default-model"}).ask(context.Background(), input)
			}
			if err != nil {
				t.Fatal(err)
			}
			if body["model"] != "selected-model" {
				t.Fatalf("model override dropped: %#v", body)
			}
			if family == "codex" {
				if body["store"] != false || body["tool_choice"] != "none" || len(body["tools"].([]any)) != 0 || body["reasoning"].(map[string]any)["effort"] != "high" {
					t.Fatalf("read-only reasoning contract changed: %#v", body)
				}
				blocks := body["input"].([]any)[0].(map[string]any)["content"].([]any)
				if blocks[len(blocks)-1].(map[string]any)["image_url"] != "data:image/png;base64,YWJj" {
					t.Fatalf("image dropped: %#v", blocks)
				}
			} else {
				if body["tool_choice"].(map[string]any)["type"] != "none" {
					t.Fatal("Claude gained tools")
				}
				blocks := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
				if blocks[len(blocks)-1].(map[string]any)["source"].(map[string]any)["data"] != "YWJj" {
					t.Fatalf("image dropped: %#v", blocks)
				}
			}
		})
	}
}

func TestSideQuestionImagesStayInEphemeralChainAndReceiptIdentity(t *testing.T) {
	var requests []map[string]any
	f := newClaudeSideFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		requests = append(requests, body)
		writeSideAnswer(w, "Image answer")
	}))
	view := f.catalog(t)
	if view.InputOptions == nil || !view.InputOptions.Images || !view.InputOptions.Model || view.InputOptions.Effort || !view.InputOptions.ReadOnly {
		t.Fatalf("wrong negotiated input options: %+v", view)
	}
	req := sideFixtureRequest(view)
	req.Images = []protocol.SideQuestionImage{{Mime: "image/png", Data: "YWJj"}}
	req.Attachments = []string{"att_" + strings.ReplaceAll(newUUID(), "-", "")}
	if result, err := f.a.AskSideQuestion(f.ctx, f.p.id, f.snapshot, req, f.guard); err != nil || result.State != "completed" {
		t.Fatalf("send: %+v %v", result, err)
	}
	changed := req
	changed.Images = []protocol.SideQuestionImage{{Mime: "image/png", Data: "ZGVm"}}
	if _, err := f.a.AskSideQuestion(f.ctx, f.p.id, f.snapshot, changed, f.guard); !errors.Is(err, errSideQuestionStale) {
		t.Fatalf("changed image reused old receipt: %v", err)
	}
	next := protocol.SideQuestionRequest{Question: "Follow up", OperationID: newUUID(), PreviousOperationID: req.OperationID, ExpectedRevision: view.Revision}
	if result, err := f.a.AskSideQuestion(f.ctx, f.p.id, f.snapshot, next, f.guard); err != nil || result.State != "completed" {
		t.Fatalf("followup: %+v %v", result, err)
	}
	if len(requests) != 2 {
		t.Fatalf("unexpected request count %d", len(requests))
	}
	first := requests[1]["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if first[len(first)-1].(map[string]any)["source"].(map[string]any)["data"] != "YWJj" {
		t.Fatal("previous image not retained")
	}
	if len(f.a.sideQuestions.documents) != 0 || f.writes.Load() != 0 {
		t.Fatal("temporary images touched persistent conversation or parent")
	}
}
