// Command salcara-bridge is the Salcara Bridge desktop program: it keeps this computer online at the
// Salcara hub so the phone app can see and drive Codex / Claude Code here, and serves a local console on
// http://127.0.0.1:47831.
//
//	salcara-bridge                 run (single instance; if already running, just open the console)
//	salcara-bridge --background    run without opening the browser (used by autostart)
//	salcara-bridge --relay URL     pre-fill the relay address (install scripts)
//	salcara-bridge mcp-approval --port N --token T --session S   Claude Code permission helper
//	salcara-bridge version
//
// Environment overrides (tests, portable setups):
//
//	SALCARA_BRIDGE_PORT         console / local port instead of 47831
//	SALCARA_BRIDGE_CONFIG_DIR   folder for config.json, bridge.log and state instead of the user config dir
//	SALCARA_NO_SHELL_PATH=1     don't ask the login shell for PATH when looking for codex / claude
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/applog"
	"salcara/bridge/internal/autostart"
	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/codexruntime"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/console"
	"salcara/bridge/internal/desktopcompanion"
	"salcara/bridge/internal/desktoplink"
	"salcara/bridge/internal/hubclient"
	"salcara/bridge/internal/launcher"
	"salcara/bridge/internal/mcpapproval"
	"salcara/bridge/internal/protocol"
)

var version = "dev"

// startupNotes are messages from before the log file was open (macOS self-install).
var startupNotes []string

// prefillRelay is --relay: the install scripts pass the relay they were downloaded from.
var prefillRelay string

// DefaultPort is the fixed console / internal port (also the single-instance lock).
const DefaultPort = 47831

// LocalPort is DefaultPort unless SALCARA_BRIDGE_PORT overrides it.
var LocalPort = portFromEnv()

func portFromEnv() int {
	if v := os.Getenv("SALCARA_BRIDGE_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 && p < 65536 {
			return p
		}
	}
	return DefaultPort
}

// cleanArgs drops the "-psn_0_12345" process serial number older macOS versions pass to apps started from
// Finder; the flag parser would otherwise exit with "flag provided but not defined".
func cleanArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if strings.HasPrefix(a, "-psn_") {
			continue
		}
		out = append(out, a)
	}
	return out
}

