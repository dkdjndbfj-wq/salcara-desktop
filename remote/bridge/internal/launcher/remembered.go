package launcher

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"salcara/bridge/internal/atomicfile"
)

// Local-only install metadata. This contains no account, key or command line
// and is kept separate from explicit user-selected executable overrides.
type toolLocation struct {
	Path   string `json:"path"`
	Family string `json:"family,omitempty"`
	AppID  string `json:"appId,omitempty"`
}

func (s *Service) loadLocations() {
	s.locations = map[string]toolLocation{}
	if s.Dir == "" {
		return
	}
	f, err := os.Open(filepath.Join(s.Dir, "tool-locations.json"))
	if err != nil {
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (32<<10)+1))
	if err != nil || len(raw) > 32<<10 {
		return
	}
	var locations map[string]toolLocation
	if json.Unmarshal(raw, &locations) != nil {
		return
	}
	for id, location := range locations {
		if ValidTarget(id) && validLocation(location) {
			s.locations[id] = location
		}
	}
}

func validLocation(location toolLocation) bool {
	if !filepath.IsAbs(location.Path) || len(location.Path) > 4096 || strings.ContainsAny(location.Path, "\x00\r\n") || len(location.Family) > 512 || len(location.AppID) > 512 || strings.ContainsAny(location.Family+location.AppID, "\x00\r\n") {
		return false
	}
	if (location.Family == "") != (location.AppID == "") {
		return false
	}
	st, err := os.Stat(location.Path)
	return err == nil && st.Mode().IsRegular()
}

// RememberedPaths is a cheap copy for runtime settings. Explicit configured
// paths always take precedence; unavailable/removed installations are ignored.
func (s *Service) RememberedPaths() map[string]string {
	s.locationsMu.Lock()
	defer s.locationsMu.Unlock()
	paths := map[string]string{}
	for id, location := range s.locations {
		if validLocation(location) {
			paths[id] = location.Path
		}
	}
	return paths
}

func (s *Service) applyLocations(tools []Tool, paths map[string]string) []Tool {
	s.locationsMu.Lock()
	defer s.locationsMu.Unlock()
	for i := range tools {
		tool := &tools[i]
		location, ok := s.locations[tool.ID]
		if !tool.Available && paths[tool.ID] == "" && ok && validLocation(location) {
			tool.Path, tool.Family, tool.AppID, tool.Available = location.Path, location.Family, location.AppID, true
		}
	}
	return tools
}

func (s *Service) rememberLocations(tools []Tool) {
	s.locationsMu.Lock()
	defer s.locationsMu.Unlock()
	if s.locations == nil {
		s.locations = map[string]toolLocation{}
	}
	changed := false
	for _, tool := range tools {
		location := toolLocation{Path: tool.Path, Family: tool.Family, AppID: tool.AppID}
		if !tool.Custom && tool.Available && ValidTarget(tool.ID) && validLocation(location) && s.locations[tool.ID] != location {
			s.locations[tool.ID] = location
			changed = true
		}
	}
	if changed && s.Dir != "" {
		if raw, err := json.Marshal(s.locations); err == nil {
			// Failure to persist metadata must never block using a valid install.
			_ = atomicfile.WriteFile(filepath.Join(s.Dir, "tool-locations.json"), raw, 0o600)
		}
	}
}
