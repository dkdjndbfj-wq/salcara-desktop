package launcher

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

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

// ApplyDefault is explicitly requested by the user, never a side-effect of launching.
func (s *Service) ApplyDefault(a config.LocalAccount) error {
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
		dir := filepath.Dir(toolcfg.CodexConfigPath())
		paths = []string{filepath.Join(dir, "config.toml"), filepath.Join(dir, "auth.json")}
	}
	manifest := s.snapshotPath(a.Kind)
	var files []savedFile
	b, err := os.ReadFile(manifest)
	if err == nil {
		if json.Unmarshal(b, &files) != nil {
			return errors.New("原配置备份损坏，请先检查本地备份目录")
		}
		// Environment/config paths can change between launches. Never restore another directory.
		if len(files) != len(paths) {
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
		for _, path := range paths {
			data, e := os.ReadFile(path)
			if e != nil && !os.IsNotExist(e) {
				return e
			}
			files = append(files, savedFile{Path: path, Existed: e == nil, Data: data})
		}
		if err := saveSnapshot(manifest, files); err != nil {
			return err
		}
	} else {
		return err
	}
	_, v1, _ := config.APIBase(a.BaseURL)
	var applyErr error
	if a.Kind == "codex" {
		applyErr = toolcfg.ApplySwitchCodex(filepath.Dir(paths[0]), v1, a.Key, a.Model)
	} else {
		applyErr = toolcfg.ApplyLocalClaude(paths[0], a.BaseURL, a.Key, a.Model, a.AuthMode)
	}
	for i := range files {
		data, e := os.ReadFile(files[i].Path)
		if e == nil {
			files[i].AppliedHash = digest(data)
		}
	}
	if err := saveSnapshot(manifest, files); err != nil {
		return err
	}
	return applyErr
}

func (s *Service) RestoreDefault(kind string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind != "codex" && kind != "claude" && kind != "claude-desktop" {
		return errors.New("无效的工具")
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