func main() {
	os.Args = append(os.Args[:1], cleanArgs(os.Args[1:])...)
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "mcp-approval":
			fs := flag.NewFlagSet("mcp-approval", flag.ExitOnError)
			port := fs.Int("port", LocalPort, "bridge local port")
			token := fs.String("token", "", "bridge local token")
			session := fs.String("session", "", "session key")
			_ = fs.Parse(os.Args[2:])
			if err := mcpapproval.Run(*port, *token, *session); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			return
		case "version", "--version", "-v":
			fmt.Println("Salcara Bridge " + version)
			return
		}
	}
	fs := flag.NewFlagSet("salcara-bridge", flag.ExitOnError)
	background := fs.Bool("background", false, "start without opening the console")
	relayFlag := fs.String("relay", "", "pre-fill the relay address when not logged in yet (used by the install scripts)")
	_ = fs.Parse(os.Args[1:])
	prefillRelay = *relayFlag
	// macOS: install a downloaded .app into ~/Applications, and when Finder / LaunchServices opened the
	// .app, continue in a detached copy so the next double-click launches again (see main_darwin.go).
	if macStartup(*background) {
		return
	}
	if err := run(*background); err != nil {
		log.Printf("fatal: %v", err)
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func consoleURL() string { return "http://127.0.0.1:" + strconv.Itoa(LocalPort) + "/" }

// alreadyRunning reports whether another Salcara Bridge answers on the port.
func alreadyRunning() bool {
	c := &http.Client{Timeout: 2 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, consoleURL()+"healthz", nil)
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode == 200 && bytes.Contains(b, []byte("salcara-bridge"))
}

func run(background bool) error {
	// Single instance: the console port is the lock.
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(LocalPort))
	if err != nil {
		if alreadyRunning() {
			if !background {
				_ = autostart.OpenBrowser(consoleURL())
			}
			return nil
		}
		return fmt.Errorf("端口 %d 被其他程序占用：%w", LocalPort, err)
	}

	dir, err := configDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var logw io.Writer = os.Stderr
	lf, err := applog.Open(filepath.Join(dir, "bridge.log"))
	if err == nil {
		defer lf.Close()
		// file first: on Windows GUI builds stderr is invalid and MultiWriter stops at the first failing writer
		logw = io.MultiWriter(lf, os.Stderr)
	}
	logger := log.New(logw, "", log.LstdFlags)
	log.SetOutput(logw)
	logger.Printf("Salcara Bridge %s starting (background=%v, %s/%s)", version, background, runtime.GOOS, runtime.GOARCH)
	go agents.SearchPath() // warm the login-shell PATH probe (up to 3s) before the first tool detection
	for _, n := range startupNotes {
		logger.Printf("startup: %s", n)
	}
	if from := os.Getenv("SALCARA_INSTALLED_FROM"); from != "" {
		logger.Printf("installed to %s from %s", autostart.Executable(), from)
	}

	store, err := config.Open(filepath.Join(dir, "config.json"))
	if err != nil {
		return err
	}
	// Arm a fresh in-memory gateway lease for every Bridge process. A Codex or
	// Claude process started before Salcara (or holding a previous lease after a
	// reboot) must not be able to reach the local adapter automatically.
	store.SetRuntimeGatewayKey("rt-salcara-" + config.RandomToken(32))
	if r := config.NormalizeRelayRoot(prefillRelay); r != "" && strings.HasPrefix(r, "http") && store.Get().RelayRoot == "" {
		_ = store.Update(func(c *config.Config) error { c.RelayRoot = r; return nil })
		logger.Printf("relay address pre-filled: %s", r)
	}
	exe := autostart.Executable()
	// Inside the desktop app, start at login means starting the app itself (it
	// runs in the background with its tray and floating ball and owns this core),
	// not this headless core alone.
	if desktop := os.Getenv("SALCARA_DESKTOP_EXE"); os.Getenv("SALCARA_EMBEDDED_WINDOW") == "1" && filepath.IsAbs(desktop) {
		if st, err := os.Stat(desktop); err == nil && !st.IsDir() {
			exe = desktop
		}
	}
	localToken := config.RandomToken(24)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigc
		cancel()
	}()

	bundleDir := filepath.Join(filepath.Dir(exe), "desktop-companion")
	if configured := os.Getenv("SALCARA_DESKTOP_COMPANION_DIR"); configured != "" {
		bundleDir = configured
	}
	nodePath := filepath.Join(filepath.Dir(exe), "SalcaraProbeNode")
	if runtime.GOOS == "windows" {
		nodePath += ".exe"
	}
	if _, err := os.Stat(nodePath); err != nil {
		nodePath, _ = exec.LookPath("node")
	}
	companion := desktopcompanion.New(desktopcompanion.Options{
		BundleDir: bundleDir,
		DataDir:   dir,
		NodePath:  nodePath,
		// Native Codex Desktop control is admitted only while Salcara has an
		// explicit Codex source selected.  The identity is opaque and contains
		// no API key; the fingerprint lets the companion reject a lease bound
		// to a different provider/account after a restart or API switch.
		NativeSourceIdentity: func() string {
			return store.Get().NativeDesktopSourceIdentity()
		},
		RequireManagedLaunch: true,
	})
	localLauncher := launcher.New(dir)
	claudeDesktop := launcher.NewClaudeDesktop3P(localLauncher)
	var cons *console.Server
	hub := hubclient.New(hubclient.Options{
		Store:   store,
		Version: version,
		Logger:  logger,
		Desktop: companion,
		// Agent status is requested whenever the remote workbench opens. Use the
		// launcher service's bounded, coalesced inventory instead of the raw
		// package scan; otherwise every phone refresh can rescan Windows installs.
		DiscoverTools: func(ctx context.Context, paths map[string]string) []launcher.Tool {
			return localLauncher.Inventory(ctx, paths)
		},
		ClaudeDesktopHistory: func(ctx context.Context) (hubclient.ReadOnlyDesktopHistory, error) {
			return claudeDesktop.ReadOnlyHistory(ctx, store.Get().LocalToolPaths)
		},
		NavigateDesktop: func(ctx context.Context, key string) error {
			want, open, missing := "codex-desktop", desktoplink.OpenCodex, "没有检测到 Codex Desktop；本操作不会新建 CLI 会话"
			if desktoplink.ClaudeKey(key) {
				imported := hubclient.ImportRequested(ctx)
				want, missing = "claude-desktop", "没有检测到 Claude Desktop"
				open = func(k string) error { return desktoplink.OpenClaude(k, imported) }
			}
			for _, tool := range localLauncher.Inventory(ctx, store.Get().LocalToolPaths) {
				if tool.ID == want && tool.Available {
					return open(key)
				}
			}
			return errors.New(missing)
		},
		OnStatus: func(st hubclient.Status) {
			if cons != nil {
				cons.BroadcastStatus(st)
			}
		},
	})
	accountRuntime := codexruntime.New(dir)
	defer accountRuntime.Close()
	var officialLogin codexaccounts.LoginConnector
	browserLogin, browserLoginErr := codexaccounts.NewBrowserConnector(codexaccounts.BrowserOptions{OpenBrowser: autostart.OpenBrowser})
	if browserLoginErr != nil {
		logger.Printf("official account browser login unavailable")
	} else {
		officialLogin = browserLogin
	}
	var officialDeviceLogin codexaccounts.LoginConnector
	deviceLogin, deviceLoginErr := codexaccounts.NewLegacyDeviceConnector()
	if deviceLoginErr != nil {
		logger.Printf("official account device login unavailable")
	} else {
		officialDeviceLogin = deviceLogin
	}
	officialAccounts, accountErr := codexaccounts.New(codexaccounts.Options{
		Path:            filepath.Join(dir, "codex-accounts.v1.protected"),
		Refresher:       codexaccounts.RefreshFunc(accountRuntime.Refresh),
		Login:           officialLogin,
		DeviceLogin:     officialDeviceLogin,
		ExclusiveWriter: true,
	})
	if accountErr != nil {
		if errors.Is(accountErr, codexaccounts.ErrVaultBusy) {
			logger.Printf("official account vault is owned by another Bridge process; existing API Key routes remain available")
		} else {
			logger.Printf("official account vault unavailable; existing API Key routes remain available")
		}
	}
	if officialAccounts != nil {
		defer officialAccounts.Close()
	}
	officialGateway := (codexruntime.AccountExecutor{Accounts: officialAccounts, Runtime: accountRuntime}).Do
	cons = console.New(console.Deps{
		Store:           store,
		Hub:             hub,
		Version:         version,
		Port:            LocalPort,
		LogPath:         logPathOf(lf),
		Exe:             exe,
		Dir:             dir,
		Logger:          logger,
		Quit:            cancel,
		Companion:       companion,
		Local:           localLauncher,
		ClaudeDesktop:   claudeDesktop,
		CodexAccounts:   officialAccounts,
		OfficialGateway: officialGateway,
		OfficialImages:  (codexruntime.AccountExecutor{Accounts: officialAccounts, Runtime: accountRuntime}).DoImages,
	})
	if err := cons.StartDirectRelay(); err != nil {
		logger.Printf("direct pairing listener unavailable: %v", err)
	}
	defer cons.CloseUsageLedger()
	defer cons.CloseAPIListeners()
	defer cons.CloseSelfHostServices()

	settings := func() agents.Settings {
		c := store.Get()
		if c.LocalToolPaths == nil {
			c.LocalToolPaths = map[string]string{}
		}
		for id, path := range localLauncher.RememberedPaths() {
			if c.LocalToolPaths[id] == "" {
				c.LocalToolPaths[id] = path
			}
		}
		s := agents.LocalSettingsWithGateway(c, LocalPort)
		s.BridgeExe, s.LocalPort, s.LocalToken = exe, LocalPort, localToken
		return s
	}
	sink := func(ev protocol.Event) {
		ev = cons.Attribute(ev)
		// Keep the explicitly selected direct conversation off Hub. If the
		// direct transport fails, it stays unavailable instead of silently
		// mirroring its private event stream through a different station.
		cons.PublishHubEvent(ev)
		cons.Broadcast(ev)
	}
	var agentOpts agents.Options
	agentOpts.BundledAgents = true
	if os.Getenv("SALCARA_BRIDGE_CONFIG_DIR") != "" {
		agentOpts.StateDir = dir
	}
	mgr := agents.NewManagerWithOptions(sink, settings, agentOpts)
	defer mgr.Close()
	hub.SetManager(mgr)
	cons.SetManager(mgr)
	// Start the console-owned maintenance loops with the process lifetime.
	// These are intentionally wired after both dependencies are ready: the
	// prober needs the API service and account store, while the watchdog needs
	// the manager.  ctx is cancelled by the signal/server shutdown path.
	cons.StartProber(ctx)
	cons.StartWatchdog(ctx)

	// Projects / device name changes → re-register.
	store.OnChange(func(old, cur config.Config) {
		if old.DeviceName != cur.DeviceName || len(old.Projects) != len(cur.Projects) || old.LocalToolPaths["codex"] != cur.LocalToolPaths["codex"] || old.LocalToolPaths["claude"] != cur.LocalToolPaths["claude"] || old.LocalToolPaths["deepseek-harness"] != cur.LocalToolPaths["deepseek-harness"] || old.LocalToolPaths["hermes"] != cur.LocalToolPaths["hermes"] {
			hub.Reregister()
		}
	})

	srv := &http.Server{
		Handler:           cons.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("console server: %v", err)
			cancel()
		}
	}()
	go mgr.Watch(ctx)
	go hub.Run(ctx)

	// Keep the autostart entry pointing at this exe (the user may have moved it).
	if c := store.Get(); c.Autostart && exe != "" {
		if err := autostart.Enable(exe); err != nil {
			logger.Printf("autostart refresh: %v", err)
		}
	}

	cfg := store.Get()
	if os.Getenv("SALCARA_EMBEDDED_WINDOW") != "1" && (!background || cfg.OpenConsoleOnStart || (!cfg.LoggedIn() && len(cfg.LocalAccounts) == 0)) {
		go func() {
			time.Sleep(300 * time.Millisecond)
			if err := autostart.OpenBrowser(consoleURL()); err != nil {
				logger.Printf("open browser: %v", err)
			}
		}()
	}
	logger.Printf("console: %s", consoleURL())

	<-ctx.Done()
	logger.Printf("shutting down")
	sctx, scancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer scancel()
	_ = srv.Shutdown(sctx)
	return nil
}

// configDir is SALCARA_BRIDGE_CONFIG_DIR or the per-user default (config.Dir).
func configDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv("SALCARA_BRIDGE_CONFIG_DIR")); d != "" {
		return filepath.Abs(d)
	}
	return config.Dir()
}

func logPathOf(lf *applog.File) string {
	if lf == nil {
		return ""
	}
	return lf.Path()
}
