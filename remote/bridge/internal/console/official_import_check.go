package console

import (
	"encoding/json"
	"net/http"

	"salcara/bridge/internal/codexaccounts"
)

func (s *Server) handleOfficialImportCheck(w http.ResponseWriter, r *http.Request) {
	p, _, ok := s.preparedOfficialImport(w, r)
	if !ok {
		return
	}
	result, err := s.d.CodexAccounts.CheckImport(r.Context(), p)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "账号检查未完成，请重试")
		return
	}
	writeJSON(w, result)
}

func selectedOfficialImport(p *codexaccounts.PreparedImport, raw json.RawMessage) (*codexaccounts.PreparedImport, error) {
	var values []*int
	if json.Unmarshal(raw, &values) != nil || len(values) == 0 || len(values) > codexaccounts.MaxAccounts {
		return nil, codexaccounts.ErrInvalidImport
	}
	indices := make([]int, len(values))
	for i, value := range values {
		if value == nil {
			return nil, codexaccounts.ErrInvalidImport
		}
		indices[i] = *value
	}
	return p.Select(indices)
}
