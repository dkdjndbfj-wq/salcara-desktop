package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func featureFixture(name string) protocol.AgentFeatures {
	return protocol.AgentFeatures{CollaborationModes: []string{"default", "plan"}, SkillsSupported: true, Skills: []protocol.Skill{{Name: name, Description: "description"}}}
}

func TestFeatureCatalogTTLRevisionAndDetachedValues(t *testing.T) {
	var cache featureCatalogCache
	calls := 0
	load := func(context.Context) (protocol.AgentFeatures, error) { calls++; return featureFixture("review"), nil }
	first, err := cache.read(context.Background(), "scope", false, load)
	if err != nil || len(first.Revision) != 64 {
		t.Fatal(first, err)
	}
	first.Skills[0].Name = "mutated"
	second, err := cache.read(context.Background(), "scope", false, load)
	if err != nil || calls != 1 || second.Skills[0].Name != "review" {
		t.Fatal(second, calls, err)
	}
	cache.mu.Lock()
	cache.entries["scope"].expires = time.Now().Add(-time.Second)
	cache.mu.Unlock()
	third, err := cache.read(context.Background(), "scope", false, load)
	if err != nil || calls != 2 || third.Revision != second.Revision {
		t.Fatal(third, calls, err)
	}
	changed, err := cache.read(context.Background(), "scope", true, func(context.Context) (protocol.AgentFeatures, error) {
		value := featureFixture("review")
		value.Skills[0].Description = "edited"
		return value, nil
	})
	if err != nil || changed.Revision == third.Revision {
		t.Fatal(changed, err)
	}
}

func TestFeatureCatalogConcurrentReadsCoalesce(t *testing.T) {
	var cache featureCatalogCache
	var calls atomic.Int32
	var ready, finished sync.WaitGroup
	gate := make(chan struct{})
	load := func(context.Context) (protocol.AgentFeatures, error) {
		calls.Add(1)
		<-gate
		return featureFixture("review"), nil
	}
	for range 16 {
		ready.Add(1)
		finished.Add(1)
		go func() {
			defer finished.Done()
			ready.Done()
			value, err := cache.read(context.Background(), "scope", false, load)
			if err != nil || len(value.Skills) != 1 {
				t.Errorf("value=%+v err=%v", value, err)
			}
		}()
	}
	ready.Wait()
	close(gate)
	finished.Wait()
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
}

func TestFeatureCatalogRefreshAndClearFenceInFlightReads(t *testing.T) {
	for _, operation := range []string{"force", "clear"} {
		t.Run(operation, func(t *testing.T) {
			var cache featureCatalogCache
			started, release, failed := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			go func() {
				_, err := cache.read(context.Background(), "scope", false, func(context.Context) (protocol.AgentFeatures, error) {
					close(started)
					<-release
					return featureFixture("old"), nil
				})
				failed <- err
			}()
			<-started
			if operation == "clear" {
				cache.clear()
			} else {
				value, err := cache.read(context.Background(), "scope", true, func(context.Context) (protocol.AgentFeatures, error) { return featureFixture("new"), nil })
				if err != nil || value.Skills[0].Name != "new" {
					t.Fatal(value, err)
				}
			}
			close(release)
			if err := <-failed; err == nil {
				t.Fatal("retired request republished")
			}
			if operation == "force" {
				value, err := cache.read(context.Background(), "scope", false, func(context.Context) (protocol.AgentFeatures, error) {
					t.Error("cache lost")
					return protocol.AgentFeatures{}, nil
				})
				if err != nil || value.Skills[0].Name != "new" {
					t.Fatal(value, err)
				}
			}
		})
	}
}

