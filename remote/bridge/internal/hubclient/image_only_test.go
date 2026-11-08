package hubclient

import (
	"context"
	"strings"
	"testing"
)

func TestImageOnlyMessagesReachCapableWorkerWithEmptyText(t *testing.T) {
	for _, family := range []string{"codex", "claude"} {
		for _, typ := range []string{"session.start", "session.send"} {
			t.Run(family+"/"+typ, func(t *testing.T) {
				c, worker, cwd := effortClient(t)
				worker.id = family
				id := prepareCapabilityImage(t)
				_, err := c.Dispatch(context.Background(), map[string]any{"type": typ, "tool": family, "cwd": cwd, "prompt": " \n ", "text": "", "sessionKey": family + ":1", "attachments": []any{id}})
				if err != nil {
					t.Fatal(err)
				}
				if typ == "session.start" {
					if len(worker.startOptions) != 1 || len(worker.startOptions[0].Images) != 1 || len(worker.started) != 1 || worker.started[0] != cwd+"||ask" {
						t.Fatalf("image-only start lost its original content: %+v", worker.started)
					}
				} else if len(worker.sendOptions) != 1 || len(worker.sendOptions[0].Images) != 1 || len(worker.sent) != 1 || worker.sent[0] != "1|" {
					t.Fatalf("image-only send lost its original content: %+v", worker.sent)
				}
			})
		}
	}
}

func TestImageOnlyMessagesStillValidateContentAndAttachments(t *testing.T) {
	for _, typ := range []string{"session.start", "session.send"} {
		for _, invalid := range []string{"empty", "missing-image", "invalid-text"} {
			t.Run(typ+"/"+invalid, func(t *testing.T) {
				c, worker, cwd := effortClient(t)
				cmd := map[string]any{"type": typ, "tool": "codex", "cwd": cwd, "prompt": "", "text": "", "sessionKey": "codex:1"}
				if invalid != "empty" {
					id := prepareCapabilityImage(t)
					if invalid == "missing-image" {
						id = "att_" + strings.Repeat("cd", 16)
					}
					cmd["attachments"] = []any{id}
				}
				if invalid == "invalid-text" {
					cmd["prompt"], cmd["text"] = 42, 42
				}
				if _, err := c.Dispatch(context.Background(), cmd); err == nil {
					t.Fatal("invalid content was accepted")
				}
				if len(worker.started)+len(worker.sent)+len(worker.startOptions)+len(worker.sendOptions) != 0 {
					t.Fatal("invalid content reached worker")
				}
			})
		}
	}
}
