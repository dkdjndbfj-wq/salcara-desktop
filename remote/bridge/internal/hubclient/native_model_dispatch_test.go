package hubclient

import (
	"context"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/desktopcompanion"
)

func TestDesktopLiveRejectsModelWithoutSendingOrFallingBack(t *testing.T) {
	key := "codex:01a0ae56-e9f6-4933-9ad4-5e08cd7874e5"
	for _, model := range []string{"gpt-selected", "claude-opus-5-5", "vendor/custom-model", " ", "bad\nmodel"} {
		t.Run(model, func(t *testing.T) {
			c, worker, _ := effortClient(t)
			d := &routeDesktop{st: desktopcompanion.NativeConnection{
				Active: true, ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), SessionKeys: []string{key},
			}}
			c.o.Desktop = d
			result, err := c.Dispatch(context.Background(), map[string]any{
				"type": "session.send", "sessionKey": key, "text": "must keep the model", "model": model, "controlSurface": "desktop",
			})
			if err == nil || result != nil || !strings.Contains(err.Error(), "不支持手机指定模型") || !strings.Contains(err.Error(), "消息未发送") {
				t.Fatalf("result=%v err=%v", result, err)
			}
			if len(d.sent)+len(worker.sent)+len(worker.sendOptions) != 0 {
				t.Fatal("desktop live model was dropped or fell back to CLI")
			}
		})
	}
}

func TestDesktopLiveWithoutOverridesSendsWithDesktopSettings(t *testing.T) {
	key := "codex:01a0ae56-e9f6-4933-9ad4-5e08cd7874e5"
	c, worker, _ := effortClient(t)
	d := &routeDesktop{st: liveDesktop(key)}
	c.o.Desktop = d
	result, err := c.Dispatch(context.Background(), map[string]any{
		"type": "session.send", "sessionKey": key, "text": "use desktop settings", "model": "", "effort": "",
		"controlSurface": "desktop", "operationId": "11111111-2222-4333-8444-555555555555",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.(map[string]any)["via"] != "desktop" || len(d.sent) != 1 || d.sent[0] != key+"|use desktop settings|11111111-2222-4333-8444-555555555555" {
		t.Fatalf("result=%v native=%v", result, d.sent)
	}
	if len(worker.sent)+len(worker.sendOptions) != 0 {
		t.Fatal("native send also reached the background worker")
	}
}

func TestExplicitBackgroundFrozenModelRetryDoesNotSwitchExecutor(t *testing.T) {
	key := "codex:01a0ae56-e9f6-4933-9ad4-5e08cd7874e5"
	c, worker, _ := effortClient(t)
	d := &routeDesktop{}
	c.o.Desktop = d
	// The phone retries the frozen original command after delivery uncertainty.
	// Desktop Live may have become active since the original background send.
	frozen := map[string]any{
		"type": "session.send", "sessionKey": key, "text": "original text", "model": "gpt-frozen-model",
		"controlSurface": "cli", "operationId": "frozen-operation",
	}
	result, err := c.Dispatch(context.Background(), frozen)
	if err != nil || result.(map[string]any)["via"] != "background" || len(worker.sendOptions) != 1 || worker.sendOptions[0].Model != "gpt-frozen-model" {
		t.Fatalf("original result=%v err=%v options=%+v", result, err, worker.sendOptions)
	}
	d.st = desktopcompanion.NativeConnection{Active: true, ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), SessionKeys: []string{key}}
	for retry := 0; retry < 2; retry++ {
		result, err = c.Dispatch(context.Background(), frozen)
		if err != nil || result.(map[string]any)["via"] != "background" {
			t.Fatalf("retry=%d result=%v err=%v", retry, result, err)
		}
	}
	if len(d.sent) != 0 || len(worker.sent) != 3 || len(worker.sendOptions) != 3 {
		t.Fatalf("retry silently changed executor: native=%v background=%v options=%+v", d.sent, worker.sent, worker.sendOptions)
	}
	if frozen["model"] != "gpt-frozen-model" || frozen["operationId"] != "frozen-operation" || frozen["text"] != "original text" {
		t.Fatalf("frozen command mutated: %v", frozen)
	}
}
