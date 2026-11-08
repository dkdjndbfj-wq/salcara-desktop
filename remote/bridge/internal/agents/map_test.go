package agents

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

func TestUnifiedDiff(t *testing.T) {
	d := unifiedDiff("f.txt", "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\n", "a\nb\nc\nd\nE\nf\ng\nh\ni\nj\nk\n")
	want := "--- a/f.txt\n+++ b/f.txt\n@@ -2,9 +2,10 @@\n b\n c\n d\n-e\n+E\n f\n g\n h\n i\n j\n+k\n"
	if d != want {
		t.Fatalf("got\n%s\nwant\n%s", d, want)
	}
	add := addedFileDiff("n.go", "x\ny\n")
	if add != "--- /dev/null\n+++ b/n.go\n@@ -0,0 +1,2 @@\n+x\n+y\n" {
		t.Fatalf("add %q", add)
	}
	if unifiedDiff("s", "same\n", "same\n") != "" {
		t.Fatal("no-op diff")
	}
	far := unifiedDiff("g", "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n", "X\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\nY\n")
	if strings.Count(far, "@@ -") != 2 {
		t.Fatalf("expected two hunks:\n%s", far)
	}
}

func TestCodexItemMapping(t *testing.T) {
	cases := []struct {
		item string
		kind string
		want string
	}{
		{`{"type":"commandExecution","id":"1","command":"rg foo","cwd":"/w","status":"completed","exitCode":1,"commandActions":[{"type":"search","command":"rg foo","query":"foo","path":null}],"aggregatedOutput":"x"}`, "search", "搜索 “foo”|failed"},
		{`{"type":"commandExecution","id":"1","command":"go test ./...","cwd":"/w","status":"inProgress","commandActions":[{"type":"unknown","command":"go test ./..."}]}`, "command", "运行 go test ./...|running"},
		{`{"type":"fileChange","id":"2","status":"completed","changes":[{"path":"/w/new.txt","kind":{"type":"add"},"diff":"hi\n"}]}`, "file_change", "创建 new.txt|done"},
		{`{"type":"mcpToolCall","id":"3","server":"gh","tool":"search","status":"completed","arguments":{"q":1},"result":{"content":[{"type":"text","text":"r"}]},"error":null}`, "mcp", "调用 gh.search|done"},
		{`{"type":"webSearch","id":"4","query":"go generics","action":{"type":"search","query":"go generics"}}`, "web", "搜索网页 “go generics”|done"},
		{`{"type":"imageView","id":"4b","path":"C:\\work\\input.png"}`, "image_view", "查看图片|done"},
		{`{"type":"imageGeneration","id":"5","status":"completed","savedPath":"C:\\work\\generated.png"}`, "image_generation", "生成图片|done"},
		{`{"type":"imageGeneration","id":"6","status":"completed","result":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAAB"}`, "image_generation", "生成图片|done"},
	}
	for _, c := range cases {
		var it cxItem
		if err := json.Unmarshal([]byte(c.item), &it); err != nil {
			t.Fatal(err)
		}
		e, ok := codexItemEvent("codex:t", it, true, "/w")
		if !ok || e.Kind != c.kind || e.Title+"|"+e.Status != c.want {
			t.Fatalf("%s → %+v", c.item, e)
		}
		if it.Type == "fileChange" && !strings.Contains(e.Diff, "+++ b/w/new.txt\n@@ -0,0 +1,1 @@\n+hi") {
			t.Fatalf("diff %q", e.Diff)
		}
		if it.Type == "imageGeneration" && strings.Contains(c.item, `"id":"6"`) && !strings.HasPrefix(e.Detail, "image-base64:") {
			t.Fatalf("base64 image fallback missing: %+v", e)
		}
	}
	if codexClient(json.RawMessage(`"appServer"`), "", false) != "Codex App" ||
		codexClient(json.RawMessage(`"vscode"`), "Codex Desktop", false) != "Codex App" ||
		codexClient(json.RawMessage(`{"custom":"x"}`), "", false) != "Codex" ||
		codexClient(json.RawMessage(`"cli"`), "", true) != "Codex" {
		t.Fatal("client mapping")
	}
}

