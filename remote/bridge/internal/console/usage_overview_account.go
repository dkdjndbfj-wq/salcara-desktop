package console

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/relay"
	"salcara/bridge/internal/usageledger"
)

type usageSourceOption struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Host string `json:"host"`
	Kind string `json:"kind"` // relay | api | official
}

type usageOverviewSelection struct {
	option               usageSourceOption
	root, key, accountID string
	probe                bool
	accountEpoch         uint64
	credentialRef        string
	vaultEpoch           uint64
	vaultIdentity        uint64
	vaultName            string
	invalid              string
}

type usageOverviewResponse struct {
	Days           int                 `json:"days"`
	Sources        []usageSource       `json:"sources"`
	Options        []usageSourceOption `json:"options"`
	SelectedSource string              `json:"selectedSource"`
	RangeEnd       string              `json:"rangeEnd"`
	UpdatedAt      int64               `json:"updatedAt"`
	Error          string              `json:"error,omitempty"`
}

func usageOverviewHost(root string) string {
	u, err := url.Parse(root)
	if err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" {
		return u.Host
	}
	return ""
}

// Selection is by saved source ID, never name, host, or a deduplicated key.
// List is a local vault read; building the menu never probes an official API.
func (s *Server) usageOverviewSelections(c config.Config) []usageOverviewSelection {
	out := []usageOverviewSelection{}
	if c.LoggedIn() && !c.RemoteDeviceOnly {
		out = append(out, usageOverviewSelection{option: usageSourceOption{ID: "relay", Name: "中转站", Host: usageOverviewHost(c.RelayRoot), Kind: "relay"}, root: c.RelayRoot, key: c.AccountKey, probe: true})
	}
	vault := map[string]codexaccounts.Account{}
	if s.d.CodexAccounts != nil {
		for _, row := range s.d.CodexAccounts.List() {
			vault[row.ID] = row
		}
	}
	attached := map[string]bool{}
	for _, a := range c.LocalAccounts {
		target := usageOverviewSelection{option: usageSourceOption{ID: a.ID, Name: strings.TrimSpace(a.Name), Host: usageOverviewHost(a.BaseURL), Kind: "api"}, root: config.NormalizeRelayRoot(a.BaseURL), key: strings.TrimSpace(a.Key), accountID: a.ID}
		if target.option.Name == "" {
			target.option.Name = target.option.Host
		}
		if target.option.Name == "" {
			target.option.Name = a.ID
		}
		if a.SourceType == "official_account" || a.CredentialRef != "" {
			target.option.Kind, target.option.Host = "official", ""
			target.credentialRef, target.accountEpoch = a.CredentialRef, a.CredentialEpoch
			row, ok := vault[a.CredentialRef]
			if ok {
				// A replaced identity still belongs to this saved choice. Do not
				// append a vault-only alias that silently selects the new identity.
				attached[row.ID] = true
			}
			if !ok {
				target.invalid = "官方账号已删除或暂不可用，请重新选择"
			} else if a.CredentialEpoch == 0 || a.CredentialEpoch != row.IdentityEpoch {
				target.invalid = "官方账号身份已变更，请重新关联后读取"
			} else {
				target.vaultEpoch, target.vaultIdentity, target.vaultName = row.Epoch, row.IdentityEpoch, row.Name
			}
		} else {
			switch a.SourceType {
			case "", "api_key", "reverse_proxy":
				target.probe = target.root != "" && target.key != ""
			}
		}
		out = append(out, target)
	}
	ids := make([]string, 0, len(vault))
	for id := range vault {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if attached[id] {
			continue
		}
		row := vault[id]
		// The normal attach path uses this exact local account ID. A vault-only
		// account has a known empty history until records exist for that ID and
		// current identity; never borrow another saved alias's records.
		localID := "official-" + id
		out = append(out, usageOverviewSelection{option: usageSourceOption{ID: localID, Name: row.Name, Kind: "official"}, accountID: localID, accountEpoch: row.IdentityEpoch, credentialRef: id, vaultEpoch: row.Epoch, vaultIdentity: row.IdentityEpoch, vaultName: row.Name})
	}
	return out
}

