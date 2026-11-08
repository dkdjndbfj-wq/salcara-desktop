package hubclient

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"salcara/bridge/internal/protocol"
)

const featureSyncPageSize = 200
const featureSyncPageBytes = 256 << 10
const featureSyncTTL = 2 * time.Minute
const featureHistoryTTL = 10 * time.Minute

var errFeatureCursor = errors.New("feature_cursor_expired: 技能目录分页已过期，请重新同步")

type featureSyncRequest struct {
	enabled  bool
	cursor   string
	pageSize int
}
type featureCatalogSnapshot struct {
	value protocol.AgentFeatures
	size  int
	at    time.Time
}
type featureCatalogTransfer struct {
	scope   string
	page    protocol.FeatureCatalogPage
	skills  []protocol.Skill
	removed []protocol.SkillReference
	size    int
	expires time.Time
}

func featureSkillKey(skill protocol.Skill) string { return skill.Name + "\x00" + skill.Path }
func featureInt(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case float64:
		if v >= 0 && v <= 100000 && v == float64(int(v)) {
			return int(v), true
		}
	case json.Number:
		n, err := strconv.Atoi(string(v))
		return n, err == nil
	}
	return 0, false
}
func parseFeatureSyncRequest(cmd map[string]any) (featureSyncRequest, error) {
	r := featureSyncRequest{pageSize: featureSyncPageSize}
	bad := errors.New("agent_feature_invalid: 技能分页参数无效")
	if raw, present := cmd["syncVersion"]; present {
		n, ok := featureInt(raw)
		if !ok || n != 1 {
			return r, bad
		}
		r.enabled = true
	}
	if raw, present := cmd["pageSize"]; present {
		n, ok := featureInt(raw)
		if !r.enabled || !ok || n < 1 || n > featureSyncPageSize {
			return r, bad
		}
		r.pageSize = n
	}
	if raw, present := cmd["cursor"]; present {
		value, ok := raw.(string)
		if !r.enabled || !ok || len(value) > 64 || value == "" {
			return r, bad
		}
		parts := strings.Split(value, ":")
		if len(parts) != 2 || len(parts[0]) != 32 {
			return r, bad
		}
		if _, err := hex.DecodeString(parts[0]); err != nil {
			return r, bad
		}
		n, err := strconv.Atoi(parts[1])
		if err != nil || n <= 0 || n > 2*protocol.MaxFeatureSkills || strconv.Itoa(n) != parts[1] {
			return r, bad
		}
		if _, present := cmd["ifRevision"]; present {
			return r, bad
		}
		if _, present := cmd["force"]; present {
			return r, bad
		}
		r.cursor = value
	}
	return r, nil
}

// History survives a native change notification so only added/changed/deleted
// metadata crosses the wire. Credential/binding clear discards history too.
func (cache *featureCatalogCache) rememberLocked(scope string, value protocol.AgentFeatures, size int) {
	if cache.history == nil {
		cache.history = make(map[string][]featureCatalogSnapshot)
	}
	now := time.Now()
	items := []featureCatalogSnapshot{{value: cloneFeatureCatalog(value), size: size, at: now}}
	for _, previous := range cache.history[scope] {
		if previous.value.Revision != value.Revision && now.Sub(previous.at) < featureHistoryTTL && len(items) < 3 {
			items = append(items, previous)
		}
	}
	cache.history[scope] = items
	total := 0
	for key, snapshots := range cache.history {
		valid := make([]featureCatalogSnapshot, 0, len(snapshots))
		for _, snapshot := range snapshots {
			if now.Sub(snapshot.at) < featureHistoryTTL {
				valid = append(valid, snapshot)
				total += snapshot.size
			}
		}
		if len(valid) == 0 {
			delete(cache.history, key)
		} else {
			cache.history[key] = valid
		}
	}
	for total > featureCatalogBytes || len(cache.history) > featureCatalogLimit {
		var oldestKey string
		var oldest time.Time
		for key, snapshots := range cache.history {
			if key != scope && (oldestKey == "" || snapshots[0].at.Before(oldest)) {
				oldestKey, oldest = key, snapshots[0].at
			}
		}
		if oldestKey == "" {
			break
		}
		for _, snapshot := range cache.history[oldestKey] {
			total -= snapshot.size
		}
		delete(cache.history, oldestKey)
	}
}

