package console

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"salcara/bridge/internal/atomicfile"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/toolcfg"
)

// MCP servers are configured once in Bridge and written into every Agent that should see them.
// Bridge keeps its own registry (mcp-servers.json) so it only ever touches entries it manages;
// servers a user added directly to Codex or Claude Code are shown as "discovered" until imported.

var mcpAgents = []string{"codex", "claude"}

type mcpEntry struct {
	toolcfg.MCPServer
	Enabled bool            `json:"enabled"`
	Agents  map[string]bool `json:"agents"`
}

type mcpRegistry struct {
	Version int        `json:"version"`
	Servers []mcpEntry `json:"servers"`
}

var (
	mcpMu           sync.Mutex
	mcpRegistryPath = func() (string, error) {
		dir, err := config.Dir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, "mcp-servers.json"), nil
	}
	mcpAgentPaths = func() map[string]string {
		return map[string]string{"codex": toolcfg.CodexConfigPath(), "claude": toolcfg.ClaudeUserConfigPath()}
	}
)

func loadMCPRegistry() (mcpRegistry, error) {
	path, err := mcpRegistryPath()
	if err != nil {
		return mcpRegistry{}, err
	}
	reg := mcpRegistry{Version: 1}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return reg, nil
	}
	if err != nil {
		return reg, err
	}
	if err := json.Unmarshal(b, &reg); err != nil {
		return mcpRegistry{Version: 1}, errors.New("MCP 配置文件损坏，请检查 mcp-servers.json")
	}
	for i := range reg.Servers {
		reg.Servers[i] = normalizeMCPEntry(reg.Servers[i])
	}
	return reg, nil
}

func saveMCPRegistry(reg mcpRegistry) error {
	path, err := mcpRegistryPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	reg.Version = 1
	b, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WriteFileKeepMode(path, append(b, '\n'), 0o600)
}

func (reg *mcpRegistry) find(name string) int {
	for i, s := range reg.Servers {
		if s.Name == name {
			return i
		}
	}
	return -1
}

func normalizeMCPEntry(e mcpEntry) mcpEntry {
	e.Name = strings.TrimSpace(e.Name)
	e.Command = strings.TrimSpace(e.Command)
	e.URL = strings.TrimSpace(e.URL)
	agents := map[string]bool{}
	for _, a := range mcpAgents {
		on, set := e.Agents[a]
		agents[a] = on || !set && e.Agents == nil
	}
	e.Agents = agents
	return e
}

// syncMCP writes the registry into each Agent's configuration. removed lists managed names that
// must disappear everywhere (renamed or deleted entries).
func syncMCP(reg mcpRegistry, removed []string) error {
	var errs []string
	for agent, path := range mcpAgentPaths() {
		var upsert []toolcfg.MCPServer
		remove := append([]string{}, removed...)
		for _, s := range reg.Servers {
			if s.Enabled && s.Agents[agent] {
				upsert = append(upsert, s.MCPServer)
			} else {
				remove = append(remove, s.Name)
			}
		}
		var err error
		if agent == "codex" {
			err = toolcfg.ApplyCodexMCP(path, upsert, remove)
		} else {
			err = toolcfg.ApplyClaudeMCP(path, upsert, remove)
		}
		if err != nil {
			label := map[string]string{"codex": "Codex", "claude": "Claude Code"}[agent]
			errs = append(errs, label+"："+err.Error())
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return errors.New(strings.Join(errs, "；"))
	}
	return nil
}

type mcpDiscovered struct {
	toolcfg.MCPServer
	From []string `json:"from"`
}

type mcpListResponse struct {
	Servers    []mcpEntry        `json:"servers"`
	Discovered []mcpDiscovered   `json:"discovered"`
	Paths      map[string]string `json:"paths"`
	Errors     map[string]string `json:"errors,omitempty"`
}

func mcpList() (mcpListResponse, error) {
	reg, err := loadMCPRegistry()
	if err != nil {
		return mcpListResponse{}, err
	}
	resp := mcpListResponse{Servers: reg.Servers, Discovered: []mcpDiscovered{}, Paths: mcpAgentPaths(), Errors: map[string]string{}}
	if resp.Servers == nil {
		resp.Servers = []mcpEntry{}
	}
	found := map[string]*mcpDiscovered{}
	var order []string
	for _, agent := range mcpAgents {
		path := resp.Paths[agent]
		var servers map[string]toolcfg.MCPServer
		if agent == "codex" {
			servers, err = toolcfg.ReadCodexMCP(path)
		} else {
			servers, err = toolcfg.ReadClaudeMCP(path)
		}
		if err != nil {
			resp.Errors[agent] = err.Error()
			continue
		}
		for name, s := range servers {
			if reg.find(name) >= 0 || !toolcfg.ValidMCPName(name) || s.Validate() != nil {
				continue
			}
			if d, ok := found[name]; ok {
				d.From = append(d.From, agent)
				continue
			}
			found[name] = &mcpDiscovered{MCPServer: s, From: []string{agent}}
			order = append(order, name)
		}
	}
	sort.Strings(order)
	for _, name := range order {
		resp.Discovered = append(resp.Discovered, *found[name])
	}
	if len(resp.Errors) == 0 {
		resp.Errors = nil
	}
	return resp, nil
}

func (s *Server) mcpRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/mcp", s.handleMCPList)
	m.HandleFunc("POST /api/mcp/save", s.handleMCPSave)
	m.HandleFunc("POST /api/mcp/toggle", s.handleMCPToggle)
	m.HandleFunc("POST /api/mcp/remove", s.handleMCPRemove)
	m.HandleFunc("POST /api/mcp/import", s.handleMCPImport)
}

