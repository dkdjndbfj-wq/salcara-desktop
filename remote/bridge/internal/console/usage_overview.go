package console

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

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
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Host  string         `json:"host"`
	Kind  string         `json:"kind"` // relay | api | local
	OK    bool           `json:"ok"`
	Error string         `json:"error,omitempty"`
	Usage map[string]any `json:"usage,omitempty"`
}

var usageOverview struct {
	mu   sync.Mutex
	key  string
	at   time.Time
	data []usageSource
}

func hostOf(root string) string {
	if u, err := url.Parse(root); err == nil && u.Host != "" {
		return u.Host
	}
	return root
}

func (s *Server) handleUsageOverview(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 {
		days = 14
	}
	if days > 90 {
		days = 90
	}
	c := s.d.Store.Get()

	type target struct {
		src       usageSource
		root, key string
	}
	var targets []target
	seen := map[string]bool{}
	add := func(t target) {
		if t.root == "" || t.key == "" || seen[t.root+"|"+t.key] {
			return
		}
		seen[t.root+"|"+t.key] = true
		targets = append(targets, t)
	}
	if c.LoggedIn() && !c.RemoteDeviceOnly {
		add(target{src: usageSource{ID: "relay", Name: "中转站", Host: hostOf(c.RelayRoot), Kind: "relay"}, root: c.RelayRoot, key: c.AccountKey})
	}
	for _, a := range c.LocalAccounts {
		root := config.NormalizeRelayRoot(a.BaseURL)
		name := strings.TrimSpace(a.Name)
		if name == "" {
			name = hostOf(root)
		}
		add(target{src: usageSource{ID: a.ID, Name: name, Host: hostOf(root), Kind: "api"}, root: root, key: strings.TrimSpace(a.Key)})
	}

	cacheKey := strconv.Itoa(days)
	for _, t := range targets {
		cacheKey += "|" + t.src.ID + "@" + t.root
	}
	force := r.URL.Query().Get("refresh") == "1"
	usageOverview.mu.Lock()
	if !force && usageOverview.key == cacheKey && time.Since(usageOverview.at) < 60*time.Second {
		data := usageOverview.data
		at := usageOverview.at
		usageOverview.mu.Unlock()
		writeJSON(w, map[string]any{"days": days, "sources": data, "updatedAt": at.UnixMilli()})
		return
	}
	usageOverview.mu.Unlock()

	ctx, cancel := ctxTimeout(r, 20*time.Second)
	defer cancel()
	out := make([]usageSource, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t target) {
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
	usageOverview.mu.Lock()
	usageOverview.key, usageOverview.at, usageOverview.data = cacheKey, now, out
	usageOverview.mu.Unlock()
	writeJSON(w, map[string]any{"days": days, "sources": out, "updatedAt": now.UnixMilli()})
}
