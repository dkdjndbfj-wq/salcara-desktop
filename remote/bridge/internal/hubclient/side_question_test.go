package hubclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/png"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

type sideQuestionFixture struct {
	*sessionActionsFixture
	received agents.SideQuestionSnapshot
	bad      bool
	wait     func()
	options  *protocol.SideQuestionInputOptions
	request  protocol.SideQuestionRequest
}

func (a *sideQuestionFixture) SideQuestionCatalog(_ context.Context, id string, snapshot agents.SideQuestionSnapshot, validate func() error) (protocol.SideQuestionCatalog, error) {
	err := a.check(validate)
	a.received = snapshot
	return protocol.SideQuestionCatalog{SessionKey: "codex:" + id, Revision: snapshot.Revision, Available: true, ContextScope: "loadedSnapshot", ContextMessages: len(snapshot.Messages), ContextTruncated: snapshot.Truncated, InputOptions: a.options}, err
}
func (a *sideQuestionFixture) AskSideQuestion(_ context.Context, id string, snapshot agents.SideQuestionSnapshot, request protocol.SideQuestionRequest, validate func() error) (protocol.SideQuestionResult, error) {
	a.request = request
	if a.wait != nil {
		a.wait()
	}
	err := a.check(validate)
	a.received = snapshot
	if a.bad {
		id = "other"
	}
	return protocol.SideQuestionResult{SessionKey: "codex:" + id, OperationID: request.OperationID, State: "completed", Answer: "独立答复", ContextScope: "loadedSnapshot", ContextMessages: len(snapshot.Messages), ContextTruncated: snapshot.Truncated}, err
}