func TestFeatureCatalogFailureBoundsAndCancellation(t *testing.T) {
	var cache featureCatalogCache
	load := func(context.Context) (protocol.AgentFeatures, error) { return featureFixture("review"), nil }
	for i := range featureCatalogLimit + 1 {
		if _, err := cache.read(context.Background(), fmt.Sprint(i), false, load); err != nil {
			t.Fatal(err)
		}
	}
	if len(cache.entries) != featureCatalogLimit {
		t.Fatal(len(cache.entries))
	}
	_, err := cache.read(context.Background(), "error", true, func(context.Context) (protocol.AgentFeatures, error) {
		return protocol.AgentFeatures{}, errors.New("account deleted")
	})
	if err == nil || cache.entries["error"] != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.read(ctx, "cancelled", false, load); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if cache.entries["cancelled"] != nil {
		t.Fatal("cancelled catalog persisted")
	}
}

func TestFeatureCatalogScopeSeparatesPairAccountAgentProjectSessionAndSource(t *testing.T) {
	_, a, cwd := turnFeatureFixture(t)
	base := config.Config{DeviceID: "pc", PhoneBindingID: "pair", PhoneHash: "phone", DeviceSecret: "secret"}
	key := featureCatalogScope(context.Background(), base, a, "account-a", cwd, "session-a")
	for _, mutate := range []func(*config.Config){
		func(c *config.Config) { c.DeviceID = "other" },
		func(c *config.Config) { c.PhoneBindingID = "other" },
		func(c *config.Config) { c.PhoneHash = "other" },
		func(c *config.Config) { c.DeviceSecret = "other" },
		func(c *config.Config) { c.ClaudeKey = "other" },
		func(c *config.Config) { c.LocalToolPaths = map[string]string{"codex": "other"} },
	} {
		changed := base.Clone()
		mutate(&changed)
		if featureCatalogScope(context.Background(), changed, a, "account-a", cwd, "session-a") == key {
			t.Fatal("configuration scopes merged")
		}
	}
	for _, other := range []string{
		featureCatalogScope(context.Background(), base, a, "account-b", cwd, "session-a"),
		featureCatalogScope(context.Background(), base, a, "account-a", cwd+"other", "session-a"),
		featureCatalogScope(context.Background(), base, a, "account-a", cwd, "session-b"),
		featureCatalogScope(WithLocalExecution(context.Background()), base, a, "account-a", cwd, "session-a"),
	} {
		if other == key {
			t.Fatal("request scopes merged")
		}
	}
	_, other, _ := aggregationFixture(t, "other")
	if featureCatalogScope(context.Background(), base, other, "account-a", cwd, "session-a") == key {
		t.Fatal("agent namespaces merged")
	}
}

func TestFeatureCatalogDispatchConditionalForceAndPermissions(t *testing.T) {
	c, a, cwd := turnFeatureFixture(t)
	cmd := map[string]any{"type": "features.list", "tool": "hermes", "cwd": cwd, "controlSurface": "cli"}
	first, err := c.Dispatch(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	cmd["ifRevision"] = first.(protocol.AgentFeatures).Revision
	second, err := c.Dispatch(context.Background(), cmd)
	encoded, _ := json.Marshal(second)
	var conditional map[string]any
	_ = json.Unmarshal(encoded, &conditional)
	if err != nil || conditional["notModified"] != true || conditional["skills"] != nil || len(a.featuresCalls) != 1 {
		t.Fatal(second, len(a.featuresCalls), err)
	}
	cmd["force"] = true
	forced, err := c.Dispatch(context.Background(), cmd)
	if err != nil || !forced.(protocol.AgentFeatures).SkillsSupported || len(a.featuresCalls) != 2 {
		t.Fatal(forced, err)
	}
	delete(cmd, "force")
	a.descriptor.Capabilities.Features = false
	if _, err := c.Dispatch(context.Background(), cmd); err == nil {
		t.Fatal("cached catalog bypassed capability check")
	}
	a.descriptor.Capabilities.Features = true
	cmd["cwd"] = t.TempDir()
	if _, err := c.Dispatch(context.Background(), cmd); err == nil {
		t.Fatal("cached catalog bypassed cwd check")
	}
	if len(a.featuresCalls) != 2 {
		t.Fatal(a.featuresCalls)
	}
}
