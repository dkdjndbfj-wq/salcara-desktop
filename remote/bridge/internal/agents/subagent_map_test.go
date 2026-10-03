package agents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"salcara/bridge/internal/protocol"
)

func fixtureCXItem(t *testing.T, raw string) cxItem {
	t.Helper()
	var it cxItem
	if err := json.Unmarshal([]byte(raw), &it); err != nil {
		t.Fatal(err)
	}
	return it
}

func fixtureClaudeLine(t *testing.T, raw string) claudeLine {
	t.Helper()
	var l claudeLine
	if err := json.Unmarshal([]byte(raw), &l); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestSubagentCodexStatesArePerChild(t *testing.T) {
	it := fixtureCXItem(t, `{"type":"collabAgentToolCall","id":"wait1","tool":"wait","status":"completed","receiverThreadIds":["a","b","c"],"agentsStates":{"a":{"status":"running","message":"正在测试"},"b":{"status":"completed","message":"全部通过"},"c":{"status":"errored","message":"构建失败"}}}`)
	main, ok := codexItemEvent("codex:parent", it, true, "")
	children := codexSubagentEvents("codex:parent", it)
	if !ok || main.Kind != "subagent" || main.Status != "done" || len(children) != 3 {
		t.Fatalf("%+v %+v", main, children)
	}
	want := []string{"running", "done", "failed"}
	for i, e := range children {
		if e.ParentID != "wait1" || e.Status != want[i] || e.Output == "" || len(e.ChildSessionKeys) != 1 {
			t.Fatalf("child %d: %+v", i, e)
		}
	}
	th := cxThread{ID: "parent", Turns: []cxTurn{{Status: "completed", Items: []json.RawMessage{json.RawMessage(`{"type":"collabAgentToolCall","id":"spawn1","tool":"spawnAgent","status":"completed","receiverThreadIds":["a"],"agentsStates":{"a":{"status":"pendingInit","message":null}}}`)}}}}
	evs := codexHistory("codex:parent", th)
	if len(evs) != 2 || evs[1].Status != "running" || evs[1].Output != "正在启动" {
		t.Fatalf("%+v", evs)
	}
}

func TestSubagentCodexUnknownAndUnavailableRemainHonest(t *testing.T) {
	it := fixtureCXItem(t, `{"type":"collabAgentToolCall","id":"call","tool":"resumeAgent","status":"completed","receiverThreadIds":["a","../secret","a"],"agentsStates":{"b":{"status":"notFound","message":null}}}`)
	evs := codexSubagentEvents("codex:p", it)
	if len(evs) != 2 || evs[0].Status != "running" || evs[0].Output != "状态未返回" || len(evs[1].ChildSessionKeys) != 0 || evs[1].Status != "failed" {
		t.Fatalf("%+v", evs)
	}
}

func TestSubagentCodexNotifyAndRealParentMetadata(t *testing.T) {
	r := &recorder{}
	a := newCodexAgent(r.sink, func() Settings { return Settings{} }, "", "")
	defer a.thr.Close()
	a.onNotify("item/completed", json.RawMessage(`{"threadId":"p","item":{"type":"collabAgentToolCall","id":"spawn","tool":"spawnAgent","status":"completed","receiverThreadIds":["c"],"agentsStates":{"c":{"status":"running","message":"读取文件"}}}}`))
	got := r.all()
	if len(got) != 2 || got[1].ParentID != "spawn" || got[1].Output != "读取文件" {
		t.Fatalf("%+v", got)
	}
	p := "p"
	a.mu.Lock()
	info := a.applyThreadLocked(cxThread{ID: "c", ParentThreadID: &p}).info
	a.mu.Unlock()
	if info.ParentSessionKey != "codex:p" {
		t.Fatalf("%+v", info)
	}
	if !strings.Contains(strings.Join(codexSourceKinds, ","), "subAgentThreadSpawn") {
		t.Fatal("child source kinds missing")
	}
}

func TestSubagentCodexBackgroundChildBlocksAPISwitchUntilNativeTerminalState(t *testing.T) {
	a := newCodexAgent(func(protocol.Event) {}, func() Settings { return Settings{} }, "", "")
	defer a.thr.Close()
	a.mu.Lock()
	task := a.threadLocked("parent")
	task.loaded = true
	a.mu.Unlock()
	a.onNotify("item/completed", json.RawMessage(`{"threadId":"parent","item":{"type":"collabAgentToolCall","id":"spawn","tool":"spawnAgent","status":"completed","receiverThreadIds":["child"],"agentsStates":{"child":{"status":"running","message":null}}}}`))
	a.onNotify("turn/completed", json.RawMessage(`{"threadId":"parent","turn":{"id":"turn","status":"completed"}}`))
	if !a.ActiveRemoteTurns() {
		t.Fatal("parent completion hid running child")
	}
	a.onNotify("thread/status/changed", json.RawMessage(`{"threadId":"child","status":{"type":"notLoaded"}}`))
	if !a.ActiveRemoteTurns() {
		t.Fatal("unloaded child does not prove it completed")
	}
	a.onNotify("thread/status/changed", json.RawMessage(`{"threadId":"child","status":{"type":"idle"}}`))
	if a.ActiveRemoteTurns() {
		t.Fatal("terminal child permanently blocked API switch")
	}
}

func TestSubagentCodexCrashedWorkerResumesOriginalWithoutDeadChildTracker(t *testing.T) {
	f := newFixture(t, "auto_all")
	a := f.m.Get("codex").(*codexAgent)
	id, err := a.Start(context.Background(), f.workDir, "first", "", "auto_all")
	if err != nil {
		t.Fatal(err)
	}
	f.rec.wait(t, "first completion", func(e protocol.Event) bool {
		return e.Type == "turn" && e.Status == "completed" && e.SessionKey == "codex:"+id
	})
	params, _ := json.Marshal(map[string]any{"threadId": id, "item": map[string]any{"type": "collabAgentToolCall", "id": "spawn", "tool": "spawnAgent", "status": "completed", "receiverThreadIds": []string{"old-child"}, "agentsStates": map[string]any{"old-child": map[string]any{"status": "running"}}}})
	a.onNotify("item/completed", params)
	if !a.ActiveRemoteTurns() {
		t.Fatal("live child not protected")
	}
	a.mu.Lock()
	c := a.conn
	a.threads[id].childOverflow = true
	a.mu.Unlock()
	c.Kill()
	<-c.done
	a.onExit(c) // Ensure confirmed exit processed; the normal watcher is idempotent.
	if err = a.Send(context.Background(), id, "resume original"); err != nil {
		t.Fatal(err)
	}
	f.rec.wait(t, "resumed completion", func(e protocol.Event) bool { return countTurns(f.rec) >= 2 })
	a.mu.Lock()
	thread := a.threads[id]
	identity, trackers, overflow := thread.info.SessionKey, len(thread.childStates), thread.childOverflow
	a.mu.Unlock()
	if identity != "codex:"+id || trackers != 0 || overflow || a.ActiveRemoteTurns() {
		t.Fatal("dead worker epoch changed identity or kept admission locked")
	}
	// A real event in the new epoch must protect a newly active child again.
	a.onNotify("item/completed", params)
	if !a.ActiveRemoteTurns() {
		t.Fatal("new process child state ignored")
	}
}

func TestSubagentCodexDeadEpochIsClearedBeforeExitWatcherRuns(t *testing.T) {
	f := newFixture(t, "auto_all")
	a := f.m.Get("codex").(*codexAgent)
	done := make(chan struct{})
	close(done)
	dead := &rpcConn{closed: true, done: done}
	a.mu.Lock()
	a.conn = dead
	thread := a.threadLocked("original")
	thread.loaded, thread.childOverflow = true, true
	thread.childStates = map[string]string{"old-child": "running"}
	a.mu.Unlock()
	if _, err := a.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	stale := thread.loaded || thread.childOverflow || len(thread.childStates) != 0 || a.conn == dead
	identity := thread.info.SessionKey
	a.mu.Unlock()
	if stale || identity != "codex:original" {
		t.Fatal("new process inherited a dead epoch before watcher cleanup")
	}
}

func TestSubagentClaudeInternalMessagesToolsAndReasoningAreGrouped(t *testing.T) {
	m := newClaudeMapper("claude:p", "")
	parent := fixtureClaudeLine(t, `{"type":"assistant","message":{"id":"main","content":[{"type":"tool_use","id":"task1","name":"Agent","input":{"description":"测试","prompt":"检查代码"}}]}}`)
	if evs := m.entry(&parent); len(evs) != 1 || evs[0].Kind != "subagent" {
		t.Fatalf("%+v", evs)
	}
	child := fixtureClaudeLine(t, `{"type":"assistant","parent_tool_use_id":"task1","isSidechain":true,"message":{"id":"child","content":[{"type":"thinking","thinking":"先读取"},{"type":"text","text":"开始检查"},{"type":"tool_use","id":"read1","name":"Read","input":{"file_path":"fixture.go"}}]}}`)
	evs := m.entry(&child)
	if len(evs) != 3 {
		t.Fatalf("%+v", evs)
	}
	for _, e := range evs {
		if e.ParentID != "task1" || e.SessionKey != "claude:p" || len(e.ChildSessionKeys) != 0 {
			t.Fatalf("%+v", e)
		}
	}
	result := fixtureClaudeLine(t, `{"type":"user","parent_tool_use_id":"task1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"read1","content":"文件内容"}]}}`)
	evs = m.entry(&result)
	if len(evs) != 1 || evs[0].Kind != "read" || evs[0].ParentID != "task1" || evs[0].Output != "文件内容" {
		t.Fatalf("%+v", evs)
	}
	end := fixtureClaudeLine(t, `{"type":"result","subtype":"success","parent_tool_use_id":"task1","result":"检查完成"}`)
	evs = m.entry(&end)
	if len(evs) != 1 || evs[0].ID != "task1" || evs[0].Status != "done" || evs[0].Output != "检查完成" {
		t.Fatalf("%+v", evs)
	}
}

func TestSubagentClaudeChildResultDoesNotFinishParentTurn(t *testing.T) {
	r := &recorder{}
	a := newClaudeAgent(r.sink, func() Settings { return Settings{} }, "", t.TempDir())
	defer a.thr.Close()
	p := &claudeProc{id: "p", turns: 1, mapper: newClaudeMapper("claude:p", "")}
	a.handleLine(p, []byte(`{"type":"result","parent_tool_use_id":"task1","subtype":"success","result":"子任务完成"}`))
	if p.turns != 1 {
		t.Fatal("child result consumed main turn")
	}
	for _, e := range r.all() {
		if e.Type == "turn" || e.Type == "session.updated" {
			t.Fatalf("unexpected parent mutation: %+v", e)
		}
	}
}

func TestSubagentClaudeBackgroundLifecycleIsAuthoritative(t *testing.T) {
	m := newClaudeMapper("claude:p", "")
	l := fixtureClaudeLine(t, `{"type":"assistant","message":{"id":"main","content":[{"type":"tool_use","id":"task1","name":"Task","input":{"description":"后台检查","run_in_background":true}}]}}`)
	m.entry(&l)
	l = fixtureClaudeLine(t, `{"type":"system","subtype":"task_started","task_id":"native-task","tool_use_id":"task1","description":"后台检查"}`)
	evs := m.entry(&l)
	if evs[0].Kind != "subagent" || evs[0].Status != "running" {
		t.Fatalf("%+v", evs)
	}
	l = fixtureClaudeLine(t, `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"task1","content":"已启动"}]}}`)
	if evs = m.entry(&l); evs[0].Status != "running" {
		t.Fatalf("launch is not completion: %+v", evs)
	}
	l = fixtureClaudeLine(t, `{"type":"system","subtype":"task_progress","task_id":"native-task","tool_use_id":"task1","last_tool_name":"Grep","usage":{"duration_ms":1234}}`)
	if evs = m.entry(&l); evs[0].DurationMS != 1234 || evs[0].Output != "正在使用 Grep" {
		t.Fatalf("%+v", evs)
	}
	l = fixtureClaudeLine(t, `{"type":"system","subtype":"task_updated","task_id":"native-task","patch":{"status":"killed"}}`)
	if evs = m.entry(&l); evs[0].Status != "failed" || evs[0].ID != "task1" {
		t.Fatalf("%+v", evs)
	}
	l = fixtureClaudeLine(t, `{"type":"system","subtype":"task_notification","task_id":"native-task","tool_use_id":"task1","status":"completed","summary":"真实结果"}`)
	m.entry(&l)
	l = fixtureClaudeLine(t, `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"task1","content":"迟到的启动确认"}]}}`)
	if evs = m.entry(&l); evs[0].Status != "done" || evs[0].Output != "真实结果" {
		t.Fatalf("late launch overwrote result: %+v", evs)
	}
}

func TestSubagentClaudeTodoWriteKeepsTypedChecklistAfterAck(t *testing.T) {
	m := newClaudeMapper("claude:p", "")
	l := fixtureClaudeLine(t, `{"type":"assistant","message":{"id":"main","content":[{"type":"tool_use","id":"todo","name":"TodoWrite","input":{"todos":[{"content":"读取","status":"completed"},{"content":"修改","status":"in_progress"},{"content":"测试","status":"pending"}]}}]}}`)
	evs := m.entry(&l)
	if len(evs) != 1 || evs[0].Kind != "plan" || evs[0].Output != "✓ 读取\n▸ 修改\n○ 测试" {
		t.Fatalf("%+v", evs)
	}
	l = fixtureClaudeLine(t, `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"todo","content":"Todos have been modified successfully."}]}}`)
	if evs = m.entry(&l); evs[0].Output != "✓ 读取\n▸ 修改\n○ 测试" || evs[0].Status != "running" {
		t.Fatalf("ack replaced checklist: %+v", evs)
	}
}

func TestSubagentClaudeWorkerSettingsSigTracksCredentialsAndDoesNotExposeThem(t *testing.T) {
	s := Settings{ClaudeRoot: "https://fixture.test/", ClaudeKey: "secret-fixture", ClaudeAuthMode: "api-key", ClaudeModel: "fixture-model", ClaudePath: "fixture-path"}
	sig := claudeSettingsSig(s, "")
	if len(sig) != 64 || strings.Contains(sig, s.ClaudeKey) {
		t.Fatal("unsafe signature")
	}
	for _, change := range []func(*Settings){func(s *Settings) { s.ClaudeKey = "new" }, func(s *Settings) { s.ClaudeRoot = "other" }, func(s *Settings) { s.ClaudeAuthMode = "bearer" }, func(s *Settings) { s.ClaudePath = "other" }, func(s *Settings) { s.ClaudeModel = "other" }} {
		copyS := s
		change(&copyS)
		if claudeSettingsSig(copyS, "") == sig {
			t.Fatal("changed credentials reused process")
		}
	}
}

func TestSubagentClaudeBusyCredentialChangeDoesNotWriteOrKill(t *testing.T) {
	a := newClaudeAgent(func(protocol.Event) {}, func() Settings { return Settings{ClaudeKey: "new"} }, "", t.TempDir())
	defer a.thr.Close()
	p := &claudeProc{id: "p", turns: 1, done: make(chan struct{}), workerSettingsSig: claudeSettingsSig(Settings{ClaudeKey: "old"}, "")}
	a.procs["p"] = p
	if err := a.SendWithOptions(context.Background(), "p", "不该发送", TurnOptions{}); err == nil || !strings.Contains(err.Error(), "结束后") {
		t.Fatalf("%v", err)
	}
	if p.stopped {
		t.Fatal("active task killed")
	}
}

func TestSubagentClaudeIdleAPIChangeResumesSameSession(t *testing.T) {
	f := newFixture(t, "auto_all")
	ctx := context.Background()
	cl := f.m.Get("claude")
	id, err := cl.Start(ctx, f.workDir, "fixture first turn", "", "auto_all")
	if err != nil {
		t.Fatal(err)
	}
	f.rec.wait(t, "initial completed turn", func(e protocol.Event) bool {
		return e.Type == "turn" && e.Status == "completed" && e.SessionKey == "claude:"+id
	})
	f.mu.Lock()
	f.s.ClaudeKey, f.s.ClaudeAuthMode = "new-fixture-key", "api-key"
	f.mu.Unlock()
	if err = cl.Send(ctx, id, "fixture second turn"); err != nil {
		t.Fatal(err)
	}
	f.rec.wait(t, "resumed completed turn", func(e protocol.Event) bool { return countTurns(f.rec) >= 2 })
	args := readLog(t, f.logDir, "claude-args.jsonl")
	if len(args) != 2 {
		t.Fatalf("expected a restarted worker, got %d", len(args))
	}
	last := args[1]
	if last["apikey"] != "new-fixture-key" || last["token"] != "" {
		t.Fatalf("wrong fixture auth mode: %+v", last)
	}
	joined := strings.Join(toStrings(last["args"]), " ")
	if !strings.Contains(joined, "--resume "+id) || strings.Contains(joined, "--session-id") {
		t.Fatalf("history identity changed: %s", joined)
	}
	for _, e := range f.rec.all() {
		if e.Type == "turn" && (e.Status == "failed" || e.Status == "interrupted") {
			t.Fatalf("API switch interrupted completed session: %+v", e)
		}
	}
}

func TestSubagentClaudeBackgroundWorkBlocksCredentialRestart(t *testing.T) {
	a := newClaudeAgent(func(protocol.Event) {}, func() Settings { return Settings{ClaudeKey: "new"} }, "", t.TempDir())
	defer a.thr.Close()
	p := &claudeProc{id: "p", done: make(chan struct{}), workerSettingsSig: claudeSettingsSig(Settings{ClaudeKey: "old"}, "")}
	p.activeSubtasks.Store(true)
	a.procs["p"] = p
	if err := a.SendWithOptions(context.Background(), "p", "不要发送", TurnOptions{}); err == nil {
		t.Fatal("restarted unfinished child task")
	}
	if p.stopped {
		t.Fatal("background task killed")
	}
	a.mu.Lock()
	info := protocol.SessionInfo{SessionKey: "claude:p"}
	a.decorateLocked("p", &info)
	a.mu.Unlock()
	if info.Status != "running" {
		t.Fatalf("family guard cannot see active child: %+v", info)
	}
}

func TestSubagentClaudeHistoryReadsOnlyReferencedRealChild(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Join(dir, "project")
	children := filepath.Join(project, "parent", "subagents")
	if err := os.MkdirAll(children, 0o700); err != nil {
		t.Fatal(err)
	}
	main := `{"type":"user","uuid":"u","timestamp":"2026-10-02T01:00:00Z","message":{"content":"检查代码"}}
{"type":"assistant","timestamp":"2026-10-02T01:00:01Z","message":{"id":"m","content":[{"type":"tool_use","id":"task1","name":"Agent","input":{"description":"检查","prompt":"检查代码"}}]}}
{"type":"user","timestamp":"2026-10-02T01:00:04Z","message":{"content":[{"type":"tool_result","tool_use_id":"task1","content":"检查完成\nagentId: actual-child"}]}}
`
	child := `{"type":"assistant","isSidechain":true,"timestamp":"2026-10-02T01:00:02Z","message":{"id":"child-msg","content":[{"type":"thinking","thinking":"先读取"},{"type":"text","text":"发现问题"},{"type":"tool_use","id":"read1","name":"Read","input":{"file_path":"fixture.go"}}]}}
{"type":"user","isSidechain":true,"timestamp":"2026-10-02T01:00:03Z","message":{"content":[{"type":"tool_result","tool_use_id":"read1","content":"内容"}]}}
`
	if err := os.WriteFile(filepath.Join(project, "parent.jsonl"), []byte(main), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(children, "agent-actual-child.jsonl"), []byte(child), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(children, "agent-unreferenced.jsonl"), []byte(strings.ReplaceAll(child, "发现问题", "不应读取")), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newClaudeHistory(dir)
	_, evs, err := h.open("parent")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, e := range evs {
		if strings.Contains(e.Text, "不应读取") {
			t.Fatal("unreferenced file leaked")
		}
		if e.ID == "child-msg:0" || e.ID == "child-msg:1" || e.ID == "read1" {
			found++
			if e.ParentID != "task1" || len(e.ChildSessionKeys) != 0 {
				t.Fatalf("wrong child scope: %+v", e)
			}
		}
	}
	if found != 3 || len(h.list()) != 1 {
		t.Fatalf("real child history missing: %+v", evs)
	}
	if claudeResultAgentID("agentId: ../../secret") != "" {
		t.Fatal("accepted path as agent ID")
	}
}

func TestSubagentClaudeForegroundAckClearsInternalWork(t *testing.T) {
	m := newClaudeMapper("claude:p", "")
	l := fixtureClaudeLine(t, `{"type":"assistant","message":{"id":"main","content":[{"type":"tool_use","id":"task1","name":"Agent","input":{"description":"前台检查"}}]}}`)
	m.entry(&l)
	l = fixtureClaudeLine(t, `{"type":"assistant","parent_tool_use_id":"task1","message":{"id":"child","content":[{"type":"text","text":"检查"}]}}`)
	m.entry(&l)
	l = fixtureClaudeLine(t, `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"task1","content":"检查完成"}]}}`)
	evs := m.entry(&l)
	if evs[0].Status != "done" || m.hasActiveSubtasks() {
		t.Fatalf("completed foreground task stuck: %+v", evs)
	}
}
