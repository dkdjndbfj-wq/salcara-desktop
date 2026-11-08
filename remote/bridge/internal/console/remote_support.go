package console

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/hubclient"
)

type remoteSupportDTO struct {
	APIRemote     string `json:"apiRemote"`
	AccountRemote string `json:"accountRemote"`
	CheckedAt     int64  `json:"checkedAt,omitempty"`
	FreshUntil    int64  `json:"freshUntil,omitempty"`
	HubURL        string `json:"hubUrl,omitempty"`
}

func unknownRemoteSupport() remoteSupportDTO {
	return remoteSupportDTO{APIRemote: "unknown", AccountRemote: "unknown"}
}

// These display hints never arm a remote worker or grant execution authority.
func stationRemoteSupport(observation *config.StationObservation, account *config.LocalAccount, hubURL string) remoteSupportDTO {
	out := unknownRemoteSupport()
	out.HubURL = hubURL
	if observation != nil {
		out.CheckedAt = observation.CheckedAt
	}
	if observation == nil || observation.Revision == "" || !config.ValidStationPolicy(observation.Station) {
		return out
	}
	out.CheckedAt, out.FreshUntil = observation.CheckedAt, observation.CheckedAt+int64((5*time.Minute)/time.Millisecond)
	if !observation.Available() {
		if observation.Mode == "disabled" && observation.CheckedAt > 0 && observation.CheckedAt <= time.Now().UnixMilli()+30000 && out.FreshUntil > time.Now().UnixMilli() {
			out.APIRemote, out.AccountRemote = "unsupported", "unsupported"
		}
		return out
	}
	out.APIRemote, out.AccountRemote = "unsupported", "unsupported"
	if account == nil && config.StationHasAPIAccounts(observation.Station) || account != nil && config.StationAllowsAccount(observation.Station, *account) {
		out.APIRemote = "supported"
	}
	if observation.Station.AllowOfficialAccounts {
		out.AccountRemote = "supported"
	}
	return out
}

func remoteOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	root, err := config.NormalizeStationAPIBase(u.Scheme + "://" + u.Host)
	if err != nil {
		return ""
	}
	return root
}

func accountSavedRemoteSupport(c config.Config, account config.LocalAccount) remoteSupportDTO {
	out := unknownRemoteSupport()
	if config.IsProtectedOfficialAccount(account) {
		return out
	}
	origin := remoteOrigin(account.BaseURL)
	if origin == "" {
		return out
	}
	first := true
	merge := func(previous, next string) string {
		if previous == "supported" || next == "supported" {
			return "supported"
		}
		if previous == "unknown" || next == "unknown" {
			return "unknown"
		}
		return "unsupported"
	}
	for _, station := range c.RemoteConnections {
		if remoteOrigin(station.HubURL) != origin {
			continue
		}
		next := stationRemoteSupport(station.RemoteSupport, &account, station.HubURL)
		if first {
			out, first = next, false
			continue
		}
		out.APIRemote, out.AccountRemote = merge(out.APIRemote, next.APIRemote), merge(out.AccountRemote, next.AccountRemote)
		out.HubURL = ""
		if next.FreshUntil > time.Now().UnixMilli() && (out.FreshUntil <= time.Now().UnixMilli() || next.FreshUntil < out.FreshUntil) {
			out.CheckedAt, out.FreshUntil = next.CheckedAt, next.FreshUntil
		}
	}
	return out
}

func accountRemoteDiscoveryTarget(c config.Config, account config.LocalAccount) (string, error) {
	origin := remoteOrigin(account.BaseURL)
	saved, matching := map[string]bool{}, map[string]bool{}
	for _, station := range c.RemoteConnections {
		if remoteOrigin(station.HubURL) != origin {
			continue
		}
		target, err := remoteDiscoveryTarget(origin, station.HubURL)
		if err != nil {
			continue
		}
		saved[target] = true
		// An older declaration can locate the saved plugin, but only its new
		// response below may determine current support for this full API base.
		if station.RemoteSupport != nil && config.ValidStationPolicy(station.RemoteSupport.Station) && config.StationAllowsAccount(station.RemoteSupport.Station, account) {
			matching[target] = true
		}
	}
	if len(matching) == 1 {
		for target := range matching {
			return target, nil
		}
	}
	if len(saved) == 1 {
		for target := range saved {
			return target, nil
		}
	}
	if len(saved) > 1 {
		return "", errors.New("本站保存了多个插件地址，请先在手机远程中检测对应站点")
	}
	return remoteDiscoveryTarget(origin, "")
}

// The request identifies a saved source, not a free-form URL or credential.
// Detection sends only a same-origin plugin ping and never enrolls the device.
func (s *Server) handleAccountRemoteSupport(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID                     string  `json:"id"`
		ExpectedSourceRevision *string `json:"expectedSourceRevision"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if dec.Decode(&in) != nil || dec.Decode(new(any)) != io.EOF || !config.ValidAccountID(in.ID) {
		writeErr(w, http.StatusBadRequest, "请求格式有误")
		return
	}
	c := s.d.Store.Get()
	account, exists := c.LocalAccount(in.ID)
	if !exists {
		writeErr(w, http.StatusNotFound, "账号已移除，请刷新列表")
		return
	}
	if in.ExpectedSourceRevision == nil || checkLocalSourceRevision(c, account, in.ExpectedSourceRevision) != nil {
		writeErr(w, http.StatusConflict, errLocalSourceChanged.Error())
		return
	}
	base, err := config.NormalizeStationAPIBase(account.BaseURL)
	if err != nil || account.CredentialRef != "" || account.SourceType != "api_key" && account.SourceType != "reverse_proxy" {
		writeErr(w, http.StatusBadRequest, "请选择有效的 API 账号")
		return
	}
	ctx, cancel := ctxTimeout(r, 10*time.Second)
	defer cancel()
	target, detectErr := accountRemoteDiscoveryTarget(c, account)
	var info hubclient.Discovery
	if detectErr == nil {
		info, detectErr = hubclient.Discover(ctx, remoteOrigin(base), target)
	}
	latest := s.d.Store.Get()
	current, exists := latest.LocalAccount(in.ID)
	if !exists || checkLocalSourceRevision(latest, current, in.ExpectedSourceRevision) != nil {
		writeErr(w, http.StatusConflict, errLocalSourceChanged.Error())
		return
	}
	currentTarget, _ := accountRemoteDiscoveryTarget(latest, current)
	if currentTarget != target {
		writeErr(w, http.StatusConflict, "站点配置已更改，请重新检测")
		return
	}
	out := map[string]any{"ok": detectErr == nil, "id": account.ID, "sourceRevision": *in.ExpectedSourceRevision, "baseUrl": base, "remoteSupport": unknownRemoteSupport()}
	if detectErr == nil {
		out["remoteSupport"] = stationRemoteSupport(info.RemoteAccess, &account, info.HubURL)
	} else {
		out["error"] = "暂时无法确认本站远程能力，请稍后重试"
	}
	writeJSON(w, out)
}

func remoteDiscoveryTarget(raw, override string) (string, error) {
	u, err := hubclient.RemoteURL(raw)
	if err != nil {
		return "", err
	}
	target := u.Scheme + "://" + u.Host + "/salcara-hub"
	if strings.Contains(u.Path, "/salcara-hub") {
		target = config.NormalizeHubURL(u.String())
	}
	if override != "" {
		h, err := hubclient.RemoteURL(override)
		if err != nil || h.Scheme != u.Scheme || h.Host != u.Host {
			return "", errors.New("插件地址必须与中转站同源")
		}
		target = config.NormalizeHubURL(h.String())
	}
	return target, nil
}
