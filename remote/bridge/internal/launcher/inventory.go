package launcher

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

type inventoryProbe struct{ done chan struct{} }

// InventorySnapshot is a presentation-only stale-while-revalidate read. A page
// must never wait for PowerShell, package discovery or an executable probe.
// Admission to Launch/Switch continues to use authoritative Tools instead.
func (s *Service) InventorySnapshot(paths map[string]string) ([]Tool, bool) {
	encoded, _ := json.Marshal(paths)
	key := string(encoded)
	s.inventoryMu.Lock()
	defer s.inventoryMu.Unlock()
	out := []Tool{}
	if s.inventoryKey == key && s.inventory != nil {
		out = append(out, s.inventory...)
		if time.Since(s.inventoryAt) < 30*time.Second {
			return out, false
		}
	}
	if len(out) == 0 {
		// Persisted installation locations are cheap local metadata. Showing
		// them does not start a tool or bypass authoritative launch validation.
		for _, id := range []string{"codex-desktop", "codex", "claude-desktop", "claude"} {
			t := Tool{ID: id, Name: toolNames[id], Kind: strings.Split(id, "-")[0], Custom: paths[id] != ""}
			if t.Custom {
				t.Path = desktopExecutable(paths[id], t.Kind)
				t.Available = fileExists(t.Path)
			}
			out = append(out, t)
		}
		out = s.applyLocations(out, paths)
	}
	s.startInventoryLocked(key, paths)
	return out, true
}

func (s *Service) startInventoryLocked(key string, paths map[string]string) *inventoryProbe {
	if s.inventoryFlight != nil {
		return s.inventoryFlight
	}
	epoch := s.inventoryEpoch
	flight := &inventoryProbe{done: make(chan struct{})}
	s.inventoryFlight = flight
	ownedPaths := map[string]string{}
	for id, p := range paths {
		ownedPaths[id] = p
	}
	go func() {
		probeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		out := s.Tools(probeCtx, ownedPaths)
		s.inventoryMu.Lock()
		if epoch == s.inventoryEpoch {
			s.inventoryKey, s.inventoryAt, s.inventory = key, time.Now(), append([]Tool{}, out...)
		}
		s.inventoryFlight = nil
		close(flight.done)
		s.inventoryMu.Unlock()
	}()
	return flight
}

// Inventory is read-only UI discovery, not launch admission. Coalesce the slow
// Windows package probe across pages. Tools/Switch still revalidate before acting.
func (s *Service) Inventory(ctx context.Context, paths map[string]string) []Tool {
	encoded, _ := json.Marshal(paths)
	key := string(encoded)
	for {
		s.inventoryMu.Lock()
		if s.inventory != nil && s.inventoryKey == key && time.Since(s.inventoryAt) < 30*time.Second {
			out := append([]Tool{}, s.inventory...)
			s.inventoryMu.Unlock()
			return out
		}
		flight := s.startInventoryLocked(key, paths)
		s.inventoryMu.Unlock()
		select {
		case <-ctx.Done():
			return []Tool{}
		case <-flight.done:
		}
	}
}

func (s *Service) InvalidateInventory() {
	s.inventoryMu.Lock()
	s.inventoryEpoch++
	s.inventoryAt = time.Time{}
	s.inventoryMu.Unlock()
}
