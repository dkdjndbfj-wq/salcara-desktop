package hubclient

import (
	"context"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

type persistentSideFixtureAgent struct {
	*sideQuestionFixture
	owner   string
	request protocol.SideQuestionConversationRequest
	alter   func(*protocol.SideQuestionConversation)
}

func (a *persistentSideFixtureAgent) SideQuestionConversation(_ context.Context, id, owner string, snapshot agents.SideQuestionSnapshot, request protocol.SideQuestionConversationRequest, guard func() error) (protocol.SideQuestionConversation, error) {
	a.owner, a.received, a.request = owner, snapshot, request
	err := a.check(guard)
	value := protocol.SideQuestionConversation{SideQuestionCatalog: protocol.SideQuestionCatalog{SessionKey: "codex:" + id, Revision: "fresh-runtime", Available: true, ContextScope: "loadedSnapshot", ContextMessages: len(snapshot.Messages)}, ChainID: request.ChainID, Turns: []protocol.SideQuestionExchange{}}
	if request.Action == "send" {
		value.LastOperationID = request.OperationID
		value.Turns = append(value.Turns, protocol.SideQuestionExchange{OperationID: request.OperationID, Question: request.Question, State: "completed", Answer: "independent response", CreatedAt: time.Now().UnixMilli()})
	}
	if a.alter != nil {
		a.alter(&value)
	}
	return value, err
}

func persistentDispatchFixture(t *testing.T) (*Client, *persistentSideFixtureAgent) {
	c, base := sideFixture(t)
	a := &persistentSideFixtureAgent{sideQuestionFixture: base}
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	return c, a
}

func persistentSideCommand(action string) map[string]any {
	cmd := sideCommand("")
	cmd["type"], cmd["chainId"] = "session.side-question."+action, "11111111-2222-4333-8444-555555555551"
	if action == "send" || action == "delete" {
		cmd["operationId"] = "11111111-2222-4333-8444-555555555552"
	}
	if action == "send" {
		cmd["question"], cmd["expectedRevision"] = "question", "fresh-runtime"
	}
	return cmd
}

func TestPersistentSideQuestionDispatchUsesServerSnapshotAndResumesWithoutCache(t *testing.T) {
	c, a := persistentDispatchFixture(t)
	rememberSideFixture(c, []protocol.Event{{SessionKey: "codex:s", ID: "parent", Type: "message", Role: "user", Text: "server snapshot"}}, "")
	if _, err := c.Dispatch(context.Background(), persistentSideCommand("open")); err != nil {
		t.Fatal(err)
	}
	if len(a.received.Messages) != 1 || a.received.Messages[0].Text != "server snapshot" || a.owner == "" {
		t.Fatal("open lost server-owned snapshot")
	}
	owner := a.owner
	c.sideQuestions.mu.Lock()
	c.sideQuestions.histories = nil
	c.sideQuestions.mu.Unlock()
	for _, action := range []string{"open", "read", "send", "delete"} {
		if _, err := c.Dispatch(context.Background(), persistentSideCommand(action)); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		if len(a.received.Messages) != 0 || a.owner != owner || a.request.Action != action {
			t.Fatal("resume depended on cached parent or changed owner")
		}
	}
	if len(a.sent)+len(a.started)+len(a.opened) != 0 {
		t.Fatal("side conversation wrote to parent")
	}
	if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.PhoneBindingID = "new-pairing"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Dispatch(context.Background(), persistentSideCommand("read")); err != nil {
		t.Fatal(err)
	}
	if owner == a.owner {
		t.Fatal("new pairing inherited persistent owner")
	}
}

func TestPersistentSideQuestionDispatchRejectsForeignAndMalformedReplies(t *testing.T) {
	for _, variant := range []string{"chain", "scope", "duplicate", "state", "empty", "selection", "timestamp", "head", "cursor", "oversize"} {
		t.Run(variant, func(t *testing.T) {
			c, a := persistentDispatchFixture(t)
			a.alter = func(v *protocol.SideQuestionConversation) {
				switch variant {
				case "chain":
					v.ChainID = "11111111-2222-4333-8444-555555555553"
				case "scope":
					v.ContextScope = "nativeFull"
				case "duplicate":
					v.Turns = append(v.Turns, v.Turns[0])
				case "state":
					v.Turns[0].State = "invented"
				case "empty":
					v.Turns[0].Answer = ""
				case "selection":
					v.Turns[0].Selection = &protocol.ContextReference{Kind: "message", SessionKey: "claude:foreign"}
				case "timestamp":
					v.Turns[0].CreatedAt = -1
				case "head":
					v.LastOperationID = ""
				case "cursor":
					v.NextCursor = "-1"
				case "oversize":
					v.Turns[0].Answer = string(make([]byte, 17<<10))
				}
			}
			if _, err := c.Dispatch(context.Background(), persistentSideCommand("send")); err == nil {
				t.Fatal("malformed persistent reply accepted")
			}
		})
	}
}

func TestPersistentSideQuestionDispatchFreshGuardEveryAction(t *testing.T) {
	for _, action := range []string{"open", "read", "send", "delete"} {
		for _, phase := range []string{"before", "after"} {
			t.Run(action+"/"+phase, func(t *testing.T) {
				c, a := persistentDispatchFixture(t)
				change := func() {
					if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.CodexModel = "different"; return nil }); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "before" {
					a.before = change
				} else {
					a.after = change
				}
				if _, err := c.Dispatch(context.Background(), persistentSideCommand(action)); err == nil {
					t.Fatal("revoked source returned saved side history")
				}
			})
		}
	}
}
