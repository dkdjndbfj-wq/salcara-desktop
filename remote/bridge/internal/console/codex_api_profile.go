package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/toolcfg"
)

type codexProfileClient struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Revision string   `json:"revision"`
	Models   []string `json:"models"`
}

type codexProfileView struct {
	Profile  toolcfg.CodexServiceProfileStatus `json:"profile"`
	Running  bool                              `json:"running"`
	Active   bool                              `json:"active"`
	ClientID string                            `json:"clientId,omitempty"`
	Model    string                            `json:"model,omitempty"`
	Clients  []codexProfileClient              `json:"clients"`
}

func (s *Server) codexAPIProfileRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/local/codex-api/profile", s.handleCodexAPIProfile)
	m.HandleFunc("POST /api/local/codex-api/profile/apply", s.handleCodexAPIProfileApply)
	m.HandleFunc("POST /api/local/codex-api/profile/restore", s.handleCodexAPIProfileRestore)
}

func (s *Server) apiServiceClosed() bool {
	if s.apiNetwork == nil {
		return false
	}
	s.apiNetwork.mu.Lock()
	defer s.apiNetwork.mu.Unlock()
	return s.apiNetwork.closed
}

// The caller owns codexProfileMu. Capture the target once for this server;
// a later environment change must not redirect restoration to a different home.
func (s *Server) codexServiceProfile() (*toolcfg.CodexServiceProfile, error) {
	if s.codexProfile == nil {
		dir := s.d.Dir
		if dir == "" {
			dir = filepath.Dir(s.d.Store.Path())
		}
		p, err := toolcfg.NewCodexServiceProfile(dir, filepath.Dir(toolcfg.CodexConfigPath()))
		if err != nil {
			return nil, errors.New("Codex 配置目录不可用")
		}
		s.codexProfile = p
	}
	return s.codexProfile, nil
}

