package console

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"

	"salcara/bridge/internal/config"
)

var errLocalModelRulesAccountMissing = errors.New("账号已移除，请刷新列表")

// handleLocalModelRules accepts only model policy. Never reconstruct an account
// from its public DTO: official credential references and epochs are private.
func (s *Server) handleLocalModelRules(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID                     string    `json:"id"`
		ExpectedSourceRevision *string   `json:"expectedSourceRevision"`
		ModelAllowlist         *[]string `json:"modelAllowlist"`
		ModelDenylist          *[]string `json:"modelDenylist"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式有误")
		return
	}
	if err := dec.Decode(new(any)); err != io.EOF || !config.ValidAccountID(in.ID) || in.ModelAllowlist == nil || in.ModelDenylist == nil {
		writeErr(w, http.StatusBadRequest, "请提交完整的模型规则")
		return
	}
	if in.ExpectedSourceRevision == nil {
		writeErr(w, http.StatusConflict, errLocalSourceChanged.Error())
		return
	}

	changed := false
	// Serialize policy publication with model probes so an earlier catalog
	// response cannot restore routes that the new policy has just invalidated.
	s.modelProbeMu.Lock()
	committed, err := s.d.Store.UpdateWithSnapshot(func(c *config.Config) error {
		for i, old := range c.LocalAccounts {
			if old.ID != in.ID {
				continue
			}
			if err := checkLocalSourceRevision(*c, old, in.ExpectedSourceRevision); err != nil {
				return err
			}
			candidate := old
			candidate.ModelAllowlist, candidate.ModelDenylist = *in.ModelAllowlist, *in.ModelDenylist
			if err := config.ValidateLocalAccount(&candidate); err != nil {
				return err
			}
			changed = !slices.Equal(old.ModelAllowlist, candidate.ModelAllowlist) || !slices.Equal(old.ModelDenylist, candidate.ModelDenylist)
			if !changed {
				return nil
			}
			// Copy only validated policy fields; validation must not normalize or
			// replace the saved source, credentials, pool settings or display name.
			old.ModelAllowlist, old.ModelDenylist = candidate.ModelAllowlist, candidate.ModelDenylist
			old.Models, old.ModelsManual, old.Model = nil, false, ""
			c.LocalAccounts[i] = old
			invalidateAccountModelSelections(c, old, true)
			return nil
		}
		return errLocalModelRulesAccountMissing
	})
	if err == nil && changed {
		delete(s.modelProbeSeq, in.ID)
	}
	s.modelProbeMu.Unlock()
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errLocalSourceChanged) {
			status = http.StatusConflict
		} else if errors.Is(err, errLocalModelRulesAccountMissing) {
			status = http.StatusNotFound
		}
		writeErr(w, status, err.Error())
		return
	}
	if changed && s.d.Hub != nil {
		s.d.Hub.InvalidateModelCatalog(in.ID)
	}
	account, _ := committed.LocalAccount(in.ID)
	writeJSON(w, map[string]any{
		"ok": true, "id": account.ID, "sourceRevision": committed.AccountSourceRevision(account),
		"modelAllowlist": account.ModelAllowlist, "modelDenylist": account.ModelDenylist,
		"modelsInvalidated": changed,
	})
}
