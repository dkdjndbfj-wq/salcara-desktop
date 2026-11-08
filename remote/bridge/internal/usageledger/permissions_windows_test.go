//go:build windows

package usageledger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestSQLiteDirectoryProtectsInheritedJournalPermissions(t *testing.T) {
	l := testLedger(t)
	journal := filepath.Join(filepath.Dir(l.path), "synthetic-journal")
	if err := os.WriteFile(journal, []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Dir(l.path), l.path, journal} {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		dacl, _, err := sd.DACL()
		if err != nil || dacl == nil || dacl.AceCount != 2 {
			t.Fatal("storage grants unexpected principals", sd.String())
		}
		text := sd.String()
		if !strings.Contains(text, user.User.Sid.String()) || !strings.Contains(text, ";;;SY)") {
			t.Fatal("missing owner/system ACL", text)
		}
	}
}
