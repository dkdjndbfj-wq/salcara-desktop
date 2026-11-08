package hubclient

import (
	"testing"

	"salcara/bridge/internal/config"
)

func TestDurableReceiptUsesPhoneAccountHandleSource(t *testing.T) {
	c, store := newTestClient(t, "https://hub.invalid", "key", nil)
	cfg := store.Get()
	cfg.LocalAccounts = []config.LocalAccount{
		{ID: "computer-api", Kind: "codex", BaseURL: "https://computer.invalid", Key: "synthetic-computer-key", Model: "model"},
		{ID: "phone-api", Kind: "codex", BaseURL: "https://phone.invalid", Key: "synthetic-phone-key", Model: "model"},
	}
	cfg.ToolAPISelections = map[string]string{"codex": "computer-api"}
	cfg.RemoteAPI = map[string]config.RemoteAPI{"codex": {AccountID: "phone-api", Model: "model"}}
	cmd := map[string]any{"type": "session.send", "sessionKey": "codex:original", "accountId": APIHandle(cfg, "phone-api"), "operationId": config.NewUUID(), "text": "synthetic request"}
	ctx := durableTestContext(c)
	finish, replay, _, err := c.beginDurableExecution(ctx, cfg, cmd)
	if err != nil || replay || finish == nil {
		t.Fatalf("first admission failed: replay=%v err=%v", replay, err)
	}
	if err := finish(map[string]any{"via": "background"}, nil); err != nil {
		t.Fatal(err)
	}
	// The computer's independent key is not the phone command's source.
	cfg.LocalAccounts[0].Key = "synthetic-unrelated-rotation"
	if _, replay, _, err := c.beginDurableExecution(ctx, cfg, cmd); err != nil || !replay {
		t.Fatalf("unrelated local API invalidated phone receipt: %v", err)
	}
	cfg.LocalAccounts[1].Key = "synthetic-phone-rotation"
	if _, _, _, err := c.beginDurableExecution(ctx, cfg, cmd); err == nil {
		t.Fatal("rotated phone source replayed the old credential's receipt")
	}
}
