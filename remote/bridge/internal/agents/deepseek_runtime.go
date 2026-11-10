package agents

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The supported official npm SDK declares engines.node >=24.9.0. This is not
// a dependency of standalone native dsh binaries or the bundled remote core.
const deepSeekMinimumNode = "24.9.0"

var deepSeekNodeVersionRE = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// DeepSeekCommand resolves and checks the runtime shared by local and remote
// launches. It does not start dsh or attach the request context to its lifetime:
// remote ownership/stop fences and the local launcher manage that separately.
func DeepSeekCommand(ctx context.Context, exe string, args ...string) (*exec.Cmd, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	exe, err := filepath.Abs(exe)
	if err != nil {
		return nil, errors.New("DeepSeek Harness 程序路径无效")
	}
	entry, node, err := deepSeekNodeEntry(exe)
	if err != nil {
		return nil, err
	}
	if entry == "" {
		return exec.Command(exe, args...), nil
	}
	if node == "" {
		var ok bool
		node, ok = findExecutable("node", "")
		if !ok {
			return nil, fmt.Errorf("DeepSeek Harness 的 npm 安装需要 Node.js %s 或更高版本，请安装后重试", deepSeekMinimumNode)
		}
	}
	node, err = filepath.Abs(node)
	if err != nil {
		return nil, errors.New("无法解析 DeepSeek Harness 使用的 Node.js 路径")
	}
	if err := checkDeepSeekNode(ctx, node); err != nil {
		return nil, err
	}
	return exec.Command(node, append([]string{entry}, args...)...), nil
}

// Keep the internal helper used by the opt-in official codec fixture.
func deepSeekCommand(exe string, args ...string) (*exec.Cmd, error) {
	return DeepSeekCommand(context.Background(), exe, args...)
}

func deepSeekNodeEntry(exe string) (entry, node string, err error) {
	st, err := os.Stat(exe)
	if err != nil || !st.Mode().IsRegular() {
		return "", "", errors.New("未找到 DeepSeek Harness；请先安装或检查程序路径")
	}
	ext := strings.ToLower(filepath.Ext(exe))
	if ext == ".cmd" || ext == ".bat" {
		for _, candidate := range []string{
			filepath.Join(filepath.Dir(exe), "node_modules", "@deepseek-ai", "dsh", "lib", "bin.js"),
			filepath.Join(filepath.Dir(exe), "..", "@deepseek-ai", "dsh", "lib", "bin.js"),
		} {
			if info, e := os.Stat(candidate); e == nil && info.Mode().IsRegular() {
				return filepath.Clean(candidate), "", nil
			}
		}
		return "", "", errors.New("无法解析 dsh npm 入口，请选择原生可执行文件或重新安装")
	}
	// POSIX npm's extensionless bin link is not a native executable.
	if resolved, e := filepath.EvalSymlinks(exe); e == nil {
		exe = resolved
	}
	ext = strings.ToLower(filepath.Ext(exe))
	if ext == ".js" || ext == ".mjs" {
		return exe, "", nil
	}
	// Recognize only explicit Node shebangs, reading at most one small prefix.
	// Native binaries and unrelated wrapper formats never gain a Node dependency.
	f, err := os.Open(exe)
	if err != nil {
		return "", "", errors.New("无法读取 DeepSeek Harness 程序，请检查安装")
	}
	defer f.Close()
	prefix, err := io.ReadAll(io.LimitReader(f, 512))
	if err != nil {
		return "", "", errors.New("无法读取 DeepSeek Harness 程序，请检查安装")
	}
	line, _, _ := strings.Cut(string(prefix), "\n")
	if !strings.HasPrefix(line, "#!") {
		return "", "", nil
	}
	parts := strings.Fields(strings.TrimPrefix(line, "#!"))
	if len(parts) == 2 && filepath.Base(parts[0]) == "env" && parts[1] == "node" ||
		len(parts) == 3 && filepath.Base(parts[0]) == "env" && parts[1] == "-S" && parts[2] == "node" {
		return exe, "", nil
	}
	if len(parts) == 1 && filepath.IsAbs(parts[0]) && strings.EqualFold(filepath.Base(parts[0]), "node") {
		return exe, parts[0], nil
	}
	return "", "", nil
}

