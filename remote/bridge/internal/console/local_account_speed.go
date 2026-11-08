package console

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"salcara/bridge/internal/config"
)

var errAccountSpeedMissing = errors.New("账号已移除，请刷新列表")

// A card changes just its speed preference, not a stale public copy of the
// entire account. In particular it cannot rewrite a protected credential.
func (s *Server) handleLocalAccountSpeed(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID             string  `json:"id"`
		SourceRevision *string `json:"sourceRevision"`
		SpeedMode      string  `json:"speedMode"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式有误")
		return
	}
	if dec.Decode(new(any)) != io.EOF || !config.ValidAccountID(in.ID) || in.SpeedMode == "" || !config.ValidAccountSpeedMode(in.SpeedMode) {
		writeErr(w, http.StatusBadRequest, "请提交有效的速度档位")
		return
	}
	if in.SourceRevision == nil {
		writeErr(w, http.StatusConflict, errLocalSourceChanged.Error())
		return
	}
	committed, err := s.d.Store.UpdateWithSnapshot(func(c *config.Config) error {
		for i, account := range c.LocalAccounts {
			if account.ID != in.ID {
				continue
			}
			if err := checkLocalSourceRevision(*c, account, in.SourceRevision); err != nil {
				return err
			}
			if config.AccountSpeedMode(account.SpeedMode) != in.SpeedMode {
				c.LocalAccounts[i].SpeedMode = in.SpeedMode
			}
			return nil
		}
		return errAccountSpeedMissing
	})
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errLocalSourceChanged) {
			status = http.StatusConflict
		} else if errors.Is(err, errAccountSpeedMissing) {
			status = http.StatusNotFound
		}
		writeErr(w, status, err.Error())
		return
	}
	a, _ := committed.LocalAccount(in.ID)
	writeJSON(w, map[string]any{"ok": true, "id": a.ID, "speedMode": config.AccountSpeedMode(a.SpeedMode), "sourceRevision": committed.AccountSourceRevision(a)})
}