// Only previously saved model hints are presented here. Opening this panel
// neither refreshes credentials nor makes a provider request.
func codexProfileModels(c config.Config, client config.NamedAPIClient) []string {
	seen := map[string]bool{}
	for _, id := range managedClientMembers(client) {
		if client.PoolStrategy == config.PoolStrategySingleAccount && len(client.AccountIDs) > 0 && id != client.PoolAccountID {
			continue
		}
		a, ok := c.LocalAccount(id)
		if !ok {
			continue
		}
		models := append(append([]string(nil), a.Models...), a.Model)
		for _, model := range models {
			if !config.ValidCatalogModel(model) || !config.ModelAllowed(a, model) || !config.APIClientModelAllowed(client, model) {
				continue
			}
			if _, shadowed := client.ModelAliases[model]; !shadowed {
				if visible, ok := config.APIClientVisibleModel(client, model); ok {
					seen[visible] = true
				}
			}
			for alias, actual := range client.ModelAliases {
				if actual == model {
					if visible, ok := config.APIClientVisibleModel(client, alias); ok {
						seen[visible] = true
					}
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for model := range seen {
		out = append(out, model)
	}
	sort.Strings(out)
	return out
}

func (s *Server) codexProfileView(ctx context.Context, p *toolcfg.CodexServiceProfile) (codexProfileView, error) {
	status, err := p.Status(ctx)
	if err != nil {
		return codexProfileView{Profile: status}, err
	}
	c := s.d.Store.Get()
	out := codexProfileView{Profile: status, Running: c.CodexAPIActive(), Clients: []codexProfileClient{}}
	b := c.CodexServiceProfile
	out.Active = status.State == "applied" && b.Generation == status.Generation && c.CodexServiceProfileCredential() != ""
	if out.Active {
		out.ClientID, out.Model = b.ClientID, b.Model
	}
	for _, client := range c.CodexAPI.Clients {
		_, revision, err := s.managedLaunchState(c, client.ID, "codex")
		if err != nil {
			continue
		}
		models := codexProfileModels(c, client)
		if len(models) > 0 && len(models) <= 1000 {
			out.Clients = append(out.Clients, codexProfileClient{client.ID, client.Name, revision, models})
		}
	}
	return out, nil
}

func (s *Server) handleCodexAPIProfile(w http.ResponseWriter, r *http.Request) {
	if !s.codexProfileMu.TryLock() {
		writeErr(w, http.StatusConflict, "Codex 配置正在更新")
		return
	}
	defer s.codexProfileMu.Unlock()
	p, err := s.codexServiceProfile()
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	view, err := s.codexProfileView(ctx, p)
	if err != nil {
		message := "Codex 配置状态读取失败"
		if view.Profile.Error != "" {
			message += "：" + view.Profile.Error
		}
		writeErr(w, http.StatusConflict, message)
		return
	}
	writeJSON(w, view)
}

func (s *Server) codexProfileAdmission(ctx context.Context) (func(), error) {
	releaseRemote := func() {}
	if s.d.Hub != nil {
		var err error
		releaseRemote, err = s.d.Hub.BeginLocalToolChange(ctx)
		if err != nil {
			return nil, errors.New("远程任务尚未结束")
		}
	} else if m := s.manager(); m != nil {
		for _, agent := range m.Agents() {
			active, known := agent.(interface{ ActiveRemoteTurns() bool })
			if !known || active.ActiveRemoteTurns() {
				return nil, errors.New("远程任务尚未结束")
			}
		}
	}
	releaseAPI, err := s.apiService.BeginManagedLaunch()
	if err != nil {
		releaseRemote()
		return nil, errors.New("API 服务仍有进行中的请求")
	}
	return func() { releaseAPI(); releaseRemote() }, nil
}

func (s *Server) noWorkbenchCodexBackup() error {
	// Checked again from inside the shared profile lock, so the other writer
	// cannot slip a Workbench snapshot between this check and our first write.
	path := filepath.Join(s.d.Local.Dir, "default-backups", "codex.json")
	if _, err := os.Lstat(path); err == nil {
		return errors.New("Codex 配置仍由工具工作台管理")
	} else if !os.IsNotExist(err) {
		return errors.New("Codex 配置备份状态无法确认")
	}
	return nil
}

func (s *Server) handleCodexAPIProfileApply(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ClientID        string `json:"clientId"`
		ClientRevision  string `json:"clientRevision"`
		ProfileRevision string `json:"profileRevision"`
		Model           string `json:"model"`
	}
	if readReverseJSON(r, &in, 8192) != nil || !config.ValidAccountID(in.ClientID) || !config.ValidCatalogModel(in.Model) || in.ClientRevision == "" || in.ProfileRevision == "" {
		writeErr(w, http.StatusBadRequest, "Codex 配置请求无效")
		return
	}
	if !s.codexProfileMu.TryLock() {
		writeErr(w, http.StatusConflict, "Codex 配置正在更新")
		return
	}
	defer s.codexProfileMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	c := s.d.Store.Get()
	client, revision, err := s.managedLaunchState(c, in.ClientID, "codex")
	if err != nil || revision != in.ClientRevision || !containsLocalModel(codexProfileModels(c, client), in.Model) {
		writeErr(w, http.StatusConflict, "服务、账号或模型配置已变化")
		return
	}
	release, err := s.codexProfileAdmission(ctx)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	defer release()
	p, err := s.codexServiceProfile()
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	guard := func() error {
		_, now, err := s.managedLaunchState(s.d.Store.Get(), in.ClientID, "codex")
		if err != nil || now != revision || ctx.Err() != nil {
			return errors.New("服务或账号授权已变化")
		}
		return s.noWorkbenchCodexBackup()
	}
	if err := guard(); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	lease := config.RandomToken(32)
	key := c.CodexServiceProfileKeyFor(client, c.CodexAPI.AuthEpoch, lease)
	if key == "" {
		writeErr(w, http.StatusConflict, "API 服务授权未就绪")
		return
	}
	// Inference admission is closed until this transaction releases. A stale
	// profile revision must not revoke an otherwise valid current connection.
	status, err := p.Apply(ctx, in.ProfileRevision, toolcfg.CodexServiceProfileParams{
		BaseURL: fmt.Sprintf("http://127.0.0.1:%d/codex-api/profile/v1", s.d.Port), Key: key, Model: in.Model,
	}, guard)
	if err == nil && (status.State != "applied" || status.Generation == "" || !status.HasBackup) {
		err = errors.New("配置提交状态无法确认")
	}
	if err != nil {
		check, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		current, checkErr := p.Status(check)
		done()
		if checkErr != nil || current.State != "applied" || current.Generation != c.CodexServiceProfile.Generation {
			s.d.Store.RevokeCodexServiceProfile(c.CodexServiceProfile.Lease)
		}
		writeErr(w, http.StatusConflict, "Codex 配置应用失败："+err.Error())
		return
	}
	s.d.Store.RevokeCodexServiceProfile(c.CodexServiceProfile.Lease)
	if guard() != nil || !s.d.Store.ArmCodexServiceProfile(client, c.CodexAPI.AuthEpoch, lease, status.Generation, in.Model) {
		// The new key has not been admitted. Revert only the exact generation
		// just written; external edits still cause a conflict rather than loss.
		cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_, rollbackErr := p.Restore(cleanup, status.Revision, status.Generation, func() error {
			b := s.d.Store.Get().CodexServiceProfile
			if b.Lease != "" && b.Lease != lease {
				return errors.New("Codex 配置授权已变化")
			}
			return nil
		})
		done()
		if rollbackErr != nil {
			writeErr(w, http.StatusConflict, "账号授权已变化；配置恢复未完成，连接已停用")
		} else {
			writeErr(w, http.StatusConflict, "账号授权已变化，原配置已恢复")
		}
		return
	}
	now := s.d.Store.Get()
	currentClient, currentRevision, currentErr := s.managedLaunchState(now, in.ClientID, "codex")
	if currentErr != nil || currentRevision != revision || !containsLocalModel(codexProfileModels(now, currentClient), in.Model) || now.CodexServiceProfile.Generation != status.Generation || now.CodexServiceProfileCredential() != key {
		s.d.Store.RevokeCodexServiceProfile(lease)
		writeErr(w, http.StatusConflict, "配置已写入，但账号或服务授权已变化，连接已停用")
		return
	}
	// Return only the accepted generation, never the connection secret.
	writeJSON(w, map[string]any{"ok": true, "profile": status})
}

func (s *Server) handleCodexAPIProfileRestore(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Revision   string `json:"revision"`
		Generation string `json:"generation"`
	}
	if readReverseJSON(r, &in, 4096) != nil || in.Revision == "" || in.Generation == "" {
		writeErr(w, http.StatusBadRequest, "Codex 配置恢复请求无效")
		return
	}
	if !s.codexProfileMu.TryLock() {
		writeErr(w, http.StatusConflict, "Codex 配置正在更新")
		return
	}
	defer s.codexProfileMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	release, err := s.codexProfileAdmission(ctx)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	defer release()
	p, err := s.codexServiceProfile()
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	guard := func() error {
		b := s.d.Store.Get().CodexServiceProfile
		if b.Lease != "" {
			if b.Generation != in.Generation {
				return errors.New("Codex 配置授权已变化")
			}
			s.d.Store.RevokeCodexServiceProfile(b.Lease)
		}
		return nil
	}
	status, err := p.Restore(ctx, in.Revision, in.Generation, guard)
	if err != nil {
		writeErr(w, http.StatusConflict, "Codex 配置恢复失败："+err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "profile": status})
}

// Called after runtime retirement, with codexProfileMu held. An ordinary
// service stop with no owned record must not inspect or change native auth.
func (s *Server) restoreStoppedCodexProfile(parent context.Context, epoch string) error {
	p, err := s.codexServiceProfile()
	if err != nil {
		return err
	}
	exists, err := p.HasRecord()
	if err != nil {
		return errors.New("Codex 配置备份状态无法确认")
	}
	if !exists {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	status, err := p.Status(ctx)
	if err != nil {
		return errors.New("Codex 配置状态读取失败")
	}
	if !status.HasBackup {
		return nil
	}
	_, err = p.Restore(ctx, status.Revision, status.Generation, func() error {
		c := s.d.Store.Get()
		if c.CodexAPI.AuthEpoch != epoch || c.CodexAPIActive() {
			return errors.New("API 服务授权已变化")
		}
		return nil
	})
	if err != nil {
		return errors.New("Codex 原配置未恢复：" + err.Error())
	}
	return nil
}

// Graceful shutdown has the same ownership rules as an explicit stop. Crashes
// keep the journal for a later explicit restore; neither case re-arms a key on
// the next process. CloseAPIListeners marks the listener manager closed first.
func (s *Server) closeCodexServiceProfile() error {
	if s.d.Store == nil {
		return nil
	}
	before := s.d.Store.Get()
	// A network restart may already have advanced this snapshot. This first
	// retirement is best-effort; always reach the final retirement under lock.
	_, _ = s.d.Store.RetireCodexAPIServiceRuntime(before.CodexAPI.AuthEpoch)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for !s.codexProfileMu.TryLock() {
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.New("配置操作尚未退出，连接已停用")
		case <-timer.C:
		}
	}
	defer s.codexProfileMu.Unlock()
	// A network restart already inside its transaction may have retired again
	// after noticing closed listeners. Retire that final snapshot under the same
	// lifecycle lock before using its epoch as the restoration guard.
	current := s.d.Store.Get()
	epoch, ok := s.d.Store.RetireCodexAPIServiceRuntime(current.CodexAPI.AuthEpoch)
	if !ok {
		return errors.New("服务授权已变化，配置恢复未执行")
	}
	return s.restoreStoppedCodexProfile(ctx, epoch)
}
