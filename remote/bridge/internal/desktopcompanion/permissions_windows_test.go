//go:build windows

package desktopcompanion

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsBackupsHaveProtectedOwnerAndSystemDACL(t *testing.T) {
	s, o := fixture(t, "model = \"fixture-private\"\n")
	if _, err := s.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(o.DataDir, "desktop-companion", "backups", "*.toml"))
	if err != nil || len(files) != 2 {
		t.Fatal("backup missing")
	}
	archives, err := filepath.Glob(filepath.Join(o.DataDir, "desktop-companion", "archives", "*.json"))
	if err != nil || len(archives) != 1 {
		t.Fatal("ownership archive missing")
	}
	var token windows.Token
	if err = windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(o.DataDir, "desktop-companion"), filepath.Join(o.DataDir, "desktop-companion", "backups"), filepath.Join(o.DataDir, "desktop-companion", "archives"), filepath.Join(o.CodexHome, "config.toml")}
	paths = append(paths, files...)
	paths = append(paths, archives...)
	for _, path := range paths {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		control, _, err := sd.Control()
		if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
			t.Fatal("private ACL inherited broader access")
		}
		dacl, _, err := sd.DACL()
		if err != nil || dacl == nil || dacl.AceCount != 2 {
			t.Fatal("private ACL granted access beyond the two intended principals")
		}
		text := sd.String()
		if !strings.Contains(text, user.User.Sid.String()) || !strings.Contains(text, ";;;SY)") || strings.Contains(text, ";;;WD)") || strings.Contains(text, ";;;BU)") || strings.Contains(text, ";;;AU)") {
			t.Fatal("backup ACL did not restrict access to current user and SYSTEM")
		}
	}
}
