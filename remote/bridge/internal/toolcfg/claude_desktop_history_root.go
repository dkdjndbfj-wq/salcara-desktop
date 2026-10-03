package toolcfg

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

const claudeHistoryPlaceholderOrg = "00000000-0000-4000-8000-000000000001"

var claudeHistoryStorageUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ResolveClaudeDesktopHistoryRoot establishes one current 3P storage namespace,
// not every account on this computer. Distribution 2.16120.0 evidence:
// index.chunk-Dp-z0Dv3.js kS/gVt: base64 UUID in userData/ant-did;
// AUt/accountIdOverride: install UUID (not hybrid person ID) is storage account;
// identity/WBe: explicit deploymentOrganizationUuid, otherwise placeholder;
// OD: full IDs or eight-character shortened IDs. Gateway-derived telemetry IDs
// are NOT storage identities and cannot be substituted here.
// No inference requests, mode changes, credential writes, or chat reads occur.
func ResolveClaudeDesktopHistoryRoot(userDataRoot string) (string, error) {
	// Validate the fixed config-library paths before inspecting profile content.
	// Junctions cannot redirect even those small credential-bearing files into
	// another account's tree before the history namespace is established.
	if _, err := claudeHistoryIdentityPath(userDataRoot, true); err != nil {
		return "", errors.New("Claude Desktop 当前配置目录无法安全识别")
	}
	for _, item := range []struct {
		path      string
		directory bool
	}{{"claude_desktop_config.json", false}, {"configLibrary", true}, {filepath.Join("configLibrary", "_meta.json"), false}} {
		if _, err := claudeHistoryIdentityPath(filepath.Join(userDataRoot, item.path), item.directory); err != nil {
			return "", errors.New("Claude Desktop 当前配置库无法安全识别")
		}
	}
	metaFile, metaStat, err := openClaudeHistoryIdentity(filepath.Join(userDataRoot, "configLibrary", "_meta.json"), false)
	if err != nil || metaStat.Size() > 1<<20 {
		if metaFile != nil {
			metaFile.Close()
		}
		return "", errors.New("Claude Desktop 当前配置索引无法安全识别")
	}
	metaData, err := io.ReadAll(io.LimitReader(metaFile, (1<<20)+1))
	metaFile.Close()
	var meta struct {
		AppliedID string `json:"appliedId"`
	}
	if err != nil || len(metaData) > 1<<20 || json.Unmarshal(metaData, &meta) != nil || !ValidClaudeDesktopProfileID(meta.AppliedID) {
		return "", errors.New("Claude Desktop 当前配置索引无法安全识别")
	}
	if _, err := claudeHistoryIdentityPath(filepath.Join(userDataRoot, "configLibrary", meta.AppliedID+".json"), false); err != nil {
		return "", errors.New("Claude Desktop 当前配置文件无法安全识别")
	}
	state, err := InspectClaude3P(userDataRoot)
	if err != nil {
		return "", errors.New("Claude Desktop 当前配置无法安全识别本地历史")
	}
	if state.Mode != "third-party" || state.RequiresModeChange || state.ProfileID == "" {
		return "", errors.New("Claude Desktop 本地 Chat/Cowork 历史仅在已配置的第三方模式可用")
	}
	// A remote/bootstrap overlay can override the current organization or user
	// data scope; InspectClaude3P already rejects profiles declaring those modes.
	for key := range state.mode {
		if strings.HasPrefix(strings.ToLower(key), "bootstrap") || key == "selfHosted" || key == "selfHostedUrl" {
			return "", errors.New("Claude Desktop 当前组织由外部管理，未读取历史")
		}
	}
	org := claudeHistoryPlaceholderOrg
	if raw, exists := state.profile["deploymentOrganizationUuid"]; exists {
		var configured string
		if strings.TrimSpace(string(raw)) == "null" || json.Unmarshal(raw, &configured) != nil {
			return "", errors.New("Claude Desktop 当前组织编号无效")
		}
		configured = strings.TrimSpace(configured)
		if configured != "" {
			if !desktop3PUUID.MatchString(strings.ToLower(configured)) {
				return "", errors.New("Claude Desktop 当前组织编号无效")
			}
			org = strings.ToLower(configured)
		}
	}
	path := filepath.Join(userDataRoot, "ant-did")
	f, st, err := openClaudeHistoryIdentity(path, false)
	if err != nil || st.Size() > 4096 {
		if f != nil {
			f.Close()
		}
		return "", errors.New("Claude Desktop 本地安装编号无法安全读取")
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	f.Close()
	if err != nil || len(data) > 4096 {
		return "", errors.New("Claude Desktop 本地安装编号无法安全读取")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	account := strings.TrimSpace(string(decoded))
	if err != nil || !claudeHistoryStorageUUID.MatchString(account) || strings.EqualFold(account, "00000000-0000-0000-0000-000000000000") {
		return "", errors.New("Claude Desktop 本地安装编号无效")
	}
	account = strings.ToLower(account)
	parent := filepath.Join(userDataRoot, "local-agent-mode-sessions")
	accountDir, err := selectClaudeHistoryScope(parent, account)
	if err != nil {
		return "", err
	}
	orgDir, err := selectClaudeHistoryScope(accountDir, org)
	if err != nil {
		return "", err
	}
	if (filepath.Base(accountDir) == account[:8]) != (filepath.Base(orgDir) == org[:8]) {
		return "", errors.New("Claude Desktop 账号目录布局与原生格式不匹配")
	}
	return orgDir, nil
}

func claudeHistoryIdentityPath(path string, directory bool) (os.FileInfo, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("Claude Desktop 本地历史路径不是绝对路径")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	same := filepath.Clean(resolved) == filepath.Clean(abs)
	if runtime.GOOS == "windows" {
		same = strings.EqualFold(filepath.Clean(resolved), filepath.Clean(abs))
	}
	if !same {
		return nil, errors.New("Claude Desktop 本地历史路径存在链接")
	}
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if st.Mode()&os.ModeSymlink != 0 || directory && !st.IsDir() || !directory && !st.Mode().IsRegular() {
		return nil, errors.New("Claude Desktop 本地历史路径类型无效")
	}
	return st, nil
}

func openClaudeHistoryIdentity(path string, directory bool) (*os.File, os.FileInfo, error) {
	before, err := claudeHistoryIdentityPath(path, directory)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	opened, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	after, err := claudeHistoryIdentityPath(path, directory)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !os.SameFile(before, opened) || !os.SameFile(opened, after) {
		f.Close()
		return nil, nil, errors.New("Claude Desktop 本地历史路径已变化")
	}
	return f, opened, nil
}

// Only directory NAMES are inspected to reject short-ID collisions. No record,
// credential, or nested directory belonging to another account is opened.
func selectClaudeHistoryScope(parent, id string) (string, error) {
	f, _, err := openClaudeHistoryIdentity(parent, true)
	if err != nil {
		return "", errors.New("Claude Desktop 当前本地历史目录不可用")
	}
	defer f.Close()
	short := id[:8]
	var fullFound, shortFound, collision bool
	matchName := func(name, want string) bool {
		if runtime.GOOS == "windows" {
			return strings.EqualFold(name, want)
		}
		return name == want
	}
	count := 0
	for {
		entries, err := f.ReadDir(128)
		count += len(entries)
		if count > 8192 {
			return "", errors.New("Claude Desktop 本地账号目录超过安全检查上限")
		}
		for _, entry := range entries {
			name := entry.Name()
			if matchName(name, id) {
				fullFound = true
			}
			if matchName(name, short) {
				shortFound = true
			}
			if claudeHistoryStorageUUID.MatchString(name) && strings.HasPrefix(strings.ToLower(name), short) && strings.ToLower(name) != id {
				// The collision only matters when the short directory exists; keep
				// scanning names before deciding, never open the other namespace.
				collision = true
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", errors.New("Claude Desktop 本地账号目录无法安全检查")
		}
	}
	if fullFound && shortFound {
		return "", errors.New("Claude Desktop 完整和缩短账号目录同时存在")
	}
	if shortFound && collision {
		return "", errors.New("Claude Desktop 缩短账号目录编号冲突")
	}
	if !fullFound && !shortFound {
		return "", errors.New("Claude Desktop 当前账号尚无本地历史目录")
	}
	selected := filepath.Join(parent, id)
	if shortFound {
		selected = filepath.Join(parent, short)
	}
	if _, err := claudeHistoryIdentityPath(selected, true); err != nil {
		return "", errors.New("Claude Desktop 当前账号历史路径无法安全识别")
	}
	return selected, nil
}
