package agents

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCodexLateItemCompletionKeepsNativeTurnIdentity(t *testing.T) {
	for _, item := range []string{
		`{"type":"commandExecution","id":"old-command","command":"fixture","status":"completed","aggregatedOutput":"done"}`,
		`{"type":"agentMessage","id":"old-answer","text":"old answer","phase":"final_answer"}`,
		`{"type":"collabAgentToolCall","id":"old-wait","tool":"wait","status":"completed","receiverThreadIds":["child"],"agentsStates":{"child":{"status":"completed","message":"child completed"}}}`,
	} {
		for _, successor := range []bool{false, true} {
			r := &recorder{}
			a := newCodexAgent(r.sink, func() Settings { return Settings{} }, "", "")
			a.onNotify("turn/started", json.RawMessage(`{"threadId":"parent","turn":{"id":"original"}}`))
			a.onNotify("turn/completed", json.RawMessage(`{"threadId":"parent","turn":{"id":"original","status":"completed"}}`))
			if successor {
				a.onNotify("turn/started", json.RawMessage(`{"threadId":"parent","turn":{"id":"successor"}}`))
			}
			before := len(r.all())
			a.onNotify("item/completed", json.RawMessage(`{"threadId":"parent","turnId":"original","item":`+item+`}`))
			events := r.all()[before:]
			if len(events) == 0 {
				t.Fatal("late completion was lost")
			}
			for _, event := range events {
				if event.TurnID != "original" {
					t.Fatalf("late item borrowed current turn: %+v", event)
				}
			}
			active := a.threadLocked("parent").turnID
			if successor && active != "successor" || !successor && active != "" {
				t.Fatalf("item changed active turn: %s", active)
			}
			a.thr.Close()
		}
	}
}

func TestCodexItemTurnFallbackDoesNotBorrowForMalformedIdentity(t *testing.T) {
	for _, fixture := range []struct{ field, want string }{
		{"", "active"},
		{`,"turnId":"native-turn"`, "native-turn"},
		{`,"turnId":"bad identity"`, ""},
	} {
		r := &recorder{}
		a := newCodexAgent(r.sink, func() Settings { return Settings{} }, "", "")
		a.threadLocked("parent").turnID = "active"
		a.onNotify("item/started", json.RawMessage(`{"threadId":"parent"`+fixture.field+`,"item":{"type":"commandExecution","id":"tool","command":"fixture","status":"inProgress"}}`))
		got := r.all()
		if len(got) != 1 || got[0].TurnID != fixture.want {
			t.Fatalf("turn fallback changed: %+v", got)
		}
		a.thr.Close()
	}
}

func TestCodexDialogueAndToolNotificationsRetainDistinctOrderedItems(t *testing.T) {
	r := &recorder{}
	a := newCodexAgent(r.sink, func() Settings { return Settings{} }, "", "")
	defer a.thr.Close()
	for _, fixture := range []struct{ method, item string }{
		{"item/completed", `{"type":"agentMessage","id":"progress-one","text":"checking files","phase":"commentary"}`},
		{"item/started", `{"type":"commandExecution","id":"command","command":"fixture","status":"inProgress"}`},
		{"item/completed", `{"type":"agentMessage","id":"progress-two","text":"command still running","phase":"commentary"}`},
		{"item/completed", `{"type":"commandExecution","id":"command","command":"fixture","status":"completed","aggregatedOutput":"PASS"}`},
		{"item/started", `{"type":"imageView","id":"image","path":"fixture.png"}`},
		{"item/completed", `{"type":"imageView","id":"image","path":"fixture.png"}`},
		{"item/completed", `{"type":"collabAgentToolCall","id":"wait","tool":"wait","status":"completed","receiverThreadIds":["child"],"agentsStates":{"child":{"status":"completed","message":"child result"}}}`},
		{"item/completed", `{"type":"agentMessage","id":"answer","text":"final answer","phase":"final_answer"}`},
	} {
		a.onNotify(fixture.method, json.RawMessage(`{"threadId":"parent","turnId":"turn","item":`+fixture.item+`}`))
	}
	var order []string
	for _, event := range r.all() {
		order = append(order, event.ID+":"+event.Type+":"+event.Status+":"+event.MessagePhase)
		if event.TurnID != "turn" {
			t.Fatalf("missing scope on timeline event: %+v", event)
		}
	}
	want := []string{"progress-one:message::commentary", "command:tool:running:", "progress-two:message::commentary", "command:tool:done:", "image:tool:running:", "image:tool:done:", "wait:tool:done:", "wait:child:child:tool:done:", "answer:message::final_answer"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("body/tool order or phase changed:\n got %v\nwant %v", order, want)
	}
}

func TestCodexImageViewTracksNativeStartAndCompletion(t *testing.T) {
	item := fixtureCXItem(t, `{"type":"imageView","id":"image","path":"fixture.png"}`)
	for _, completed := range []bool{false, true} {
		event, ok := codexItemEvent("codex:parent", item, completed, "")
		want := "running"
		if completed {
			want = "done"
		}
		if !ok || event.Kind != "image_view" || event.Status != want || event.ID != "image" {
			t.Fatalf("image lifecycle fabricated: %+v", event)
		}
	}
}
