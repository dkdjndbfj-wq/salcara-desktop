//go:build windows

package hubclient

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWorkspaceTerminalCloseKillsWindowsChildren(t *testing.T) {
	c, _, root := workspaceFixture(t)
	first := openTestTerminal(t, c, root)
	pidFile := filepath.Join(root, "child-pid.txt")
	line := "$child = Start-Process -FilePath (Join-Path $PSHOME 'powershell.exe') -ArgumentList '-NoProfile','-Command','Start-Sleep -Seconds 60' -WindowStyle Hidden -PassThru; [IO.File]::WriteAllText('" + strings.ReplaceAll(pidFile, "'", "''") + "', [string]$child.Id)\r"
	sendTestTerminal(t, c, root, first.TerminalID, 1, line)
	var pid int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(pidFile)
		if err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			if pid > 0 {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("terminal did not create its test child")
	}
	child, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(child)
	cmd := terminalCommand(root, "close")
	cmd["terminalId"] = first.TerminalID
	if _, err = c.dispatchWorkspaceTerminal(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	status, err := windows.WaitForSingleObject(child, 5000)
	if err != nil || status != windows.WAIT_OBJECT_0 {
		t.Fatalf("child survived PTY close: status=%d err=%v", status, err)
	}
}
