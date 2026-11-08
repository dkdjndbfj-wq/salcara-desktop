package hubclient

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

func syncCatalog(t *testing.T, cache *featureCatalogCache, scope string, skills []protocol.Skill) protocol.AgentFeatures {
	t.Helper()
	value, err := cache.read(context.Background(), scope, true, func(context.Context) (protocol.AgentFeatures, error) {
		return protocol.AgentFeatures{CollaborationModes: []string{"default", "plan"}, SkillsSupported: true, Skills: skills}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestFeatureSyncDeltaIncludesOnlyAddedModifiedAndRemoved(t *testing.T) {
	var cache featureCatalogCache
	old := syncCatalog(t, &cache, "codex\x00scope", []protocol.Skill{{Name: "unchanged", Description: "same"}, {Name: "changed", Path: "/a", Description: "old"}, {Name: "removed"}})
	cache.invalidateAgent("codex")
	newValue := syncCatalog(t, &cache, "codex\x00scope", []protocol.Skill{{Name: "unchanged", Description: "same"}, {Name: "changed", Path: "/a", Description: "new", BrandColor: "#112233"}, {Name: "added"}})
	raw, err := cache.beginSync("codex\x00scope", newValue, old.Revision, 1)
	if err != nil {
		t.Fatal(err)
	}
	page := raw.(protocol.FeatureCatalogPage)
	if page.Kind != "delta" || page.BaseRevision != old.Revision || page.Revision != newValue.Revision || page.TotalChanges != 3 || page.TotalSkills != 3 {
		t.Fatal(page)
	}
	var upserts []protocol.Skill
	var removals []protocol.SkillReference
	for {
		upserts = append(upserts, page.Skills...)
		removals = append(removals, page.RemovedSkills...)
		if page.NextCursor == "" {
			break
		}
		request := featureSyncRequest{cursor: page.NextCursor, pageSize: 1}
		next, err := cache.continueSync("codex\x00scope", request)
		if err != nil {
			t.Fatal(err)
		}
		retry, err := cache.continueSync("codex\x00scope", request)
		if err != nil || !reflect.DeepEqual(next, retry) {
			t.Fatal("cursor retry changed", err)
		}
		page = next
	}
	if len(upserts) != 2 || upserts[0].Name != "added" || upserts[1].Description != "new" || len(removals) != 1 || removals[0].Name != "removed" {
		t.Fatal(upserts, removals)
	}
}

func TestFeatureSyncLargeCatalogIsCompleteBoundedAndStableAcrossNativeReload(t *testing.T) {
	var cache featureCatalogCache
	skills := make([]protocol.Skill, 1205)
	for i := range skills {
		skills[i] = protocol.Skill{Name: fmt.Sprintf("skill-%04d", i), Description: strings.Repeat("x", 3000)}
	}
	value := syncCatalog(t, &cache, "scope", skills)
	raw, err := cache.beginSync("scope", value, "", 200)
	if err != nil {
		t.Fatal(err)
	}
	page := raw.(protocol.FeatureCatalogPage)
	seen := map[string]bool{}
	pages := 0
	for {
		encoded, _ := json.Marshal(page)
		if len(encoded) > featureSyncPageBytes {
			t.Fatal("page exceeds wire budget", len(encoded))
		}
		for _, skill := range page.Skills {
			if seen[skill.Name] {
				t.Fatal("duplicate", skill.Name)
			}
			seen[skill.Name] = true
		}
		pages++
		if page.NextCursor == "" {
			break
		}
		if pages == 1 {
			syncCatalog(t, &cache, "scope", []protocol.Skill{{Name: "new-revision"}})
		}
		page, err = cache.continueSync("scope", featureSyncRequest{cursor: page.NextCursor, pageSize: 200})
		if err != nil {
			t.Fatal(err)
		}
		if page.Revision != value.Revision {
			t.Fatal("mixed snapshot")
		}
	}
	if len(seen) != len(skills) || pages < 7 {
		t.Fatal(len(seen), pages)
	}
}

func TestFeatureSyncEmptyMetadataHistoryAndInvalidation(t *testing.T) {
	var cache featureCatalogCache
	old := syncCatalog(t, &cache, "codex\x00scope", []protocol.Skill{{Name: "gone"}, {Name: "also-gone"}})
	empty, err := cache.read(context.Background(), "codex\x00scope", true, func(context.Context) (protocol.AgentFeatures, error) {
		return protocol.AgentFeatures{CollaborationModes: []string{"plan"}, Skills: []protocol.Skill{}, SkillsSupported: false}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := cache.beginSync("codex\x00scope", empty, old.Revision, 1)
	if err != nil {
		t.Fatal(err)
	}
	page := raw.(protocol.FeatureCatalogPage)
	if page.Kind != "delta" || page.TotalSkills != 0 || page.SkillsSupported || len(page.Skills) != 0 || page.NextCursor == "" {
		t.Fatal(page)
	}
	if _, err = cache.continueSync("claude\x00scope", featureSyncRequest{cursor: page.NextCursor, pageSize: 1}); err == nil {
		t.Fatal("foreign scope cursor accepted")
	}
	cache.invalidateAgent("codex")
	if _, err = cache.continueSync("codex\x00scope", featureSyncRequest{cursor: page.NextCursor, pageSize: 1}); err == nil {
		t.Fatal("invalidated cursor accepted")
	}
	fresh := syncCatalog(t, &cache, "codex\x00scope", []protocol.Skill{})
	raw, err = cache.beginSync("codex\x00scope", fresh, strings.Repeat("f", 64), 200)
	if err != nil || raw.(protocol.FeatureCatalogPage).Kind != "full" {
		t.Fatal(raw, err)
	}
	cache.clear()
	if len(cache.history) != 0 || len(cache.transfers) != 0 {
		t.Fatal("clear retained scope state")
	}
}

func TestFeatureSyncCanonicalRevisionAndExpiredBaseline(t *testing.T) {
	var cache featureCatalogCache
	first := syncCatalog(t, &cache, "scope", []protocol.Skill{{Name: "b"}, {Name: "a"}})
	second := syncCatalog(t, &cache, "scope", []protocol.Skill{{Name: "a"}, {Name: "b"}})
	if first.Revision != second.Revision {
		t.Fatal("native enumeration order changed revision")
	}
	current := syncCatalog(t, &cache, "scope", []protocol.Skill{{Name: "c"}})
	for i := range cache.history["scope"] {
		cache.history["scope"][i].at = time.Now().Add(-featureHistoryTTL - time.Second)
	}
	raw, err := cache.beginSync("scope", current, first.Revision, 200)
	if err != nil || raw.(protocol.FeatureCatalogPage).Kind != "full" {
		t.Fatal(raw, err)
	}
}

func TestFeatureSyncRejectsDuplicateAndExcessCatalog(t *testing.T) {
	for _, count := range []int{2, protocol.MaxFeatureSkills + 1} {
		var cache featureCatalogCache
		_, err := cache.read(context.Background(), "scope", false, func(context.Context) (protocol.AgentFeatures, error) {
			skills := make([]protocol.Skill, count)
			for i := range skills {
				skills[i].Name = fmt.Sprint(i)
			}
			if count == 2 {
				skills[1] = skills[0]
			}
			return protocol.AgentFeatures{Skills: skills}, nil
		})
		if err == nil {
			t.Fatal("invalid catalog accepted", count)
		}
	}
}

func TestFeatureSyncDispatchNegotiationAndStrictFields(t *testing.T) {
	c, _, cwd := turnFeatureFixture(t)
	base := map[string]any{"type": "features.list", "tool": "hermes", "cwd": cwd, "controlSurface": "cli", "syncVersion": 1}
	raw, err := c.Dispatch(context.Background(), base)
	if err != nil || raw.(protocol.FeatureCatalogPage).SyncVersion != 1 {
		t.Fatal(raw, err)
	}
	for _, invalid := range []map[string]any{{"syncVersion": "1"}, {"syncVersion": 2}, {"pageSize": 0}, {"pageSize": 201}, {"pageSize": 1.5}, {"cursor": nil}, {"cursor": "fake"}, {"cursor": strings.Repeat("a", 32) + ":1", "force": false}} {
		cmd := map[string]any{}
		for key, value := range base {
			cmd[key] = value
		}
		for key, value := range invalid {
			cmd[key] = value
		}
		if _, err := c.Dispatch(context.Background(), cmd); err == nil {
			t.Fatal("accepted invalid sync params", invalid)
		}
	}
}
