package launcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"salcara/bridge/internal/atomicfile"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/toolcfg"
)

type savedFile struct {
	Path              string `json:"path"`
	Existed           bool   `json:"existed"`
	Data              []byte `json:"data,omitempty"`
	AppliedHash       string `json:"appliedHash,omitempty"`
	ChangedExternally bool   `json:"changedExternally,omitempty"`
}

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func (s *Service) snapshotPath(kind string) string {
	return filepath.Join(s.Dir, "default-backups", kind+".json")
}

func codexDefaultPaths() []string {
	dir := filepath.Dir(toolcfg.CodexConfigPath())
	return []string{
		filepath.Join(dir, "config.toml"),
		filepath.Join(dir, "auth.json"),
		toolcfg.CodexCatalogPath(dir),
	}
}

// migrateLegacyCodexSnapshot upgrades the pre-catalog backup shape (config +
// auth) without guessing the original model catalogue.  A profile that
// already pointed at Bridge's catalogue cannot be safely reconstructed, so it
// is rejected until the user checks the backup instead of overwriting it.
func migrateLegacyCodexSnapshot(files, current []savedFile) ([]savedFile, error) {
	if len(files) == len(current) {
		return files, nil
	}
	if len(files) != 2 || len(current) != 3 {
		return nil, errors.New("默认配置路径已改变，请检查原配置备份")
	}
	if toolcfg.HasCodexCatalogPointer(string(current[0].Data), filepath.Dir(current[0].Path)) {
		return nil, errors.New("旧的 Codex 模型目录备份不完整，未安全覆盖；请先手动检查或删除旧备份")
	}
	catalog := current[2]
	if catalog.Existed {
		// A two-file manifest predates Bridge-owned catalog tracking.  An
		// existing unmarked file may be user-owned; never silently overwrite it
		// or claim that it can be restored exactly.
		return nil, errors.New("旧的 Codex 配置备份缺少模型目录所有权信息，未安全覆盖")
	}
	return append(append([]savedFile(nil), files...), catalog), nil
}

