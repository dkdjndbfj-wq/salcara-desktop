package agents

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

//go:embed hermes_sidecar.py
var hermesSidecar string

const hermesSidecarModule = "salcara_hermes_gateway_sidecar"

type hermesGateway interface {
	Call(context.Context, string, any, any) error
	alive() bool
	detach()
}

type hermesPipe struct{ *rpcConn }

// A Gateway may have been started by the installed client. Never kill its
// process tree: the shared authority owns other viewers' accepted work.
func (p *hermesPipe) detach() {
	_ = p.stdin.Close()
	select {
	case <-p.done:
		return
	case <-time.After(time.Second):
	}
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	} // this viewer only
}

// Resolve the exact installation's isolated Python/bootstrap through the
// official machine boundary; never guess Python from PATH or a virtualenv.
func hermesGatewayCommand(ctx context.Context, exe string) (*exec.Cmd, error) {
	if ext := strings.ToLower(filepath.Ext(exe)); ext == ".cmd" || ext == ".bat" {
		return nil, errors.New("Hermes 远程控制需要原生 Hermes 启动器，请更新或修复 Hermes 安装")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	probe := exec.CommandContext(probeCtx, exe, "--print-runtime-command", "--module", hermesSidecarModule)
	prepareCmd(probe)
	raw, err := probe.Output()
	if err != nil || len(raw) > 128<<10 {
		return nil, errors.New("无法读取 Hermes 官方运行时，请更新 Hermes 启动器")
	}
	return hermesCommandFromRuntime(raw)
}

func hermesCommandFromRuntime(raw []byte) (*exec.Cmd, error) {
	var args []string
	if json.Unmarshal(raw, &args) != nil || len(args) != 4 || !filepath.IsAbs(args[0]) || args[1] != "-I" || args[2] != "-c" {
		return nil, errors.New("Hermes 运行时启动契约不兼容，请更新 Hermes")
	}
	// Preserve the install's home pinning and dependency-generation bootstrap.
	// Replace only the explicitly requested terminal entry, never arbitrary code.
	entry := "runpy.run_module('" + hermesSidecarModule + "', run_name='__main__', alter_sys=True)"
	if strings.Count(args[3], entry) != 1 {
		return nil, errors.New("Hermes 运行时入口不兼容，请更新 Hermes")
	}
	script, _ := json.Marshal(hermesSidecar)
	args[3] = strings.Replace(args[3], entry, "exec("+string(script)+")", 1)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = childEnv(args[0], map[string]string{"PYTHONIOENCODING": "utf-8"}, []string{
		"HERMES_TUI_GATEWAY_URL", "PYTHONPATH", "PYTHONHOME", "HERMES_YOLO_MODE", "HERMES_IGNORE_RULES", "HERMES_SAFE_MODE", "HERMES_IGNORE_USER_CONFIG", "HERMES_ACCEPT_HOOKS",
	})
	prepareCmd(cmd)
	return cmd, nil
}