func TestSideQuestionAttachmentDispatchUsesResolvedMemoryWithoutBindingParent(t *testing.T) {
	c, a := sideFixture(t)
	a.options = &protocol.SideQuestionInputOptions{Version: 1, Images: true, ReadOnly: true, DefaultModel: "model-a"}
	rememberSideFixture(c, []protocol.Event{{SessionKey: "codex:s", ID: "u1", Type: "message", Role: "user", Text: "parent"}}, "")
	reading := sideReading(t, c)
	previous := phoneAttachments
	phoneAttachments = &attachmentStore{dir: t.TempDir(), pending: map[string]*attachmentUpload{}, bindings: map[string]map[string]bool{}}
	t.Cleanup(func() { phoneAttachments = previous })
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(buffer.Bytes())
	id := "att_11111111222243338444777777777777"
	if _, err := phoneAttachments.put(id, "image/png", 0, 1, encoded); err != nil {
		t.Fatal(err)
	}
	cmd := sideCommand(reading.Revision)
	cmd["attachments"] = []any{id}
	if _, err := c.Dispatch(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	if len(a.request.Images) != 1 || a.request.Images[0].Mime != "image/png" || a.request.Images[0].Data != encoded {
		t.Fatalf("uploaded image not forwarded: %+v", a.request.Images)
	}
	if len(phoneAttachments.bindings) != 0 || len(a.sent)+len(a.started) != 0 {
		t.Fatal("temporary image attached to parent session")
	}
	// The same provider without an advertised image contract must not accept it.
	a.options = nil
	reading = sideReading(t, c)
	cmd = sideCommand(reading.Revision)
	cmd["attachments"] = []any{id}
	a.request = protocol.SideQuestionRequest{}
	if _, err := c.Dispatch(context.Background(), cmd); err == nil || a.request.OperationID != "" {
		t.Fatalf("unadvertised image capability reached provider: %v", err)
	}
}
func sideFixture(t *testing.T) (*Client, *sideQuestionFixture) {
	c, base, _ := aggregationFixture(t, "codex")
	a := &sideQuestionFixture{sessionActionsFixture: &sessionActionsFixture{aggregationFixtureAgent: base}}
	a.descriptor.Capabilities.SideQuestion = true
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	return c, a
}
func rememberSideFixture(c *Client, events []protocol.Event, cursor string) {
	cmd := map[string]any{"sessionKey": "codex:s", "cursor": cursor}
	result := map[string]any{"session": protocol.SessionInfo{SessionKey: "codex:s", Tool: "codex", ControlSurface: "cli"}, "events": events}
	c.rememberSideQuestionHistory(context.Background(), c.o.Store.Get(), cmd, result, c.beginSideQuestionHistory("codex:s"))
}
func sideCommand(revision string) map[string]any {
	cmd := map[string]any{"type": "session.side-question.catalog", "tool": "codex", "sessionKey": "codex:s", "controlSurface": "cli"}
	if revision != "" {
		cmd["type"] = "session.side-question.ask"
		cmd["question"] = "解释这一点"
		cmd["expectedRevision"] = revision
		cmd["operationId"] = "11111111-2222-4333-8444-555555555555"
	}
	return cmd
}
func sideReading(t *testing.T, c *Client) protocol.SideQuestionCatalog {
	t.Helper()
	value, err := c.Dispatch(context.Background(), sideCommand(""))
	if err != nil {
		t.Fatal(err)
	}
	return value.(protocol.SideQuestionCatalog)
}
func TestSideQuestionLoadedSnapshotBoundary(t *testing.T) {
	c, a := sideFixture(t)
	if value := sideReading(t, c); value.Available || a.calls != 0 {
		t.Fatal("unloaded history advertised")
	}
	rememberSideFixture(c, []protocol.Event{{SessionKey: "codex:s", ID: "u1", Type: "message", Role: "user", Text: "parent question"}, {SessionKey: "codex:s", ID: "a1", Type: "message", Role: "assistant", Text: "parent response"}, {SessionKey: "codex:s", ID: "tool1", Type: "tool", Text: "secret tool output"}, {SessionKey: "codex:s", ID: "child1", Type: "message", Role: "assistant", ParentID: "child", Text: "other context"}}, "")
	first := sideReading(t, c)
	second := sideReading(t, c)
	if !first.Available || first.ContextMessages != 2 || first.Revision == second.Revision {
		t.Fatalf("bad catalogs %+v %+v", first, second)
	}
	// A later parent read cannot replace a previously opened panel's context.
	rememberSideFixture(c, []protocol.Event{{SessionKey: "codex:s", ID: "later", Type: "message", Role: "user", Text: "new parent question"}}, "")
	value, err := c.Dispatch(context.Background(), sideCommand(first.Revision))
	if err != nil {
		t.Fatal(err)
	}
	if value.(protocol.SideQuestionResult).State != "completed" || len(a.received.Messages) != 2 || a.received.Messages[0].ID != "u1" {
		t.Fatal("side panel used later parent content")
	}
	if len(a.sent)+len(a.started)+len(a.opened) != 0 {
		t.Fatal("side question resumed or wrote parent")
	}
}
func TestSideQuestionCacheBudgetsAndStaleReads(t *testing.T) {
	c, _ := sideFixture(t)
	events := []protocol.Event{}
	for i := 0; i < 100; i++ {
		events = append(events, protocol.Event{SessionKey: "codex:s", ID: fmt.Sprintf("m-%d", i), Type: "message", Role: "user", Text: strings.Repeat("a", 1024)})
	}
	rememberSideFixture(c, events, "")
	reading := sideReading(t, c)
	if reading.ContextMessages != 64 || !reading.ContextTruncated {
		t.Fatalf("unbounded snapshot %+v", reading)
	}
	old := c.beginSideQuestionHistory("codex:s")
	_ = c.beginSideQuestionHistory("codex:s")
	c.rememberSideQuestionHistory(context.Background(), c.o.Store.Get(), map[string]any{"sessionKey": "codex:s"}, map[string]any{"session": protocol.SessionInfo{SessionKey: "codex:s"}, "events": []protocol.Event{}}, old)
	if got := sideReading(t, c); !got.Available || got.ContextMessages != 64 {
		t.Fatal("late read replaced new snapshot")
	}
	c.sideQuestions.mu.Lock()
	for key, entry := range c.sideQuestions.catalogs {
		entry.at = time.Now().Add(-21 * time.Minute)
		c.sideQuestions.catalogs[key] = entry
	}
	c.sideQuestions.mu.Unlock()
	if _, err := c.Dispatch(context.Background(), sideCommand(reading.Revision)); err == nil {
		t.Fatal("expired context admitted")
	}
}
func TestSideQuestionStrictCommandAndResponse(t *testing.T) {
	for _, variant := range []string{"no-cap", "desktop", "foreign-selection", "fake-context", "assistant-history", "path", "previous-selection", "wrong-response"} {
		t.Run(variant, func(t *testing.T) {
			c, a := sideFixture(t)
			rememberSideFixture(c, []protocol.Event{{SessionKey: "codex:s", ID: "m", Type: "message", Role: "user", Text: "context"}}, "")
			reading := sideReading(t, c)
			a.calls = 0
			cmd := sideCommand(reading.Revision)
			switch variant {
			case "no-cap":
				a.descriptor.Capabilities.SideQuestion = false
			case "desktop":
				cmd["controlSurface"] = "desktop"
			case "foreign-selection":
				cmd["selection"] = map[string]any{"kind": "message", "sessionKey": "claude:other", "label": "quote", "text": "q", "capturedAt": 1, "messageId": "m", "startOffset": 0, "endOffset": 1}
			case "previous-selection":
				cmd["previousOperationId"] = "11111111-2222-4333-8444-555555555556"
				cmd["selection"] = map[string]any{"kind": "message", "sessionKey": "codex:s", "label": "quote", "text": "q", "capturedAt": 1, "messageId": "m", "startOffset": 0, "endOffset": 1}
			case "wrong-response":
				a.bad = true
			default:
				cmd[variant] = "injected"
			}
			if _, err := c.Dispatch(context.Background(), cmd); err == nil {
				t.Fatal("invalid command or result accepted")
			}
			if variant != "wrong-response" && a.calls != 0 {
				t.Fatal("invalid command reached provider")
			}
		})
	}
}
func TestSideQuestionGuardDoesNotHoldTaskLockOverNetwork(t *testing.T) {
	c, a := sideFixture(t)
	rememberSideFixture(c, []protocol.Event{{SessionKey: "codex:s", ID: "m", Type: "message", Role: "user", Text: "context"}}, "")
	reading := sideReading(t, c)
	a.wait = func() {
		locked := make(chan struct{})
		go func() { c.taskConfigMu.Lock(); close(locked); c.taskConfigMu.Unlock() }()
		select {
		case <-locked:
		case <-time.After(time.Second):
			t.Error("side question blocked task admission lock")
		}
	}
	if _, err := c.Dispatch(context.Background(), sideCommand(reading.Revision)); err != nil {
		t.Fatal(err)
	}
}
func TestSideQuestionSourceChangeBeforeAndAfterResponse(t *testing.T) {
	for _, phase := range []string{"before", "after", "aba"} {
		t.Run(phase, func(t *testing.T) {
			c, a := sideFixture(t)
			rememberSideFixture(c, []protocol.Event{{SessionKey: "codex:s", ID: "m", Type: "message", Role: "user", Text: "context"}}, "")
			reading := sideReading(t, c)
			change := func() {
				if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.CodexModel = "changed"; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "after" {
				a.after = change
			} else {
				a.before = change
			}
			if phase == "aba" {
				a.before = func() {
					original := c.o.Store.Get()
					for _, model := range []string{"other", original.CodexModel} {
						if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.CodexModel = model; return nil }); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			_, err := c.Dispatch(context.Background(), sideCommand(reading.Revision))
			if phase != "aba" && err == nil {
				t.Fatal("changed source returned reply")
			}
			// Plain model preference ABA has no credential epoch. Credential ABA is
			// covered by the source-envelope fixture in TestSideQuestionSourceABA.
		})
	}
}
func TestSideQuestionSourceABA(t *testing.T) {
	c, store, _, env := sourceGuardFixture(t, "api_key")
	_, a := sideFixture(t)
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	rememberSideFixture(c, []protocol.Event{{SessionKey: "codex:s", ID: "m", Type: "message", Role: "user", Text: "context"}}, "")
	env.Command = sideCommand("")
	env.Command["accountId"] = env.Source.CredentialHandle
	env.Source.Model = ""
	a.before = func() {
		for _, key := range []string{"key-b", "key-a"} {
			if err := store.Update(func(cfg *config.Config) error { cfg.LocalAccounts[0].Key = key; return nil }); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := c.Dispatch(withExecutionSource(context.Background(), env), env.Command); err == nil {
		t.Fatal("credential ABA returned a usable catalog")
	}
}
