package launcher

import (
	"context"
	"encoding/json"
	"time"
)

type inventoryProbe struct{ done chan struct{} }

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
		flight := s.inventoryFlight
		if flight == nil {
			epoch := s.inventoryEpoch
			flight = &inventoryProbe{done: make(chan struct{})}
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
		}
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
