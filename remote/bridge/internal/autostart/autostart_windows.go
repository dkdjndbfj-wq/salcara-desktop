//go:build windows

package autostart

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

const nativeRunKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const runKey = `HKCU\` + nativeRunKey

// Command builds an exec.Cmd that never flashes a console window (the bridge is a GUI-subsystem binary).
func Command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	return cmd
}

// Enable adds the Run registry value.
func Enable(exe string) error {
	return Command("reg", "add", runKey, "/v", AppName, "/t", "REG_SZ", "/d", `"`+exe+`" --background`, "/f").Run()
}

// Disable removes the Run registry value.
func Disable() error {
	if !Enabled() {
		return nil
	}
	return Command("reg", "delete", runKey, "/v", AppName, "/f").Run()
}

// Enabled reports whether the Run value exists.
func Enabled() bool {
	key, err := registry.OpenKey(registry.CURRENT_USER, nativeRunKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()
	// This status is read on every local state request. Use the OS API instead
	// of starting a reg.exe child process on each navigation or refresh.
	_, _, err = key.GetStringValue(AppName)
	return err == nil
}

// OpenBrowser opens url in the default browser.
func OpenBrowser(url string) error {
	return Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

// OpenPath shows a file (selected) or folder in Explorer.
func OpenPath(p string) error {
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		return Command("explorer", "/select,", p).Start()
	}
	return Command("explorer", p).Start()
}
