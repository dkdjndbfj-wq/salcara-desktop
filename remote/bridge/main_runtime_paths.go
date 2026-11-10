package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

type desktopRuntimePaths struct {
	coreExe, startupExe, companionDir, nodeExe string
}

// The desktop launcher and Bridge core live in different packaged directories.
// Only autostart and the application entry use the launcher; resources and
// subprocess helpers always belong to the running Bridge executable.
func resolveRuntimePaths(coreExe, desktopExe, companionOverride string, embedded bool) desktopRuntimePaths {
	p := desktopRuntimePaths{
		coreExe:      coreExe,
		startupExe:   coreExe,
		companionDir: filepath.Join(filepath.Dir(coreExe), "desktop-companion"),
		nodeExe:      filepath.Join(filepath.Dir(coreExe), "SalcaraProbeNode"),
	}
	if runtime.GOOS == "windows" {
		p.nodeExe += ".exe"
	}
	packaged := embedded && desktopExe != ""
	if packaged && filepath.IsAbs(desktopExe) {
		if st, err := os.Stat(desktopExe); err == nil && st.Mode().IsRegular() {
			p.startupExe = desktopExe
		}
	}
	if companionOverride != "" {
		p.companionDir = companionOverride
	}
	// Source checkouts and standalone cores may use a developer's Node install.
	// A packaged desktop must retain the bundled path when it is missing, so
	// companion readiness reports the damaged package instead of hiding it with
	// an unrelated system runtime (or asking users to install Node themselves).
	if !packaged {
		if _, err := os.Stat(p.nodeExe); err != nil {
			p.nodeExe, _ = exec.LookPath("node")
		}
	}
	return p
}