func (s *Server) handleMCPList(w http.ResponseWriter, r *http.Request) {
	mcpMu.Lock()
	defer mcpMu.Unlock()
	resp, err := mcpList()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, resp)
}

// mcpCommit saves the registry and pushes it to the Agents, then answers with the fresh list.
func mcpCommit(w http.ResponseWriter, reg mcpRegistry, removed []string) {
	if err := saveMCPRegistry(reg); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	syncErr := syncMCP(reg, removed)
	resp, err := mcpList()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if syncErr != nil {
		if resp.Errors == nil {
			resp.Errors = map[string]string{}
		}
		resp.Errors["sync"] = syncErr.Error()
	}
	writeJSON(w, resp)
}

func (s *Server) handleMCPSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Server       mcpEntry `json:"server"`
		PreviousName string   `json:"previousName"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	entry := normalizeMCPEntry(req.Server)
	if err := entry.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	mcpMu.Lock()
	defer mcpMu.Unlock()
	reg, err := loadMCPRegistry()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var removed []string
	prev := strings.TrimSpace(req.PreviousName)
	if prev != "" && prev != entry.Name {
		if i := reg.find(prev); i >= 0 {
			reg.Servers = append(reg.Servers[:i], reg.Servers[i+1:]...)
			removed = append(removed, prev)
		}
	}
	if i := reg.find(entry.Name); i >= 0 {
		if prev == "" {
			writeErr(w, http.StatusConflict, "已经有同名的 MCP 服务器")
			return
		}
		reg.Servers[i] = entry
	} else {
		reg.Servers = append(reg.Servers, entry)
	}
	mcpCommit(w, reg, removed)
}

func (s *Server) handleMCPToggle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		Enabled *bool  `json:"enabled"`
		Agent   string `json:"agent"`
	}
	if err := readJSON(r, &req); err != nil || req.Enabled == nil {
		writeErr(w, http.StatusBadRequest, "请求格式有误")
		return
	}
	mcpMu.Lock()
	defer mcpMu.Unlock()
	reg, err := loadMCPRegistry()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	i := reg.find(req.Name)
	if i < 0 {
		writeErr(w, http.StatusNotFound, "没有找到这个 MCP 服务器")
		return
	}
	switch req.Agent {
	case "":
		reg.Servers[i].Enabled = *req.Enabled
	case "codex", "claude":
		reg.Servers[i].Agents[req.Agent] = *req.Enabled
	default:
		writeErr(w, http.StatusBadRequest, "不支持这个 Agent")
		return
	}
	mcpCommit(w, reg, nil)
}

func (s *Server) handleMCPRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	mcpMu.Lock()
	defer mcpMu.Unlock()
	reg, err := loadMCPRegistry()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	i := reg.find(req.Name)
	if i < 0 {
		writeErr(w, http.StatusNotFound, "没有找到这个 MCP 服务器")
		return
	}
	reg.Servers = append(reg.Servers[:i], reg.Servers[i+1:]...)
	mcpCommit(w, reg, []string{req.Name})
}

// handleMCPImport takes discovered servers under Bridge management (all of them when names is
// empty) and shares each with every Agent.
func (s *Server) handleMCPImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Names []string `json:"names"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	mcpMu.Lock()
	defer mcpMu.Unlock()
	list, err := mcpList()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	reg, err := loadMCPRegistry()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	want := map[string]bool{}
	for _, n := range req.Names {
		want[n] = true
	}
	for _, d := range list.Discovered {
		if len(want) > 0 && !want[d.Name] {
			continue
		}
		reg.Servers = append(reg.Servers, normalizeMCPEntry(mcpEntry{MCPServer: d.MCPServer, Enabled: true}))
	}
	mcpCommit(w, reg, nil)
}