func checkDeepSeekNode(ctx context.Context, node string) error {
	output, err := deepSeekVersionProbe(ctx, node, []string{"--version"}, 3*time.Second)
	if err != nil {
		return fmt.Errorf("无法验证 DeepSeek Harness 使用的 Node.js 版本，需要 %s 或更高版本：%w", deepSeekMinimumNode, err)
	}
	version := strings.TrimSpace(output)
	parts := deepSeekNodeVersionRE.FindStringSubmatch(version)
	if len(parts) != 4 {
		return fmt.Errorf("无法识别 Node.js 正式版本，需要 %s 或更高版本，请检查 Node.js 安装", deepSeekMinimumNode)
	}
	major, e1 := strconv.Atoi(parts[1])
	minor, e2 := strconv.Atoi(parts[2])
	_, e3 := strconv.Atoi(parts[3])
	if e1 != nil || e2 != nil || e3 != nil {
		return errors.New("无法识别 Node.js 版本，请检查 Node.js 安装")
	}
	if major < 24 || major == 24 && minor < 9 {
		return fmt.Errorf("DeepSeek Harness 需要 Node.js %s 或更高版本，当前为 %s，请升级后重试", deepSeekMinimumNode, version)
	}
	return nil
}

// CheckDeepSeekRuntime verifies availability without launching a session. Its
// errors are safe fixed diagnostics, never raw command output or credentials.
func CheckDeepSeekRuntime(ctx context.Context, exe string) error {
	_, err := deepSeekRuntimeVersion(ctx, exe)
	return err
}

func deepSeekVersion(ctx context.Context, exe string) (string, bool) {
	version, err := deepSeekRuntimeVersion(ctx, exe)
	return version, err == nil
}

type deepSeekVersionEntry struct {
	fingerprint, version string
	at                   time.Time
}

var deepSeekRuntimeVersions = struct {
	sync.Mutex
	entries map[string]deepSeekVersionEntry
}{entries: map[string]deepSeekVersionEntry{}}

func deepSeekRuntimeVersion(ctx context.Context, exe string) (string, error) {
	// Validate the actual Node on every observation, before consulting any dsh
	// cache. A stable npm shim does not mean PATH/Node/dependencies stayed stable.
	cmd, err := DeepSeekCommand(ctx, exe, "--version")
	if err != nil {
		return "", err
	}
	paths := []string{cmd.Path}
	if len(cmd.Args) == 3 {
		paths = append(paths, cmd.Args[1])
	}
	key := strings.Join(paths, "\x00")
	fingerprint, err := deepSeekRuntimeFingerprint(paths)
	if err != nil {
		return "", err
	}
	deepSeekRuntimeVersions.Lock()
	cached, hit := deepSeekRuntimeVersions.entries[key]
	deepSeekRuntimeVersions.Unlock()
	if hit && cached.fingerprint == fingerprint && time.Since(cached.at) < 30*time.Second {
		return cached.version, ctx.Err()
	}
	output, err := deepSeekVersionProbe(ctx, cmd.Path, cmd.Args[1:], 15*time.Second)
	if err != nil {
		return "", fmt.Errorf("DeepSeek Harness 版本检查失败，请检查工具安装和运行环境：%w", err)
	}
	version := versionRe.FindString(output)
	if version == "" || len(version) > 80 {
		return "", errors.New("DeepSeek Harness 未返回有效版本，请检查工具安装")
	}
	after, err := deepSeekRuntimeFingerprint(paths)
	if err != nil || fingerprint != after {
		return "", errors.New("DeepSeek Harness 运行环境在检查期间发生变化，请重试")
	}
	deepSeekRuntimeVersions.Lock()
	// Bound the process-local cache even if custom paths change frequently.
	if len(deepSeekRuntimeVersions.entries) >= 64 {
		clear(deepSeekRuntimeVersions.entries)
	}
	deepSeekRuntimeVersions.entries[key] = deepSeekVersionEntry{fingerprint: fingerprint, version: version, at: time.Now()}
	deepSeekRuntimeVersions.Unlock()
	return version, nil
}

func deepSeekRuntimeFingerprint(paths []string) (string, error) {
	var fingerprint strings.Builder
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return "", errors.New("DeepSeek Harness 运行环境不可用，请检查安装")
		}
		fmt.Fprintf(&fingerprint, "%d:%d:%d;", info.ModTime().UnixNano(), info.Size(), info.Mode())
	}
	return fingerprint.String(), nil
}

type deepSeekProbeOutput struct {
	buffer   bytes.Buffer
	overflow bool
}

func (b *deepSeekProbeOutput) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 4096 - b.buffer.Len(); n > remaining {
		p = p[:remaining]
		b.overflow = true
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}

func deepSeekVersionProbe(ctx context.Context, exe string, args []string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	prepareCmd(cmd)
	cmd.Env = childEnv(exe, nil, []string{"NODE_OPTIONS", "ELECTRON_RUN_AS_NODE", "SALCARA_DSH_API_KEY", "DEEPSEEK_API_KEY", "DEEPSEEK_BASE_URL"})
	cmd.WaitDelay = 250 * time.Millisecond
	var output deepSeekProbeOutput
	cmd.Stdout, cmd.Stderr = &output, io.Discard
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil || output.overflow {
		return "", errors.New("版本检查未成功完成")
	}
	return output.buffer.String(), nil
}
