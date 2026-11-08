package console

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"weak"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/localusage"
	"salcara/bridge/internal/relay"
)

// Token use recorded by Codex / Claude Code on this computer; fills in when a
// relay reports no daily or per-model statistics. Created on first use.
var (
	localUsageOnce sync.Once
	localUsage     *localusage.Scanner
)

func localUsageSource(r *http.Request, days int) (usageSource, bool) {
	localUsageOnce.Do(func() { localUsage = localusage.Default() })
	ctx, cancel := ctxTimeout(r, 20*time.Second)
	defer cancel()
	res, err := localUsage.Scan(ctx, days, time.Now())
	if err != nil || len(res.Tools) == 0 {
		return usageSource{}, false
	}
	raw, _ := json.Marshal(map[string]any{"daily_usage": res.Days, "model_stats": res.Models, "usage": map[string]any{"today": res.Today}, "local": true, "partial": res.Partial})
	var usage map[string]any
	if json.Unmarshal(raw, &usage) != nil {
		return usageSource{}, false
	}
	return usageSource{ID: "local", Name: "本机记录", Host: strings.Join(res.Tools, " / "), Kind: "local", OK: true, Usage: usage}, true
}

// usageSource is one place usage can be read from: the signed-in relay account
// or an API saved in the local vault. Keys never leave the Bridge.
type usageSource struct {
	ID              string                  `json:"id"`
	Name            string                  `json:"name"`
	Host            string                  `json:"host"`
	Kind            string                  `json:"kind"` // relay | api | local | official
	OK              bool                    `json:"ok"`
	Error           string                  `json:"error,omitempty"`
	Usage           map[string]any          `json:"usage,omitempty"`
	LocalUsage      *usageAccountLocalUsage `json:"localUsage,omitempty"`
	LocalUsageError string                  `json:"localUsageError,omitempty"`
}

type usageOverviewCache struct {
	mu       sync.Mutex
	key      string
	at       time.Time
	data     []usageSource
	sequence uint64
}

type usageOverviewTarget struct {
	src       usageSource
	root, key string
}

// Each console owns one bounded cache. Store's lock-bound source revision is
// the safety fence; OnChange only clears old storage. The weak callback does
// not keep a discarded console or its cache alive in Store.
func newUsageOverviewCache(store *config.Store) *usageOverviewCache {
	cache := &usageOverviewCache{}
	if store != nil {
		ref := weak.Make(cache)
		store.OnChange(func(old, next config.Config) {
			if old.UsageSourceRevision == next.UsageSourceRevision {
				return
			}
			if current := ref.Value(); current != nil {
				current.mu.Lock()
				current.key, current.at, current.data = "", time.Time{}, nil
				current.mu.Unlock()
			}
		})
	}
	return cache
}

func usageOverviewTargets(c config.Config) []usageOverviewTarget {
	var targets []usageOverviewTarget
	for _, target := range c.UsageSourceTargets() {
		targets = append(targets, usageOverviewTarget{src: usageSource{ID: target.ID, Name: target.Name, Host: target.Host, Kind: target.Kind}, root: target.Root, key: target.Key})
	}
	return targets
}

func (s *Server) handleUsageOverview(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 {
		days = 14
	}
	if days > 90 {
		days = 90
	}
	cache := s.usageOverview
	if s.d.Store == nil || cache == nil {
		s.writeUsageOverviewError(w, http.StatusServiceUnavailable, days, "", "用量来源暂不可用", time.Now())
		return
	}
	query, queryErr := url.ParseQuery(r.URL.RawQuery)
	selected := query.Get("source")
	if queryErr != nil || len(query["source"]) > 1 || selected != "" && !config.ValidAccountID(selected) {
		s.writeUsageOverviewError(w, http.StatusBadRequest, days, selected, "用量来源参数无效", time.Now())
		return
	}
	if selected != "" {
		s.handleSelectedUsageOverview(w, r, days, selected)
		return
	}
	force := r.URL.Query().Get("refresh") == "1"
	rangeEnd := time.Now().Format("2006-01-02")
	cache.mu.Lock()
	captured := s.d.Store.Get()
	targets := usageOverviewTargets(captured)
	fingerprint := captured.UsageSourceFingerprint()
	cacheKey := strconv.Itoa(days) + ":" + rangeEnd + ":" + captured.UsageSourceRevision + ":" + fingerprint
	if !force && cache.key == cacheKey && time.Since(cache.at) < 60*time.Second {
		data, at := cache.data, cache.at
		cache.mu.Unlock()
		writeJSON(w, usageOverviewResponse{Days: days, Sources: data, Options: usageOverviewOptions(s.usageOverviewSelections(captured)), RangeEnd: rangeEnd, UpdatedAt: at.UnixMilli()})
		return
	}
	cache.sequence++
	sequence := cache.sequence
	cache.mu.Unlock()

	ctx, cancel := ctxTimeout(r, 20*time.Second)
	defer cancel()
	out := make([]usageSource, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t usageOverviewTarget) {
			defer wg.Done()
			src := t.src
			u, err := relay.UsageDays(ctx, t.root, t.key, days)
			if err != nil {
				src.Error = err.Error()
			} else {
				src.OK, src.Usage = true, u
			}
			out[i] = src
		}(i, t)
	}
	wg.Wait()
	if local, ok := localUsageSource(r, days); ok {
		out = append(out, local)
	}

	now := time.Now()
	cache.mu.Lock()
	current := s.d.Store.Get()
	if current.UsageSourceRevision != captured.UsageSourceRevision || fingerprint != current.UsageSourceFingerprint() || cache.sequence != sequence || now.Format("2006-01-02") != rangeEnd {
		cache.mu.Unlock()
		s.writeUsageOverviewError(w, http.StatusConflict, days, "", "用量来源或日期已变更，或已有更新查询，请重新读取", now)
		return
	}
	cache.key, cache.at, cache.data = cacheKey, now, out
	cache.mu.Unlock()
	writeJSON(w, usageOverviewResponse{Days: days, Sources: out, Options: usageOverviewOptions(s.usageOverviewSelections(current)), RangeEnd: rangeEnd, UpdatedAt: now.UnixMilli()})
}
