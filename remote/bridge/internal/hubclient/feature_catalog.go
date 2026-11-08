package hubclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

const featureCatalogTTL = 30 * time.Second
const featureCatalogLimit = 64
const featureCatalogBytes = 64 << 20

type featureCatalogEntry struct {
	size    int
	value   protocol.AgentFeatures
	expires time.Time
	done    chan struct{}
	err     error
}

// This cache is only a UI directory. Adapters still validate the requested
// skills against their native catalog immediately before submitting a turn.
type featureCatalogCache struct {
	mu        sync.Mutex
	entries   map[string]*featureCatalogEntry
	history   map[string][]featureCatalogSnapshot
	transfers map[string]*featureCatalogTransfer
}

func (cache *featureCatalogCache) clear() {
	cache.mu.Lock()
	cache.entries = nil
	cache.history = nil
	cache.transfers = nil
	cache.mu.Unlock()
}

func (cache *featureCatalogCache) invalidateAgent(agentID string) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for key := range cache.entries {
		if strings.HasPrefix(key, agentID+"\x00") {
			// Deletion also fences a load that was admitted before the change.
			delete(cache.entries, key)
		}
	}
	for key, transfer := range cache.transfers {
		if strings.HasPrefix(transfer.scope, agentID+"\x00") {
			delete(cache.transfers, key)
		}
	}
}

func cloneFeatureCatalog(value protocol.AgentFeatures) protocol.AgentFeatures {
	value.CollaborationModes = append([]string{}, value.CollaborationModes...)
	value.Skills = append([]protocol.Skill{}, value.Skills...)
	return value
}

func (cache *featureCatalogCache) read(ctx context.Context, key string, force bool, load func(context.Context) (protocol.AgentFeatures, error)) (protocol.AgentFeatures, error) {
	if err := ctx.Err(); err != nil {
		return protocol.AgentFeatures{}, err
	}
	cache.mu.Lock()
	if cache.entries == nil {
		cache.entries = make(map[string]*featureCatalogEntry)
	}
	if previous := cache.entries[key]; previous != nil && !force {
		if previous.done != nil {
			done := previous.done
			cache.mu.Unlock()
			select {
			case <-ctx.Done():
				return protocol.AgentFeatures{}, ctx.Err()
			case <-done:
				cache.mu.Lock()
				value, err := cloneFeatureCatalog(previous.value), previous.err
				if err == nil && cache.entries[key] != previous {
					err = errors.New("技能目录或 API 连接已更新，请重新读取技能")
					value = protocol.AgentFeatures{}
				}
				cache.mu.Unlock()
				return value, err
			}
		}
		if time.Now().Before(previous.expires) {
			value := cloneFeatureCatalog(previous.value)
			cache.mu.Unlock()
			return value, nil
		}
	}
	entry := &featureCatalogEntry{done: make(chan struct{})}
	if _, exists := cache.entries[key]; !exists && len(cache.entries) >= featureCatalogLimit {
		var oldestKey string
		var oldest time.Time
		for k, e := range cache.entries {
			if oldestKey == "" || e.expires.Before(oldest) {
				oldestKey, oldest = k, e.expires
			}
		}
		delete(cache.entries, oldestKey)
	}
	cache.entries[key] = entry
	cache.mu.Unlock()

	size := 0
	value, err := load(ctx)
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		value = cloneFeatureCatalog(value)
		sort.Strings(value.CollaborationModes)
		sort.Slice(value.Skills, func(i, j int) bool { return featureSkillKey(value.Skills[i]) < featureSkillKey(value.Skills[j]) })
		for i := 1; i < len(value.Skills); i++ {
			if featureSkillKey(value.Skills[i-1]) == featureSkillKey(value.Skills[i]) {
				err = errors.New("agent_features_unavailable: 原生技能目录包含重复身份")
				break
			}
		}
		value.Revision = ""
		var encoded []byte
		if err == nil {
			encoded, err = json.Marshal(value)
		}
		if err == nil && (len(encoded) > protocol.MaxFeatureCatalogBytes || len(value.Skills) > protocol.MaxFeatureSkills) {
			err = errors.New("agent_features_unavailable: 原生技能目录过大")
		}
		if err == nil {
			sum := sha256.Sum256(encoded)
			value.Revision = hex.EncodeToString(sum[:])
			size = len(encoded)
		}
	}
	cache.mu.Lock()
	if cache.entries[key] != entry {
		err = errors.New("技能目录或 API 连接已更新，请重新读取技能")
	}
	if err != nil {
		if cache.entries[key] == entry {
			delete(cache.entries, key)
		}
	} else {
		entry.value = value
		entry.size = size
		entry.expires = time.Now().Add(featureCatalogTTL)
		cache.rememberLocked(key, value, size)
		total := 0
		for _, old := range cache.entries {
			total += old.size
		}
		for oldKey, old := range cache.entries {
			if total <= featureCatalogBytes {
				break
			}
			if oldKey != key {
				delete(cache.entries, oldKey)
				total -= old.size
			}
		}
	}
	entry.err = err
	close(entry.done)
	entry.done = nil
	cache.mu.Unlock()
	return cloneFeatureCatalog(entry.value), err
}

// Credentials are used only to compute a private cache key. The public
// revision hashes display metadata, never skill bodies or account/credential
// data. A body-only edit can invalidate the cache without changing revision.
func featureCatalogScope(ctx context.Context, cfg config.Config, a agents.Agent, accountID, cwd, session string) string {
	descriptor := agents.Describe(a)
	family := descriptor.APIFamily
	account, _, _ := managedModelAccount(cfg, family)
	vault, _ := cfg.LocalAccount(account.ID)
	snapshot := struct {
		Identity, PhoneBinding, PhoneHash, Agent, Account, Cwd, Session string
		AccountFingerprint, VaultFingerprint, Route                     string
		Local                                                           bool
		Settings                                                        agents.Settings
		Applied                                                         config.AppliedAPI
		Remote                                                          config.RemoteAPI
	}{config.RemoteIdentity(cfg), cfg.PhoneBindingID, cfg.PhoneHash, a.ID(), accountID, cwd, session,
		config.AccountFingerprint(account), config.AccountFingerprint(vault), cfg.RemoteGatewayCredential("remote-" + family),
		isLocalExecution(ctx), agents.SettingsFromConfig(cfg), cfg.ToolAPIApplied[family], cfg.RemoteAPI[family]}
	if isLocalExecution(ctx) && IsDirectExecution(ctx) {
		snapshot.Identity, _, _ = DirectExecutionValidator(ctx)
		snapshot.PhoneBinding, snapshot.PhoneHash = cfg.DirectPhoneID, cfg.DirectPhoneHash
		snapshot.Settings = agents.DirectSettingsWithGateway(cfg, 47831)
		snapshot.Remote = cfg.DirectAPI[family]
		if snapshot.Remote.AccountID != "" {
			snapshot.Route = cfg.DirectGatewayCredential(config.DirectGatewayTarget(family))
			snapshot.Applied = config.AppliedAPI{}
		} else {
			snapshot.Route = cfg.LocalGatewayCredential(family)
		}
	}
	data, _ := json.Marshal(snapshot)
	sum := sha256.Sum256(data)
	return a.ID() + "\x00" + hex.EncodeToString(sum[:])
}
