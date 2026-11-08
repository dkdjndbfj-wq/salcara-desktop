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

func TestWorkspaceTerminalWindowsInboxCommands(t *testing.T) {
	c, _, root := workspaceFixture(t)
	first := openTestTerminal(t, c, root)
	first.waitReady(t)
	jsonFile := strings.ReplaceAll(filepath.Join(root, "inbox.json"), "'", "''")
	// Exercise ordinary inbox commands in the production terminal's isolated
	// environment. The output markers are
	// assembled at execution time, so echoed input cannot satisfy the test.
	line := "[IO.File]::WriteAllText('" + jsonFile + "', '{\"value\":\"ready\"}'); $value = Get-Content -LiteralPath '" + jsonFile + "' -Raw | ConvertFrom-Json; $text = New-Object System.Text.StringBuilder; [void]$text.Append($value.value); Write-Output ('INBOX_' + $text.ToString() + '_OUTPUT'); Write-Host ('INBOX_' + 'HOST')\r"
	sendTestTerminal(t, c, root, first.TerminalID, 1, line)
	first.waitText(t, "INBOX_ready_OUTPUT")
	first.waitText(t, "INBOX_HOST")
	first.waitReady(t)
}

func TestWorkspaceTerminalCloseKillsWindowsChildren(t *testing.T) {
	c, _, root := workspaceFixture(t)
	first := openTestTerminal(t, c, root)
	first.waitReady(t)
	pidFile := filepath.Join(root, "child-pid.txt")
	line := "$child = Start-Process -FilePath (Join-Path $PSHOME 'powershell.exe') -ArgumentList '-NoProfile','-Command','Start-Sleep -Seconds 60' -WindowStyle Hidden -PassThru -ErrorAction Stop; [IO.File]::WriteAllText('" + strings.ReplaceAll(pidFile, "'", "''") + "', [string]$child.Id); Write-Output ('CHILD_' + 'STARTED')\r"
	sendTestTerminal(t, c, root, first.TerminalID, 1, line)
	// Drain the real PTY and answer its renderer queries while waiting for the
	// child, instead of treating a file poll as an interactive terminal client.
	first.waitTextWithin(t, "CHILD_STARTED", 10*time.Second)
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid terminal child PID %q: %v", b, err)
	}
	child, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(child)
	if status, err := windows.WaitForSingleObject(child, 0); err != nil || status != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("child was not alive before PTY close: status=%d err=%v", status, err)
	}
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
