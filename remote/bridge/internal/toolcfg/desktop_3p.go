package toolcfg

// This adapter is for the documented Claude Desktop 3P contract, not the legacy
// %APPDATA%/Claude writer. It never opens chat/session databases.
import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const ClaudeDesktop3PVersion = "2.16120.0"

// Claude3PVersionSupport classifies a Claude Desktop version for the 3P
// configuration contract and the local Chat/Cowork history format.
//
//   - verified: the exact build this adapter was checked against.
//   - compatible: a later build of the same major line. Claude Desktop updates
//     itself often; refusing every new build would silently switch the feature
//     off after each update. Later builds are accepted only in compatibility
//     mode: every reader and writer still validates the actual files strictly
//     (InspectClaude3P, the history record decoder) and fails closed on any
//     structural difference, and switching keeps its restorable backup.
//
// Older builds and other major lines are rejected: they predate or postdate
// the contract and are never guessed at.
func Claude3PVersionSupport(version string) (compatible, verified bool) {
	v := strings.TrimSpace(version)
	if v == ClaudeDesktop3PVersion || v == ClaudeDesktop3PVersion+".0" {
		return true, true
	}
	got, ok := parseDesktopVersion(version)
	base, _ := parseDesktopVersion(ClaudeDesktop3PVersion)
	if !ok || got[0] != base[0] {
		return false, false
	}
	for i := 1; i < 3; i++ {
		if got[i] != base[i] {
			return got[i] > base[i], false
		}
	}
	return true, false
}

func parseDesktopVersion(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimSpace(v), ".")
	if len(parts) < 3 || len(parts) > 4 {
		return out, false
	}
	for i, part := range parts {
		if part == "" || len(part) > 9 {
			return out, false
		}
		n := 0
		for _, c := range part {
			if c < '0' || c > '9' {
				return out, false
			}
			n = n*10 + int(c-'0')
		}
		if i < 3 {
			out[i] = n
		}
	}
	return out, true
}

// This is the bridge's bounded catalog budget, not a native Claude limit.
const ClaudeDesktop3PCatalogLimit = 1000