func TestClaudeToolDescriptions(t *testing.T) {
	if d := describeClaudeTool("Skill", map[string]any{"skill": "review", "args": "检查代码"}, "", false); d.kind != "skill" || d.title != "使用技能 · review" || d.detail != "检查代码" {
		t.Fatalf("skill activity is not mapped explicitly: %+v", d)
	}
	d := describeClaudeTool("TodoWrite", map[string]any{"todos": []any{
		map[string]any{"content": "a", "status": "completed"}, map[string]any{"content": "b", "status": "in_progress"}}}, "", false)
	if d.kind != "plan" || d.title != "计划 · 1/2" || d.detail != "✓ a\n▸ b" {
		t.Fatalf("%+v", d)
	}
	if d := describeClaudeTool("mcp__github__create_pr", map[string]any{}, "", false); d.kind != "mcp" || d.title != "调用 github.create_pr" {
		t.Fatalf("%+v", d)
	}
	if d := describeClaudeTool("Grep", map[string]any{"pattern": "useEffect"}, "", false); d.kind != "search" || d.title != "搜索 “useEffect”" {
		t.Fatalf("%+v", d)
	}
	if d := describeClaudeTool("Write", map[string]any{"file_path": "/w/x.go", "content": "a\n"}, "/w", false); d.title != "创建 x.go" || !strings.Contains(d.diff, "+a") {
		t.Fatalf("%+v", d)
	}
	long := strings.Repeat("长", 200)
	if d := describeClaudeTool("Bash", map[string]any{"command": long}, "", false); len([]rune(d.title)) != maxTitleChars {
		t.Fatalf("title not truncated: %d", len([]rune(d.title)))
	}
}

func TestClaudeImagesRemainOpaqueUntilHubDecoration(t *testing.T) {
	raw := `{"id":"msg-image","role":"assistant","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="}}]}`
	m := newClaudeMapper("claude:image", "")
	events := m.assistant([]byte(raw), "msg-image", 1)
	if len(events) != 1 || events[0].Text != "[图片]" || len(events[0].ImageData) != 1 {
		t.Fatalf("image block was not retained for private decoration: %+v", events)
	}
}

func TestClaudeStreamIDsMatchFinal(t *testing.T) {
	m := newClaudeMapper("claude:s", "")
	m.stream(json.RawMessage(`{"type":"message_start","message":{"id":"msg_X"}}`))
	m.stream(json.RawMessage(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`))
	p := m.stream(json.RawMessage(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hm"}}`))
	m.stream(json.RawMessage(`{"type":"content_block_start","index":1,"content_block":{"type":"text"}}`))
	q1 := m.stream(json.RawMessage(`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"一"}}`))
	q := m.stream(json.RawMessage(`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"起"}}`))
	f1 := m.assistant(json.RawMessage(`{"id":"msg_X","content":[{"type":"thinking","thinking":"hm"}]}`), "", 1)
	f2 := m.assistant(json.RawMessage(`{"id":"msg_X","content":[{"type":"text","text":"一起"}]}`), "", 1)
	if p[0].ID != f1[0].ID || q[0].ID != f2[0].ID || p[0].Type != "reasoning" || q[0].Type != "message" ||
		q1[0].Final || q[0].Final || q1[0].Text != "一" || q[0].Text != "一起" || !f2[0].Final || f2[0].Text != q[0].Text {
		t.Fatalf("%+v %+v %+v %+v", p, q, f1, f2)
	}
}

func TestThrottle(t *testing.T) {
	var got []protocol.Event
	ch := make(chan struct{}, 10)
	th := newThrottle(func(e protocol.Event) { got = append(got, e); ch <- struct{}{} })
	for i := 0; i < 50; i++ {
		th.Update(protocol.Event{Type: "message", ID: "a", Text: strings.Repeat("x", i)})
	}
	<-ch // immediate first
	th.Final(protocol.Event{Type: "message", ID: "a", Text: "done", Final: true})
	<-ch
	time.Sleep(2 * streamInterval)
	if len(got) != 2 || got[1].Text != "done" {
		t.Fatalf("got %d events: %+v", len(got), got)
	}
}

func TestThrottleFinalWaitsForInFlightTimer(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	events := make(chan protocol.Event, 8)
	var once sync.Once
	th := newThrottle(func(e protocol.Event) {
		if e.Text == "partial" {
			once.Do(func() { close(started) })
			<-release
		}
		events <- e
	})
	key := func(text string) protocol.Event { return protocol.Event{Type: "message", ID: "ordered", Text: text} }
	th.Update(key("initial")) // immediate emission
	th.Update(key("partial")) // timer emission
	select {
	case <-started:
	case <-time.After(2 * streamInterval):
		t.Fatal("timer partial did not start")
	}
	// Final must wait for the timer's sink call. If it emits first, the old
	// partial can arrive after the terminal event and corrupt the phone timeline.
	done := make(chan struct{})
	go func() {
		th.Final(protocol.Event{Type: "message", ID: "ordered", Text: "final", Final: true})
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Final overtook an in-flight partial")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Final did not complete after the partial drained")
	}
	var got []string
	for len(got) < 3 {
		select {
		case e := <-events:
			got = append(got, e.Text)
		case <-time.After(time.Second):
			t.Fatalf("missing ordered events: %v", got)
		}
	}
	if got[0] != "initial" || got[1] != "partial" || got[2] != "final" {
		t.Fatalf("events out of order: %v", got)
	}
}