func (cache *featureCatalogCache) beginSync(scope string, value protocol.AgentFeatures, base string, pageSize int) (any, error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	current := cache.entries[scope]
	if current == nil || current.done != nil || current.value.Revision != value.Revision {
		return nil, errFeatureCursor
	}
	if base != "" && base == value.Revision {
		return struct {
			Revision    string `json:"revision"`
			NotModified bool   `json:"notModified"`
		}{value.Revision, true}, nil
	}
	t := &featureCatalogTransfer{scope: scope, expires: time.Now().Add(featureSyncTTL), skills: append([]protocol.Skill{}, value.Skills...), removed: []protocol.SkillReference{},
		page: protocol.FeatureCatalogPage{SyncVersion: 1, Kind: "full", Revision: value.Revision, CollaborationModes: append([]string{}, value.CollaborationModes...), SkillsSupported: value.SkillsSupported, TotalSkills: len(value.Skills)}}
	for _, previous := range cache.history[scope] {
		if base == "" || previous.value.Revision != base || time.Since(previous.at) >= featureHistoryTTL {
			continue
		}
		t.page.Kind, t.page.BaseRevision = "delta", base
		old := make(map[string]protocol.Skill, len(previous.value.Skills))
		for _, skill := range previous.value.Skills {
			old[featureSkillKey(skill)] = skill
		}
		t.skills = []protocol.Skill{}
		for _, skill := range value.Skills {
			if prior, ok := old[featureSkillKey(skill)]; !ok || prior != skill {
				t.skills = append(t.skills, skill)
			}
			delete(old, featureSkillKey(skill))
		}
		// Previous snapshots are canonical, preserving stable deletion order.
		for _, skill := range previous.value.Skills {
			if _, exists := old[featureSkillKey(skill)]; exists {
				t.removed = append(t.removed, protocol.SkillReference{Name: skill.Name, Path: skill.Path})
			}
		}
		break
	}
	t.page.TotalChanges = len(t.skills) + len(t.removed)
	encoded, err := json.Marshal(t.skills)
	if err != nil {
		return nil, err
	}
	t.size = len(encoded)
	encoded, err = json.Marshal(t.removed)
	if err != nil {
		return nil, err
	}
	t.size += len(encoded)
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	id := hex.EncodeToString(random[:])
	page, err := featureTransferPage(id, t, 0, pageSize)
	if err != nil {
		return nil, err
	}
	if page.NextCursor == "" {
		return page, nil
	}
	if cache.transfers == nil {
		cache.transfers = make(map[string]*featureCatalogTransfer)
	}
	cache.pruneTransfersLocked()
	total := t.size
	for _, old := range cache.transfers {
		total += old.size
	}
	for len(cache.transfers) >= 32 || total > featureCatalogBytes {
		var oldestKey string
		var oldest time.Time
		for key, old := range cache.transfers {
			if oldestKey == "" || old.expires.Before(oldest) {
				oldestKey, oldest = key, old.expires
			}
		}
		if oldestKey == "" {
			return nil, errors.New("agent_features_unavailable: 技能分页快照过大")
		}
		total -= cache.transfers[oldestKey].size
		delete(cache.transfers, oldestKey)
	}
	cache.transfers[id] = t
	return page, nil
}

func (cache *featureCatalogCache) pruneTransfersLocked() {
	for id, t := range cache.transfers {
		if !time.Now().Before(t.expires) {
			delete(cache.transfers, id)
		}
	}
}
func (cache *featureCatalogCache) continueSync(scope string, request featureSyncRequest) (protocol.FeatureCatalogPage, error) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.pruneTransfersLocked()
	parts := strings.Split(request.cursor, ":")
	if len(parts) != 2 {
		return protocol.FeatureCatalogPage{}, errFeatureCursor
	}
	t := cache.transfers[parts[0]]
	offset, err := strconv.Atoi(parts[1])
	if err != nil || t == nil || t.scope != scope || offset <= 0 || offset >= t.page.TotalChanges {
		return protocol.FeatureCatalogPage{}, errFeatureCursor
	}
	return featureTransferPage(parts[0], t, offset, request.pageSize)
}

func featureTransferPage(id string, t *featureCatalogTransfer, offset, pageSize int) (protocol.FeatureCatalogPage, error) {
	page := t.page
	page.Offset = offset
	page.CollaborationModes = append([]string{}, t.page.CollaborationModes...)
	page.Skills, page.RemovedSkills = []protocol.Skill{}, []protocol.SkillReference{}
	bytes, end := 1024, offset
	for end < t.page.TotalChanges && end-offset < pageSize {
		var item any
		if end < len(t.skills) {
			item = t.skills[end]
		} else {
			item = t.removed[end-len(t.skills)]
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			return page, err
		}
		if bytes+len(encoded) > featureSyncPageBytes {
			if end == offset {
				return page, errors.New("agent_features_unavailable: 单项技能元数据超过分页上限")
			}
			break
		}
		if end < len(t.skills) {
			page.Skills = append(page.Skills, t.skills[end])
		} else {
			page.RemovedSkills = append(page.RemovedSkills, t.removed[end-len(t.skills)])
		}
		bytes += len(encoded) + 1
		end++
	}
	if end < t.page.TotalChanges {
		page.NextCursor = id + ":" + strconv.Itoa(end)
	}
	return page, nil
}