func usageOverviewOptions(targets []usageOverviewSelection) []usageSourceOption {
	out := make([]usageSourceOption, 0, len(targets))
	for _, target := range targets {
		out = append(out, target.option)
	}
	return out
}

func (s *Server) writeUsageOverviewError(w http.ResponseWriter, status, days int, selected, message string, now time.Time) {
	options := []usageSourceOption{}
	if s.d.Store != nil {
		options = usageOverviewOptions(s.usageOverviewSelections(s.d.Store.Get()))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	writeJSON(w, usageOverviewResponse{Days: days, Sources: []usageSource{}, Options: options, SelectedSource: selected, RangeEnd: now.Format("2006-01-02"), UpdatedAt: now.UnixMilli(), Error: message})
}

func (s *Server) handleSelectedUsageOverview(w http.ResponseWriter, r *http.Request, days int, selected string) {
	now := time.Now()
	captured := s.d.Store.Get()
	targets := s.usageOverviewSelections(captured)
	var target usageOverviewSelection
	matches := 0
	for _, candidate := range targets {
		if candidate.option.ID == selected {
			target = candidate
			matches++
		}
	}
	if matches == 0 {
		s.writeUsageOverviewError(w, http.StatusNotFound, days, selected, "所选用量来源不存在或已删除，请重新选择", now)
		return
	}
	if matches > 1 || target.invalid != "" {
		message := target.invalid
		if matches > 1 {
			message = "用量来源标识冲突，请先修复账号来源后重新选择"
		}
		s.writeUsageOverviewError(w, http.StatusConflict, days, selected, message, now)
		return
	}
	ctx, cancel := ctxTimeout(r, 20*time.Second)
	defer cancel()
	src := usageSource{ID: target.option.ID, Name: target.option.Name, Host: target.option.Host, Kind: target.option.Kind}
	if target.accountID != "" {
		local, err := s.usageOverviewAccountTokens(ctx, target.accountID, target.accountEpoch, days, now)
		if err != nil {
			src.LocalUsageError = err.Error()
		} else {
			src.LocalUsage = local
		}
	}
	if target.option.Kind == "official" {
		// Official Token history is entirely local; there is no upstream
		// balance, OAuth request, or credential refresh in this endpoint.
		src.OK = true
	} else if target.probe {
		usage, err := relay.UsageDays(ctx, target.root, target.key, days)
		if err != nil {
			src.Error = err.Error()
		} else {
			src.OK, src.Usage = true, usage
		}
	} else {
		src.Error = "此来源未提供可读取的上游用量"
	}
	// Single selections never share the overview/ball cache or its query
	// sequence. Store's committed revision still retires credential ABA reads.
	current := s.d.Store.Get()
	changed := !s.usageOverviewSelectionCurrent(captured, current, target)
	if changed || time.Now().Format("2006-01-02") != now.Format("2006-01-02") {
		s.writeUsageOverviewError(w, http.StatusConflict, days, selected, "用量来源或日期已变更，请重新读取", time.Now())
		return
	}
	writeJSON(w, usageOverviewResponse{Days: days, Sources: []usageSource{src}, Options: usageOverviewOptions(s.usageOverviewSelections(current)), SelectedSource: selected, RangeEnd: now.Format("2006-01-02"), UpdatedAt: now.UnixMilli()})
}

func (s *Server) usageOverviewSelectionCurrent(captured, current config.Config, target usageOverviewSelection) bool {
	if current.UsageSourceRevision != captured.UsageSourceRevision || current.UsageSourceFingerprint() != captured.UsageSourceFingerprint() {
		return false
	}
	if target.credentialRef != "" {
		if s.d.CodexAccounts == nil {
			return false
		} else if row, err := s.d.CodexAccounts.Get(target.credentialRef); err != nil || row.Epoch != target.vaultEpoch || row.IdentityEpoch != target.vaultIdentity || row.Name != target.vaultName {
			return false
		}
	}
	return true
}

type usageAccountActivity struct {
	Tokens   *int64 `json:"total_tokens"`
	Requests int    `json:"requests"`
}

type usageAccountDay struct {
	Date string `json:"date"`
	usageAccountActivity
}

type usageAccountModel struct {
	Model string `json:"model"`
	usageAccountActivity
}

type usageAccountLocalUsage struct {
	Daily  []usageAccountDay   `json:"daily_usage"`
	Models []usageAccountModel `json:"model_stats"`
	Usage  struct {
		Today usageAccountActivity `json:"today"`
	} `json:"usage"`
	Local   bool `json:"local"`
	Partial bool `json:"partial"`
}

func emptyUsageAccountActivity() usageAccountActivity {
	return usageAccountActivity{Tokens: new(int64)}
}

func (a *usageAccountActivity) add(record usageledger.AccountTokenRecord) {
	a.Requests++
	if a.Tokens == nil || record.Tokens == nil {
		a.Tokens = nil
		return
	}
	*a.Tokens += *record.Tokens
}

// Visit one durable snapshot for the exact anonymous account and identity.
// Rebucket records in the same local calendar used by rangeEnd; Query.Trend
// is UTC and cannot represent local-midnight or DST boundaries.
func (s *Server) usageOverviewAccountTokens(ctx context.Context, accountID string, epoch uint64, days int, now time.Time) (*usageAccountLocalUsage, error) {
	if s.usageLedger == nil {
		return nil, errors.New("本机请求记录暂不可用")
	}
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	start, end := day.AddDate(0, 0, 1-days), day.AddDate(0, 0, 1)
	filter := usageledger.AccountTokenFilter{AccountID: s.usageLedger.AnonymousAccountID(accountID), AccountEpoch: epoch, From: start.UnixMilli(), To: end.UnixMilli() - 1}
	if filter.AccountID == "" {
		return nil, errors.New("本机请求记录账号不可用")
	}
	out := &usageAccountLocalUsage{Daily: []usageAccountDay{}, Models: []usageAccountModel{}, Local: true}
	byDay := map[string]*usageAccountDay{}
	for date := start; date.Before(end); date = date.AddDate(0, 0, 1) {
		out.Daily = append(out.Daily, usageAccountDay{Date: date.Format("2006-01-02"), usageAccountActivity: emptyUsageAccountActivity()})
	}
	for i := range out.Daily {
		byDay[out.Daily[i].Date] = &out.Daily[i]
	}
	byModel := map[string]*usageAccountModel{}
	status, err := s.usageLedger.VisitAccountTokens(ctx, filter, func(record usageledger.AccountTokenRecord) {
		date := time.UnixMilli(record.Time).In(now.Location()).Format("2006-01-02")
		bucket := byDay[date]
		if bucket == nil {
			return
		}
		bucket.add(record)
		model := byModel[record.Model]
		if model == nil {
			model = &usageAccountModel{Model: record.Model, usageAccountActivity: emptyUsageAccountActivity()}
			byModel[record.Model] = model
		}
		model.add(record)
		out.Partial = out.Partial || record.Tokens == nil || !record.UsageFinal
	})
	if err != nil {
		return nil, err
	}
	out.Partial = out.Partial || status.Pending > 0 || status.Unsaved || status.Dropped > 0 || status.SaveError != "" || status.ReadError != ""
	out.Partial = out.Partial || status.Retention.MaxRecords > 0 && status.Count >= status.Retention.MaxRecords
	// Retention is a rolling instant, not a calendar-day boundary. Both fully
	// expired days and the day containing the cutoff have incomplete totals.
	for i, date := 0, start; i < len(out.Daily); i, date = i+1, date.AddDate(0, 0, 1) {
		if date.UnixMilli() < status.RetentionCutoff {
			out.Daily[i].Tokens = nil
			out.Partial = true
		}
	}
	for _, model := range byModel {
		out.Models = append(out.Models, *model)
	}
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].Model < out.Models[j].Model })
	out.Usage.Today = byDay[day.Format("2006-01-02")].usageAccountActivity
	return out, nil
}
