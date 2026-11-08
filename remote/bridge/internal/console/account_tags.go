package console

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"salcara/bridge/internal/config"
)

type accountTagDisplay struct {
	Tags          []string `json:"tags"`
	Revision      string   `json:"revision"`
	IdentityEpoch uint64   `json:"identityEpoch,omitempty"`
}

func accountTagsDisplay(c config.Config, key string, epoch uint64, source string) accountTagDisplay {
	metadata := c.AccountTags[key]
	tags := append([]string{}, metadata.Tags...)
	if metadata.IdentityEpoch != epoch {
		tags = []string{}
	}
	row := accountTagDisplay{Tags: tags, IdentityEpoch: epoch}
	if c.GatewayRuntimeKey == "" {
		return row
	}
	raw, _ := json.Marshal([]any{"account-tags-v1", key, epoch, source, metadata.Revision, tags})
	mac := hmac.New(sha256.New, []byte(c.GatewayRuntimeKey))
	_, _ = mac.Write(raw)
	row.Revision = "tags-v1-" + hex.EncodeToString(mac.Sum(nil))
	return row
}

func normalizeAccountTags(tags []string) ([]string, bool) {
	if tags == nil || len(tags) > 12 {
		return nil, false
	}
	result := make([]string, 0, len(tags))
	seen := map[string]bool{}
	for _, value := range tags {
		value = strings.TrimSpace(value)
		if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > 24 {
			return nil, false
		}
		for _, ch := range value {
			if unicode.IsControl(ch) || unicode.Is(unicode.Cf, ch) || ch == ',' || ch == '，' {
				return nil, false
			}
		}
		if !seen[value] {
			result = append(result, value)
			seen[value] = true
		}
	}
	return result, true
}

// The request has no credential/model/pool fields. Official metadata is stored
// separately so tagging a vault-only or web-session account cannot grant API use.
func (s *Server) handleAccountTags(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID                     string   `json:"id"`
		Kind                   string   `json:"kind"`
		Tags                   []string `json:"tags"`
		IdentityEpoch          uint64   `json:"identityEpoch,omitempty"`
		ExpectedSourceRevision *string  `json:"expectedSourceRevision,omitempty"`
		ExpectedTagsRevision   string   `json:"expectedTagsRevision"`
	}
	if r.URL.RawQuery != "" || readReverseJSON(r, &in, 4096) != nil || !config.ValidAccountID(in.ID) || in.ExpectedTagsRevision == "" ||
		(in.Kind != "official" && in.Kind != "local") ||
		in.Kind == "official" && (in.IdentityEpoch == 0 || in.ExpectedSourceRevision != nil) ||
		in.Kind == "local" && (in.IdentityEpoch != 0 || in.ExpectedSourceRevision == nil || *in.ExpectedSourceRevision == "") {
		writeErr(w, 400, "标签请求有误")
		return
	}
	tags, valid := normalizeAccountTags(in.Tags)
	if !valid {
		writeErr(w, 400, "最多 12 个标签，每个 1–24 字，请用逗号分隔")
		return
	}
	key := in.Kind + ":" + in.ID
	source := ""
	committed, err := s.d.Store.UpdateWithSnapshot(func(c *config.Config) error {
		if in.Kind == "official" {
			if s.d.CodexAccounts == nil {
				return errLocalSourceChanged
			}
			account, err := s.d.CodexAccounts.Get(in.ID)
			if err != nil || account.IdentityEpoch != in.IdentityEpoch {
				return errLocalSourceChanged
			}
		} else {
			account, ok := c.LocalAccount(in.ID)
			if !ok || account.CredentialRef != "" || checkLocalSourceRevision(*c, account, in.ExpectedSourceRevision) != nil {
				return errLocalSourceChanged
			}
			source = c.AccountSourceRevision(account)
		}
		row := accountTagsDisplay(*c, key, in.IdentityEpoch, source)
		if row.Revision == "" || subtle.ConstantTimeCompare([]byte(row.Revision), []byte(in.ExpectedTagsRevision)) != 1 {
			return errLocalSourceChanged
		}
		metadata := c.AccountTags[key]
		if metadata.Revision == ^uint64(0) {
			return errLocalSourceChanged
		}
		if c.AccountTags == nil {
			c.AccountTags = map[string]config.AccountTagMetadata{}
		}
		c.AccountTags[key] = config.AccountTagMetadata{Tags: tags, IdentityEpoch: in.IdentityEpoch, Revision: metadata.Revision + 1}
		return nil
	})
	if err != nil {
		if errors.Is(err, errLocalSourceChanged) {
			writeErr(w, http.StatusConflict, "账号或标签已变化，请重新打开")
		} else {
			writeErr(w, http.StatusInternalServerError, "标签保存失败")
		}
		return
	}
	writeJSON(w, map[string]any{"ok": true, "id": in.ID, "kind": in.Kind, "metadata": accountTagsDisplay(committed, key, in.IdentityEpoch, source)})
}