func TestThrottleBoundsUnterminatedStreams(t *testing.T) {
	th := newThrottle(func(protocol.Event) {})
	for i := 0; i < maxThrottlePending+128; i++ {
		th.Update(protocol.Event{Type: "message", SessionKey: "bounded", ID: fmt.Sprintf("stream-%d", i), Text: "partial"})
	}
	th.mu.Lock()
	got := len(th.pending)
	th.mu.Unlock()
	if got > maxThrottlePending {
		t.Fatalf("unterminated stream state exceeded bound: %d", got)
	}
}

func TestThrottleCapacityRefusesPartialButAlwaysEmitsFinal(t *testing.T) {
	final := make(chan struct{}, 1)
	th := newThrottle(func(e protocol.Event) {
		if e.Final {
			final <- struct{}{}
		}
	})
	for i := 0; i < maxThrottlePending; i++ {
		th.Update(protocol.Event{Type: "message", SessionKey: "bounded", ID: fmt.Sprintf("stream-%d", i), Text: "partial"})
	}
	th.Update(protocol.Event{Type: "message", SessionKey: "bounded", ID: "overflow", Text: "partial"})
	th.mu.Lock()
	if len(th.pending) != maxThrottlePending {
		t.Fatalf("partial capacity changed: %d", len(th.pending))
	}
	th.mu.Unlock()
	// Final is not a partial admission and must remain deliverable while the
	// bounded stream table is full.
	th.Final(protocol.Event{Type: "message", SessionKey: "bounded", ID: "overflow", Text: "final", Final: true})
	select {
	case <-final:
	case <-time.After(time.Second):
		t.Fatal("capacity overflow lost its terminal event")
	}
}

func TestThrottleDropCancelsPendingAndCloseRejectsNewEvents(t *testing.T) {
	events := make(chan protocol.Event, 4)
	th := newThrottle(func(e protocol.Event) { events <- e })
	e := protocol.Event{Type: "message", SessionKey: "drop", ID: "one", Text: "partial"}
	th.Update(e)
	th.Drop(e)
	th.mu.Lock()
	if len(th.pending) != 0 {
		t.Fatalf("Drop retained pending state: %d", len(th.pending))
	}
	th.mu.Unlock()
	// The event was emitted immediately, but Drop ensures no later timer
	// snapshot can be produced for the same entry.
	select {
	case <-events:
	default:
	}
	time.Sleep(streamInterval + 20*time.Millisecond)
	select {
	case got := <-events:
		t.Fatalf("Drop allowed a late event: %+v", got)
	default:
	}
	th.Close()
	th.Final(protocol.Event{Type: "message", SessionKey: "drop", ID: "closed", Text: "final", Final: true})
	th.Update(protocol.Event{Type: "message", SessionKey: "drop", ID: "closed", Text: "partial"})
	select {
	case got := <-events:
		t.Fatalf("Close emitted after shutdown: %+v", got)
	default:
	}
}

func TestDedupe(t *testing.T) {
	evs := []protocol.Event{{Type: "tool", ID: "1", Status: "running"}, {Type: "message", ID: "m"}, {Type: "tool", ID: "1", Status: "done"}, {Type: "turn"}}
	out := dedupeEvents(evs, 0)
	if len(out) != 3 || out[0].Status != "done" {
		t.Fatalf("%+v", out)
	}
}

func TestCodexPlanBecomesChecklist(t *testing.T) {
	e := codexPlanEvent("codex:t", "plan:1", []cxPlanStep{{"读代码", "completed"}, {"改界面", "inProgress"}, {"跑测试", "pending"}})
	if e.Kind != "plan" || e.Status != "running" || e.Title != "计划 · 1/3" || e.Output != "✓ 读代码\n▸ 改界面\n○ 跑测试" {
		t.Fatalf("%+v", e)
	}
}
