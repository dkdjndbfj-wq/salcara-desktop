package hubclient

import (
	"context"
	"reflect"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func TestFeatureChangesInvalidateAllAgentScopesWithoutChangingMetadataRevision(t *testing.T) {
	var cache featureCatalogCache
	keys := []string{"codex\x00account-a/cwd-a/session-a", "codex\x00account-b/cwd-b/session-b", "claude\x00other", "codex-extra\x00other"}
	calls := 0
	load := func(context.Context) (protocol.AgentFeatures, error) {
		calls++
		return featureFixture("unchanged"), nil
	}
	first, err := cache.read(context.Background(), keys[0], false, load)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys[1:] {
		if _, err := cache.read(context.Background(), key, false, load); err != nil {
			t.Fatal(err)
		}
	}
	cache.invalidateAgent("codex")
	for i, key := range keys {
		before := calls
		value, err := cache.read(context.Background(), key, false, load)
		if err != nil || value.Revision != first.Revision || (calls > before) != (i < 2) {
			t.Fatalf("wrong invalidation scope or invented body revision: %q calls=%d before=%d value=%+v err=%v", key, calls, before, value, err)
		}
	}
}

func TestFeatureChangesFenceOldInFlightRead(t *testing.T) {
	var cache featureCatalogCache
	started, release, result := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := cache.read(context.Background(), "codex\x00scope", false, func(context.Context) (protocol.AgentFeatures, error) {
			close(started)
			<-release
			return featureFixture("removed"), nil
		})
		result <- err
	}()
	<-started
	cache.invalidateAgent("codex")
	fresh, err := cache.read(context.Background(), "codex\x00scope", false, func(context.Context) (protocol.AgentFeatures, error) { return featureFixture("new"), nil })
	if err != nil || fresh.Skills[0].Name != "new" {
		t.Fatal(fresh, err)
	}
	close(release)
	if err := <-result; err == nil {
		t.Fatal("retired native read was accepted")
	}
	value, err := cache.read(context.Background(), "codex\x00scope", false, func(context.Context) (protocol.AgentFeatures, error) {
		t.Error("old native read removed the new cache entry")
		return protocol.AgentFeatures{}, nil
	})
	if err != nil || value.Skills[0].Name != "new" {
		t.Fatal(value, err)
	}
}

func TestFeatureChangesPushInvalidatesWhileOfflineAndRejectsForeignSignals(t *testing.T) {
	c, a, cwd := turnFeatureFixture(t)
	if err := c.o.Store.Update(func(cfg *config.Config) error {
		cfg.PhoneBindingID, cfg.PhoneHash = "fixture-unpaired", ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cmd := map[string]any{"type": "features.list", "tool": "hermes", "cwd": cwd, "controlSurface": "cli"}
	first, err := c.Dispatch(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range []protocol.Event{
		{Type: "features.changed", Tool: "other"},
		{Type: "features.changed", Tool: "hermes", SessionKey: "hermes:s"},
		{Type: "features.changed", Tool: "hermes", Session: &protocol.SessionInfo{}},
	} {
		c.Push(ev)
	}
	if _, err := c.Dispatch(context.Background(), cmd); err != nil || len(a.featuresCalls) != 1 {
		t.Fatalf("foreign signal invalidated catalog: %v %v", a.featuresCalls, err)
	}
	c.Push(protocol.Event{Type: "features.changed", Tool: "hermes", Text: "ignored"})
	second, err := c.Dispatch(context.Background(), cmd)
	if err != nil || len(a.featuresCalls) != 2 || !reflect.DeepEqual(first, second) || len(c.queue) != 0 {
		t.Fatalf("offline change did not refresh local cache: calls=%v queue=%v error=%v", a.featuresCalls, c.queue, err)
	}
	a.descriptor.Capabilities.Features = false
	if _, valid := c.invalidateAgentFeatures(protocol.Event{Type: "features.changed", Tool: "hermes"}); valid {
		t.Fatal("adapter without features invalidated catalog")
	}
	a.descriptor.Capabilities.Features = true
	a.descriptor.ControlSurface = "desktop"
	if _, valid := c.invalidateAgentFeatures(protocol.Event{Type: "features.changed", Tool: "hermes"}); valid {
		t.Fatal("native surface invalidated CLI catalog")
	}
}

func TestFeatureChangesQueueCoalescesOnlyUnsentSameAgentControl(t *testing.T) {
	old := protocol.Event{Type: "features.changed", Tool: "codex", TS: 1}
	other := protocol.Event{Type: "features.changed", Tool: "claude", TS: 2}
	message := protocol.Event{Type: "message", Tool: "codex", SessionKey: "codex:s", Text: "preserved"}
	c := &Client{queue: []protocol.Event{old, other, message}, inFlight: 1}
	for _, ev := range c.queue {
		c.queueBytes += eventWeight(ev)
	}
	newer := protocol.Event{Type: "features.changed", Tool: "codex", TS: 99999}
	if c.coalesceFeatureChangeLocked(newer) {
		t.Fatal("in-flight invalidation absorbed a later change")
	}
	c.queue = append(c.queue, newer)
	c.queueBytes += eventWeight(newer)
	newer.TS = 123456789
	if !c.coalesceFeatureChangeLocked(newer) || c.coalesceFeatureChangeLocked(message) || len(c.queue) != 4 || !reflect.DeepEqual(c.queue[0], old) || !reflect.DeepEqual(c.queue[1], other) || !reflect.DeepEqual(c.queue[2], message) || !reflect.DeepEqual(c.queue[3], newer) {
		t.Fatal("coalescing changed an unrelated event or frozen prefix")
	}
	weight := 0
	for _, ev := range c.queue {
		weight += eventWeight(ev)
	}
	if c.queueBytes != weight {
		t.Fatalf("queue weight drifted: %d != %d", c.queueBytes, weight)
	}
}
