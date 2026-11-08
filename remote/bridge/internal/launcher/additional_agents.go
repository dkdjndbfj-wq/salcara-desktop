package launcher

import (
	"os"
	"path/filepath"
	"runtime"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/toolcfg"
)

func hermesCandidates() []string {
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "windows" {
		return []string{filepath.Join(toolcfg.AdditionalHome("hermes"), "bin", "hermes.exe"), filepath.Join(toolcfg.AdditionalHome("hermes"), "hermes-agent", "venv", "Scripts", "hermes.exe"), filepath.Join(os.Getenv("LOCALAPPDATA"), "hermes", "hermes-agent", "venv", "Scripts", "hermes.exe"), filepath.Join(home, ".hermes", "hermes-agent", "venv", "Scripts", "hermes.exe")}
	}
	return []string{filepath.Join(home, ".local", "bin", "hermes"), filepath.Join(home, ".hermes", "hermes-agent", "venv", "bin", "hermes")}
}

func additionalPlan(p *Plan, a config.LocalAccount, env map[string]string) {
	if a.Kind == "deepseek-harness" {
		env["DSH_HOME"] = p.ProfileDir
		p.Args = []string{"web"}
	} else {
		env["HERMES_HOME"] = p.ProfileDir
		p.Args = []string{"chat"}
	}
}

func applyAdditional(p Plan, a config.LocalAccount) error {
	_, v1, err := config.APIBase(a.BaseURL)
	if err != nil {
		return err
	}
	return toolcfg.ApplyAdditional(a.Kind, p.ProfileDir, v1, a.Key, a.Model, catalogModels(a))
}
