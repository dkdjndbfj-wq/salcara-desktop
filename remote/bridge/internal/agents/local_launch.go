package agents

import "os/exec"

// LocalExecutable resolves the same installation paths as the agent runtime.
func LocalExecutable(name, override string) (string, bool) {
	p, ok := findExecutable(name, override)
	if ok && name == "codex" {
		if native := nativeCodexExe(p); native != "" {
			p = native
		}
	}
	return p, ok
}

func LocalEnvironment(exe string, set map[string]string, drop []string) []string {
	return childEnv(exe, set, drop)
}

// PrepareChild hides the console window on Windows and isolates the process
// group elsewhere, for helper processes such as tool installers.
func PrepareChild(cmd *exec.Cmd) { prepareCmd(cmd) }
