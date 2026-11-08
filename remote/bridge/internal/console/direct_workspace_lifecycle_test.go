package console

import (
	"context"
	"fmt"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/hubclient"
	"salcara/bridge/internal/protocol"
)

type directWorkspaceLifecycleAgent struct {
	*sessionAgent
	starts int
	emit   func(string, string)
}

func (a *directWorkspaceLifecycleAgent) Start(_ context.Context, cwd, _, _, _ string) (string, error) {
	a.starts++
	if a.emit != nil {
		a.emit(a.ID()+fmt.Sprintf(":fixture-%d", a.starts), cwd)
	}
	return fmt.Sprintf("fixture-%d", a.starts), nil
}

func TestDirectTerminalEventReleasesWorkspaceWithoutHubUpload(t *testing.T) {
	for _, family := range []string{"codex", "claude"} {
		t.Run(family, func(t *testing.T) {
			s, _ := newTestServer(t)
			cwd := t.TempDir()
			if err := s.d.Store.Update(func(c *config.Config) error { c.Projects = []protocol.Project{{Path: cwd}}; return nil }); err != nil {
				t.Fatal(err)
			}
			hub := hubclient.New(hubclient.Options{Store: s.d.Store})
			s.d.Hub = hub
			a := &directWorkspaceLifecycleAgent{sessionAgent: &sessionAgent{kind: family}}
			hub.SetManager(consoleCatalogManager{agent: a})
			start := func() error {
				_, err := hub.Dispatch(context.Background(), map[string]any{"type": "session.start", "tool": family, "cwd": cwd, "prompt": "synthetic task"})
				return err
			}
			if err := start(); err != nil {
				t.Fatal(err)
			}
			key := family + ":fixture-1"
			s.rememberDirectSession(key)
			event := func(status string) protocol.Event {
				return protocol.Event{Type: "session.updated", SessionKey: key, Session: &protocol.SessionInfo{SessionKey: key, Cwd: cwd, Status: status}}
			}
			s.PublishHubEvent(event("waiting_approval"))
			if err := start(); protocol.ExecutionErrorCode(err) != "workspace_busy" {
				t.Fatalf("active workspace was not blocked: %v", err)
			}
			s.PublishHubEvent(event("idle"))
			if queued := hub.Status().Queued; queued != 0 {
				t.Fatal("private Direct event entered Hub queue")
			}
			if err := start(); err != nil {
				t.Fatalf("Direct completion did not release workspace: %v", err)
			}
			if a.starts != 2 {
				t.Fatal("unexpected native starts")
			}
		})
	}
}

func TestDirectInFlightCompletionWinsBeforeStartReply(t *testing.T) {
	s, _ := newTestServer(t)
	cwd := t.TempDir()
	if err := s.d.Store.Update(func(c *config.Config) error { c.Projects = []protocol.Project{{Path: cwd}}; return nil }); err != nil {
		t.Fatal(err)
	}
	hub := hubclient.New(hubclient.Options{Store: s.d.Store})
	s.d.Hub = hub
	a := &directWorkspaceLifecycleAgent{sessionAgent: &sessionAgent{kind: "claude"}}
	a.emit = func(key, cwd string) {
		for _, status := range []string{"running", "idle"} {
			s.PublishHubEvent(protocol.Event{Type: "session.updated", SessionKey: key, Session: &protocol.SessionInfo{SessionKey: key, Cwd: cwd, Status: status}})
		}
	}
	hub.SetManager(consoleCatalogManager{agent: a})
	for i := 0; i < 2; i++ {
		if err := s.beginDirectSessionStart(); err != nil {
			t.Fatal(err)
		}
		result, err := hub.Dispatch(context.Background(), map[string]any{"type": "session.start", "tool": "claude", "cwd": cwd, "prompt": "synthetic instant task"})
		if err != nil {
			t.Fatalf("completed in-flight Direct task left workspace busy: %v", err)
		}
		key := result.(map[string]any)["sessionKey"].(string)
		if err := s.endDirectSessionStart(key, false); err != nil {
			t.Fatal(err)
		}
	}
	if hub.Status().Queued != 0 {
		t.Fatal("in-flight Direct event entered Hub queue")
	}
	if a.starts != 2 {
		t.Fatal("second task was not admitted")
	}
}