var desktop3PUUID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[1-8][a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$`)

func ValidClaudeDesktopProfileID(id string) bool { return desktop3PUUID.MatchString(id) }

func ValidClaudeDesktopModel(model string) bool {
	// This is a data boundary, not a claim that the native app accepts every
	// upstream model ID. The local gateway publishes native-compatible routes
	// and resolves those routes against the selected API's actual catalog.
	if model == "" || len(model) > 200 || strings.TrimSpace(model) != model || !utf8.ValidString(model) {
		return false
	}
	for _, r := range model {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

type Claude3PState struct {
	Mode                string `json:"mode"`
	RequiresModeChange  bool   `json:"requiresModeChange"`
	CatalogOverride     bool   `json:"catalogOverride"`
	ModelDiscovery      bool   `json:"modelDiscoveryEnabled"`
	ProfileID           string `json:"-"`
	Root                string `json:"-"`
	mode, meta, profile map[string]json.RawMessage
}

type Claude3PGateway struct {
	Name, BaseURL, Key, AuthMode, Model string
	Models                              []string
	CatalogOverride                     bool
	CatalogEntries                      []Claude3PModelEntry
}

// Claude3PModelEntry contains only verified native picker fields. Name is the
// gateway route, whereas LabelOverride honestly displays the upstream model ID.
type Claude3PModelEntry struct {
	Name          string
	LabelOverride string
	MaxEffort     string
	Supports1M    bool
	Prefer1M      bool
}

type Claude3PWritePlan struct {
	State                           Claude3PState
	ProfileID                       string
	ModePath, MetaPath, ProfilePath string
	ModeData, MetaData, ProfileData []byte
}

// InspectClaude3P reads only the three small configuration files. Invalid or
// externally managed libraries fail closed rather than becoming fresh profiles.
func InspectClaude3P(root string) (Claude3PState, error) {
	s := Claude3PState{Mode: "standard", RequiresModeChange: true, Root: root}
	if !filepath.IsAbs(root) {
		return s, errors.New("Claude 3P 配置目录需要绝对路径")
	}
	var err error
	if s.mode, err = read3PObject(filepath.Join(root, "claude_desktop_config.json")); err != nil {
		return s, err
	}
	if raw, ok := s.mode["deploymentMode"]; ok {
		var mode string
		if json.Unmarshal(raw, &mode) != nil || (mode != "1p" && mode != "3p") {
			return s, errors.New("Claude 部署模式无法安全识别")
		}
		if mode == "3p" {
			s.Mode, s.RequiresModeChange = "third-party", false
		}
	}
	if s.meta, err = read3PObject(filepath.Join(root, "configLibrary", "_meta.json")); err != nil {
		return s, err
	}
	if len(s.meta) == 0 {
		if !s.RequiresModeChange {
			return s, errors.New("Claude 3P 配置库缺失；请先在原应用中修复")
		}
		return s, nil
	}
	if _, found := s.meta["hybridPointer"]; found {
		return s, errors.New("Claude 配置由组织连接管理，请在原应用中修改")
	}
	if json.Unmarshal(s.meta["appliedId"], &s.ProfileID) != nil || !desktop3PUUID.MatchString(s.ProfileID) {
		return s, errors.New("Claude 3P 已应用配置编号无效；未改写配置库")
	}
	var entries []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if json.Unmarshal(s.meta["entries"], &entries) != nil || len(entries) == 0 || len(entries) > 256 {
		return s, errors.New("Claude 3P 配置目录索引无效")
	}
	found := false
	for _, e := range entries {
		if !desktop3PUUID.MatchString(e.ID) {
			return s, errors.New("Claude 3P 配置索引包含无效编号")
		}
		if e.ID == s.ProfileID {
			found = true
		}
	}
	if !found {
		return s, errors.New("Claude 3P 已应用配置不在索引中")
	}
	s.profile, err = read3PObject(filepath.Join(root, "configLibrary", s.ProfileID+".json"))
	if err != nil {
		return s, err
	}
	if len(s.profile) == 0 {
		return s, errors.New("Claude 3P 已应用配置文件缺失或为空")
	}
	for key := range s.profile {
		if strings.HasPrefix(strings.ToLower(key), "bootstrap") || key == "selfHosted" || key == "selfHostedUrl" || key == "$sessionHostDeclared" {
			return s, errors.New("Claude 配置由服务端或组织管理，请在原应用中修改")
		}
	}
	_ = json.Unmarshal(s.profile["modelDiscoveryEnabled"], &s.ModelDiscovery)
	var models []json.RawMessage
	_ = json.Unmarshal(s.profile["inferenceModels"], &models)
	s.CatalogOverride = len(models) > 0 && !s.ModelDiscovery
	return s, nil
}

// PrepareClaude3P fixes a stable organization scope before changing a gateway
// URL. Otherwise Claude derives the chat scope from the URL and older local
// chats can disappear from the sidebar despite the files remaining intact.
func PrepareClaude3P(s Claude3PState, g Claude3PGateway, newID string, allowModeChange bool) (Claude3PWritePlan, error) {
	p := Claude3PWritePlan{State: s, ProfileID: s.ProfileID, ModePath: filepath.Join(s.Root, "claude_desktop_config.json"), MetaPath: filepath.Join(s.Root, "configLibrary", "_meta.json")}
	if s.RequiresModeChange && !allowModeChange {
		return p, errors.New("首次启用 Claude 第三方模式需要明确执行；原标准账号聊天仍留在原模式")
	}
	if g.Key == "" || strings.ContainsAny(g.Key, "\r\n\x00") || len(g.Key) > 8192 {
		return p, errors.New("API Key 无效")
	}
	u, err := url.Parse(g.BaseURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return p, errors.New("Claude Gateway API 地址无效")
	}
	if g.AuthMode != "bearer" && g.AuthMode != "api-key" {
		return p, errors.New("认证方式需要 Bearer 或 x-api-key")
	}
	profile := clone3P(s.profile)
	for _, key := range []string{"inferenceCustomHeaders", "inferenceGatewayHeaders"} {
		if raw := profile[key]; len(raw) > 0 {
			var headers map[string]string
			if json.Unmarshal(raw, &headers) != nil {
				return p, errors.New("原 Claude 自定义请求头无法安全检查，请在原应用中修复")
			}
			for name := range headers {
				if strings.EqualFold(name, "authorization") || strings.EqualFold(name, "x-api-key") {
					return p, errors.New("原 Claude 请求头包含认证字段，请先在原应用中移除冲突")
				}
			}
		}
	}
	if p.ProfileID == "" {
		if !desktop3PUUID.MatchString(newID) {
			return p, errors.New("新的 Claude 3P 配置编号无效")
		}
		p.ProfileID = newID
		profile["deploymentOrganizationUuid"] = raw3P(newID)
		meta := clone3P(s.meta)
		meta["appliedId"] = raw3P(newID)
		meta["entries"] = raw3P([]map[string]string{{"id": newID, "name": "Salcara Bridge"}})
		p.MetaData = marshal3P(meta)
	} else {
		var org string
		_ = json.Unmarshal(profile["deploymentOrganizationUuid"], &org)
		org = strings.TrimSpace(org)
		if !desktop3PUUID.MatchString(strings.ToLower(org)) || strings.ToLower(org) == "00000000-0000-4000-8000-000000000001" {
			var provider, base, kind string
			_ = json.Unmarshal(profile["inferenceProvider"], &provider)
			_ = json.Unmarshal(profile["inferenceGatewayBaseUrl"], &base)
			_ = json.Unmarshal(profile["inferenceCredentialKind"], &kind)
			if provider != "gateway" || (kind != "" && kind != "static") {
				return p, errors.New("原 Claude 3P 聊天范围无法安全保持，请在原应用中更改该配置")
			}
			if kind == "" {
				for _, field := range []string{"inferenceGatewayOidc", "inferenceIdpOidc", "inferenceCredentialHelper", "inferenceCredentialHelperWindows"} {
					if raw, exists := profile[field]; exists && string(raw) != "null" && string(raw) != `""` {
						return p, errors.New("原 Claude 凭据方式无法安全确定聊天范围，请在原应用中更改配置")
					}
				}
			}
			org, err = gatewayScope3P(base)
			if err != nil {
				return p, errors.New("原 Claude Gateway 地址无法安全识别")
			}
			profile["deploymentOrganizationUuid"] = raw3P(org)
		}
	}
	p.ProfilePath = filepath.Join(s.Root, "configLibrary", p.ProfileID+".json")
	profile["inferenceProvider"] = raw3P("gateway")
	profile["inferenceGatewayBaseUrl"] = raw3P(strings.TrimRight(g.BaseURL, "/"))
	profile["inferenceGatewayApiKey"] = raw3P(g.Key)
	profile["inferenceCredentialKind"] = raw3P("static")
	scheme := "bearer"
	if g.AuthMode == "api-key" {
		scheme = "x-api-key"
	}
	profile["inferenceGatewayAuthScheme"] = raw3P(scheme)
	if err := applyClaude3PCatalog(profile, g); err != nil {
		return p, err
	}
	p.ProfileData = marshal3P(profile)
	if s.RequiresModeChange {
		mode := clone3P(s.mode)
		mode["deploymentMode"] = raw3P("3p")
		p.ModeData = marshal3P(mode)
	}
	return p, nil
}

func applyClaude3PCatalog(profile map[string]json.RawMessage, g Claude3PGateway) error {
	var previous []json.RawMessage
	if raw, present := profile["inferenceModels"]; present && json.Unmarshal(raw, &previous) != nil {
		return errors.New("原 Claude 模型目录无法安全检查，请在原应用中修复")
	}
	// Disabling the override preserves the original picker and all its metadata.
	// A fresh/empty picker uses native discovery instead of inventing a default.
	if !g.CatalogOverride && len(previous) > 0 {
		return nil
	}
	if !g.CatalogOverride {
		delete(profile, "inferenceModels")
		profile["modelDiscoveryEnabled"] = raw3P(true)
		return nil
	}
	available := map[string]bool{}
	for _, model := range g.Models {
		if !ValidClaudeDesktopModel(model) {
			return errors.New("上游模型目录包含无效名称，未覆盖 Claude 模型菜单")
		}
		available[model] = true
	}
	if len(available) > ClaudeDesktop3PCatalogLimit {
		return errors.New("模型目录超过 Salcara 的 1000 项安全上限，未截断或覆盖菜单")
	}
	entries := map[string]Claude3PModelEntry{}
	for _, entry := range g.CatalogEntries {
		if !available[entry.Name] || (entry.LabelOverride != "" && !ValidClaudeDesktopModel(entry.LabelOverride)) {
			return errors.New("Claude 模型显示目录与当前 API 目录不匹配")
		}
		if entry.MaxEffort != "" && entry.MaxEffort != "low" && entry.MaxEffort != "medium" && entry.MaxEffort != "high" && entry.MaxEffort != "max" {
			return errors.New("Claude 模型目录能力字段无效")
		}
		if _, duplicate := entries[entry.Name]; duplicate {
			return errors.New("Claude 模型显示目录包含重复条目")
		}
		entries[entry.Name] = entry
	}
	if len(available) == 0 {
		delete(profile, "inferenceModels")
		profile["modelDiscoveryEnabled"] = raw3P(true)
		return nil
	}
	models, seen := []json.RawMessage{}, map[string]bool{}
	appendModel := func(name string, raw json.RawMessage) {
		if !available[name] || seen[name] {
			return
		}
		if override, ok := entries[name]; ok {
			entry := map[string]json.RawMessage{}
			_ = json.Unmarshal(raw, &entry)
			if entry == nil {
				entry = map[string]json.RawMessage{}
			}
			entry["name"] = raw3P(name)
			entry["labelOverride"] = raw3P(override.LabelOverride)
			entry["supports1m"] = raw3P(override.Supports1M)
			entry["prefer1m"] = raw3P(override.Prefer1M)
			if override.MaxEffort != "" {
				entry["maxEffort"] = raw3P(override.MaxEffort)
			} else {
				delete(entry, "maxEffort")
			}
			raw = raw3P(entry)
		}
		models, seen[name] = append(models, raw), true
	}
	// Retain previous picker ordering/metadata only for routes this Key still
	// advertises. Legacy g.Model metadata never grants access or forces selection.
	for _, raw := range previous {
		var name string
		var entry map[string]json.RawMessage
		if json.Unmarshal(raw, &name) != nil && json.Unmarshal(raw, &entry) == nil {
			_ = json.Unmarshal(entry["name"], &name)
		}
		appendModel(name, raw)
	}
	for _, model := range g.Models {
		appendModel(model, raw3P(map[string]string{"name": model}))
	}
	profile["inferenceModels"] = raw3P(models)
	profile["modelDiscoveryEnabled"] = raw3P(false)
	return nil
}

func (p Claude3PWritePlan) Paths() []string { return []string{p.ModePath, p.MetaPath, p.ProfilePath} }
func (p Claude3PWritePlan) Apply() error {
	// Publish the profile before its index/mode so a partially failed transaction
	// cannot leave the app pointing at an absent credential file.
	for _, f := range []struct {
		path string
		data []byte
	}{{p.ProfilePath, p.ProfileData}, {p.MetaPath, p.MetaData}, {p.ModePath, p.ModeData}} {
		if f.data != nil {
			if err := WriteClaude3PFile(f.path, f.data); err != nil {
				return err
			}
		}
	}
	return nil
}

func ValidateClaude3PPath(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("Claude 配置路径不是绝对路径")
	}
	for at := filepath.Clean(path); ; at = filepath.Dir(at) {
		info, err := os.Lstat(at)
		if err != nil && !os.IsNotExist(err) {
			return errors.New("无法安全检查 Claude 配置路径")
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return errors.New("Claude 配置路径存在符号链接，未写入")
		}
		parent := filepath.Dir(at)
		if parent == at {
			break
		}
	}
	return nil
}

func ReadClaude3PFile(path string) ([]byte, bool, error) {
	return ReadClaude3PFileLimit(path, 1<<20)
}

// The adapter's own backup can hold six bounded configuration snapshots; never
// use this larger limit to inspect arbitrary app-owned configuration files.
func ReadClaude3PFileLimit(path string, limit int64) ([]byte, bool, error) {
	if err := ValidateClaude3PPath(path); err != nil {
		return nil, false, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errors.New("无法读取 Claude 配置文件")
	}
	if !info.Mode().IsRegular() || info.Size() > limit || limit > 10<<20 {
		return nil, false, errors.New("Claude 配置文件类型或大小异常")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, errors.New("无法读取 Claude 配置文件")
	}
	return data, true, nil
}

func WriteClaude3PFile(path string, data []byte) error {
	if err := ValidateClaude3PPath(path); err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return errors.New("无法创建 Claude 配置目录")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".salcara-3p-*")
	if err != nil {
		return errors.New("无法准备 Claude 配置写入")
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(mode); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		return errors.New("Claude 配置写入失败")
	}
	return nil
}

func read3PObject(path string) (map[string]json.RawMessage, error) {
	data, exists, err := ReadClaude3PFile(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		return map[string]json.RawMessage{}, nil
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil || root == nil {
		return nil, errors.New("Claude 配置不是有效 JSON 对象；未覆盖原文件")
	}
	return root, nil
}
func clone3P(in map[string]json.RawMessage) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, v := range in {
		out[k] = append(json.RawMessage{}, v...)
	}
	return out
}
func raw3P(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
func marshal3P(v any) []byte      { b, _ := json.MarshalIndent(v, "", "  "); return append(b, '\n') }

func gatewayScope3P(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" {
		return "", errors.New("invalid base")
	}
	ns, _ := hex.DecodeString("a47e2c1f5b4a4c7e9d8f3e6a1b2c4d5e")
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	for _, r := range host {
		if r > 127 {
			return "", errors.New("non-ASCII host requires native configuration")
		}
	}
	path := u.EscapedPath()
	for _, segment := range strings.Split(path, "/") {
		decoded, _ := url.PathUnescape(segment)
		if decoded == "." || decoded == ".." {
			return "", errors.New("noncanonical path requires native configuration")
		}
	}
	h := sha1.New()
	_, _ = h.Write(ns)
	_, _ = h.Write([]byte("gateway:" + host + strings.TrimRight(path, "/") + "|"))
	out := h.Sum(nil)[:16]
	out[6] = out[6]&15 | 80
	out[8] = out[8]&63 | 128
	hexID := hex.EncodeToString(out)
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexID[:8], hexID[8:12], hexID[12:16], hexID[16:20], hexID[20:]), nil
}
