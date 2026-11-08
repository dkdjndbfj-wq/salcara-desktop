//go:build windows && terminaldiagnostic

// The manual diagnostic script copies this probe beside the production ConPTY
// source in an isolated module. Normal Bridge tests do not include this probe.
package hubclient

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

type terminalProcess interface {
	io.ReadWriteCloser
	Resize(int, int) error
	Wait() (int, error)
}

type diagnosticTerminal struct {
	process        terminalProcess
	writeMu        sync.Mutex
	mu             sync.Mutex
	output         string
	queries        int
	responseErrors []string
	exitCode       int
	exitError      string
	exited         bool
	readDone       chan struct{}
	waitDone       chan struct{}
}

func (d *diagnosticTerminal) write(data string) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	n, err := d.process.Write([]byte(data))
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

func startDiagnosticTerminal(t *testing.T, root string) *diagnosticTerminal {
	t.Helper()
	process, err := startTerminalProcess(root, 80, 24, terminalEnvironment())
	if err != nil {
		t.Fatal(err)
	}
	d := &diagnosticTerminal{process: process, readDone: make(chan struct{}), waitDone: make(chan struct{})}
	go func() {
		defer close(d.readDone)
		buf := make([]byte, 8192)
		for {
			n, err := process.Read(buf)
			if n > 0 {
				d.mu.Lock()
				d.output += string(buf[:n])
				queries := strings.Count(d.output, "\x1b[6n")
				responses := queries - d.queries
				d.queries = queries
				d.mu.Unlock()
				for i := 0; i < responses; i++ {
					if responseErr := d.write("\x1b[1;1R"); responseErr != nil {
						d.mu.Lock()
						d.responseErrors = append(d.responseErrors, responseErr.Error())
						d.mu.Unlock()
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		defer close(d.waitDone)
		code, err := process.Wait()
		d.mu.Lock()
		d.exited, d.exitCode = true, code
		if err != nil {
			d.exitError = err.Error()
		}
		d.mu.Unlock()
		process.Close()
	}()
	t.Cleanup(func() {
		process.Close()
		for _, done := range []chan struct{}{d.waitDone, d.readDone} {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("diagnostic PTY did not finish teardown")
			}
		}
	})
	return d
}

func (d *diagnosticTerminal) waitText(text string, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		d.mu.Lock()
		found, exited := strings.Contains(d.output, text), d.exited
		d.mu.Unlock()
		if found || exited {
			return found
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestTerminalDiagnostic(t *testing.T) {
	version := windows.RtlGetVersion()
	t.Logf("variant=%s go=%s windows=%d.%d.%d pid=%d", os.Getenv("SALCARA_TERMINAL_DIAG_VARIANT"), runtime.Version(), version.MajorVersion, version.MinorVersion, version.BuildNumber, os.Getpid())
	for _, std := range []struct {
		name string
		id   uint32
	}{{"stdin", windows.STD_INPUT_HANDLE}, {"stdout", windows.STD_OUTPUT_HANDLE}, {"stderr", windows.STD_ERROR_HANDLE}} {
		handle, err := windows.GetStdHandle(std.id)
		var mode uint32
		modeErr := windows.GetConsoleMode(handle, &mode)
		t.Logf("parent %s handle=%#x handleErr=%v mode=%#x modeErr=%v", std.name, handle, err, mode, modeErr)
	}
	if strings.Contains(os.Getenv("SALCARA_TERMINAL_DIAG_VARIANT"), "detached") {
		ok, _, err := windows.NewLazySystemDLL("kernel32.dll").NewProc("FreeConsole").Call()
		t.Logf("FreeConsole result=%d err=%v", ok, err)
	}
	system, err := windows.GetSystemDirectory()
	if err != nil {
		t.Fatal(err)
	}
	metadata := exec.Command(filepath.Join(system, "WindowsPowerShell", "v1.0", "powershell.exe"), "-NoLogo", "-NoProfile", "-NonInteractive", "-Command",
		"[pscustomobject]@{Version=$PSVersionTable.PSVersion.ToString(); LanguageMode=[string]$ExecutionContext.SessionState.LanguageMode; InputRedirected=[Console]::IsInputRedirected; OutputRedirected=[Console]::IsOutputRedirected; PSReadLine=@(Get-Module -ListAvailable PSReadLine | ForEach-Object { [pscustomobject]@{Version=$_.Version.ToString(); Path=$_.Path} })} | ConvertTo-Json -Compress -Depth 4")
	metadata.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	output, metadataErr := metadata.CombinedOutput()
	t.Logf("PowerShell metadata err=%v output=%s", metadataErr, output)
	for _, ending := range []string{"cr", "lf", "crlf", "separate-cr", "win32-enter"} {
		t.Run(ending, func(t *testing.T) {
			root := t.TempDir()
			promptSuffix := filepath.Base(root) + ">"
			t.Logf("requestedRoot=%q startupPromptSuffix=%q", root, promptSuffix)
			d := startDiagnosticTerminal(t, root)
			// Before any input, match the directory suffix because PowerShell can
			// expand a short Windows temp path into its long form in the prompt.
			if !d.waitText(promptSuffix, 10*time.Second) {
				d.mu.Lock()
				t.Logf("startup output=%q", d.output)
				d.mu.Unlock()
				t.Fatal("no initial shell prompt")
			}
			marker := "PTY_EXECUTED_" + ending
			markerFile := filepath.Join(root, "executed.txt")
			command := fmt.Sprintf("$m=('PTY_'+'EXECUTED_'+'%s'); [IO.File]::WriteAllText('%s',$m); Write-Output $m", ending, strings.ReplaceAll(markerFile, "'", "''"))
			input := command
			switch ending {
			case "cr":
				input += "\r"
			case "lf":
				input += "\n"
			case "crlf":
				input += "\r\n"
			case "win32-enter":
				input += "\x1b[13;28;13;1;0;1_\x1b[13;28;13;0;0;1_"
			}
			t.Logf("sending bytes=%d suffix=%q", len(input), input[len(command):])
			if err := d.write(input); err != nil {
				t.Fatal(err)
			}
			if ending == "separate-cr" {
				time.Sleep(50 * time.Millisecond)
				if err := d.write("\r"); err != nil {
					t.Fatal(err)
				}
			}
			visible := d.waitText(marker, 5*time.Second)
			fileBytes, fileErr := os.ReadFile(markerFile)
			d.mu.Lock()
			t.Logf("visibleMarker=%t executedFile=%t fileErr=%v exited=%t exitCode=%d exitErr=%q cursorQueries=%d responseErrors=%q rawOutput=%q", visible, string(fileBytes) == marker, fileErr, d.exited, d.exitCode, d.exitError, d.queries, d.responseErrors, d.output)
			d.mu.Unlock()
			if !visible || string(fileBytes) != marker {
				t.Error("input did not produce both an execution file and terminal output; see observations above")
			}
		})
	}
}