// ApplyDefault is explicitly requested by the user, never a side-effect of launching.
func (s *Service) ApplyDefault(a config.LocalAccount) error {
	if config.IsAdditionalTool(a.Kind) {
		return errors.New("请从 Agent 卡片使用「仅应用配置」")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := config.ValidateLocalAccount(&a); err != nil {
		return err
	}
	if a.Model == "" {
		return errors.New("请先为账号选择默认模型")
	}
	paths := []string{toolcfg.ClaudeSettingsPath()}
	if a.Kind == "codex" {
		paths = codexDefaultPaths()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		release, err := toolcfg.LockCodexProfile(ctx, filepath.Dir(paths[0]))
		if err != nil {
			return err
		}
		defer release()
		if err := toolcfg.RequireCodexProfileUnowned(filepath.Dir(paths[0])); err != nil {
			return err
		}
	}
	manifest := s.snapshotPath(a.Kind)
	current, currentErr := captureFiles(paths)
	if currentErr != nil {
		return currentErr
	}
	var files []savedFile
	b, err := os.ReadFile(manifest)
	if err == nil {
		if json.Unmarshal(b, &files) != nil {
			return errors.New("原配置备份损坏，请先检查本地备份目录")
		}
		// Environment/config paths can change between launches. Never restore another directory.
		files, err = migrateLegacyCodexSnapshot(files, current)
		if err != nil {
			return errors.New("默认配置路径已改变，请先恢复上一份配置")
		}
		for i, path := range paths {
			if files[i].Path != path {
				return errors.New("默认配置路径已改变，请先恢复上一份配置")
			}
			if files[i].AppliedHash != "" {
				current, e := os.ReadFile(path)
				if e != nil || digest(current) != files[i].AppliedHash {
					return errors.New("系统配置在上次应用后被修改，已停止覆盖。请检查 default-backups 备份或使用独立启动")
				}
			}
		}
	} else if os.IsNotExist(err) {
		files = append([]savedFile(nil), current...)
		for i := range files {
			if files[i].Existed {
				files[i].AppliedHash = digest(files[i].Data)
			}
		}
		if err := saveSnapshot(manifest, files); err != nil {
			return err
		}
	} else {
		return err
	}
	_, v1, _ := config.APIBase(a.BaseURL)
	rollback := func(cause error) error {
		if rollbackErr := restoreFiles(current); rollbackErr != nil {
			return fmt.Errorf("应用默认配置失败且回滚失败：%w（原错误：%v）", rollbackErr, cause)
		}
		return cause
	}
	var applyErr error
	if a.Kind == "codex" {
		applyErr = toolcfg.ApplySwitchCodex(filepath.Dir(paths[0]), v1, a.Key, a.Model)
	} else {
		applyErr = toolcfg.ApplyLocalClaude(paths[0], a.BaseURL, a.Key, a.Model, a.AuthMode)
	}
	if applyErr != nil {
		return rollback(applyErr)
	}
	for i := range files {
		data, e := os.ReadFile(files[i].Path)
		if e == nil {
			files[i].AppliedHash = digest(data)
		} else if !(os.IsNotExist(e) && !files[i].Existed) {
			return rollback(e)
		}
	}
	if err := saveSnapshot(manifest, files); err != nil {
		return rollback(err)
	}
	return applyErr
}

func (s *Service) RestoreDefault(kind string) error {
	return s.RestoreDefaultAuthorized(kind, nil)
}

// RestoreDefaultAuthorized retires authorization after all restore checks and
// before the first native profile write. The hook runs while the launch mutex
// is held, so callers must read the current binding here, rather than before
// waiting for a switch to finish. A later write failure never restores a lease.
func (s *Service) RestoreDefaultAuthorized(kind string, beforeRestore func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind != "codex" && kind != "claude" && kind != "claude-desktop" && !config.IsAdditionalTool(kind) {
		return errors.New("无效的工具")
	}
	if kind == "codex" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		dir := filepath.Dir(toolcfg.CodexConfigPath())
		release, err := toolcfg.LockCodexProfile(ctx, dir)
		if err != nil {
			return err
		}
		defer release()
		if err := toolcfg.RequireCodexProfileUnowned(dir); err != nil {
			return err
		}
	}
	manifest := s.snapshotPath(kind)
	b, err := os.ReadFile(manifest)
	if os.IsNotExist(err) {
		return errors.New("没有通过本地账号应用过默认配置，无需恢复")
	}
	if err != nil {
		return err
	}
	var files []savedFile
	if json.Unmarshal(b, &files) != nil || len(files) == 0 {
		return errors.New("配置备份无法读取")
	}
	paths := []string{toolcfg.ClaudeSettingsPath()}
	if config.IsAdditionalTool(kind) {
		paths = toolcfg.AdditionalPaths(kind, toolcfg.AdditionalHome(kind))
	}
	if kind == "codex" {
		paths = codexDefaultPaths()
	} else if kind == "claude-desktop" {
		root := originalAppData(Tool{ID: kind, Kind: "claude"})
		paths = []string{filepath.Join(root, "config.json"), filepath.Join(root, "configLibrary", "_meta.json"), filepath.Join(root, "configLibrary", toolcfg.SwitchClaudeProviderID+".json")}
	}
	current, err := captureFiles(paths)
	if err != nil {
		return err
	}
	files, err = migrateLegacyCodexSnapshot(files, current)
	if err != nil {
		return errors.New("默认配置路径已改变，请检查原配置备份，未恢复")
	}
	for i, path := range paths {
		if files[i].Path != path {
			return errors.New("默认配置路径已改变，请检查原配置备份，未恢复")
		}
	}
	// Verify every file before touching any: never overwrite subsequent user edits.
	for _, f := range files {
		if f.ChangedExternally {
			return errors.New("切号之间原工具配置被其他程序修改过，已停止自动恢复，避免丢掉新增设置；最初配置仍在 default-backups 中")
		}
		data, e := os.ReadFile(f.Path)
		if os.IsNotExist(e) && !f.Existed {
			continue
		}
		if e != nil {
			return e
		}
		if f.AppliedHash == "" || digest(data) != f.AppliedHash {
			return errors.New("配置在应用后被其他程序修改，已停止自动恢复。原始配置仍在 default-backups 中")
		}
	}
	if beforeRestore != nil {
		if err := beforeRestore(); err != nil {
			return err
		}
	}
	for _, f := range files {
		if f.Existed {
			if err := atomicfile.WriteFileKeepMode(f.Path, f.Data, 0o600); err != nil {
				return err
			}
		} else if err := os.Remove(f.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return os.Remove(manifest)
}

func saveSnapshot(path string, files []savedFile) error {
	b, err := json.Marshal(files)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(path, b, 0o600)
}
