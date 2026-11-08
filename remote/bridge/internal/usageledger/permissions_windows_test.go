//go:build windows

package usageledger

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// FILE_ALL_ACCESS from winnt.h; x/sys/windows does not export this constant.
const fileAllAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff

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
		if path != journal {
			control, _, err := sd.Control()
			if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
				t.Fatal("storage DACL is not protected", sd.String(), err)
			}
		}
		seenUser, seenSystem := false, false
		for i := uint32(0); i < uint32(dacl.AceCount); i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(dacl, i, &ace); err != nil || ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || int(ace.Header.AceSize) < 16 {
				t.Fatal("storage contains an invalid allow entry", sd.String(), err)
			}
			if ace.Mask != fileAllAccess || ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
				t.Fatal("storage does not grant effective full control", sd.String())
			}
			if path == filepath.Dir(l.path) && ace.Header.AceFlags&(windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE) != windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE {
				t.Fatal("storage permissions do not inherit to journals and subdirectories", sd.String())
			}
			if path == journal && ace.Header.AceFlags&windows.INHERITED_ACE == 0 {
				t.Fatal("journal did not inherit its private ACL", sd.String())
			}
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if !sid.IsValid() || sid.Len()+int(unsafe.Offsetof(ace.SidStart)) > int(ace.Header.AceSize) {
				t.Fatal("storage contains an invalid principal", sd.String())
			}
			// SDDL may abbreviate the built-in Administrator SID as LA. Compare
			// the actual SID instead of its serialized spelling.
			switch {
			case sid.Equals(user.User.Sid) && !seenUser:
				seenUser = true
			case sid.IsWellKnown(windows.WinLocalSystemSid) && !seenSystem:
				seenSystem = true
			default:
				t.Fatal("storage grants access beyond current user and SYSTEM", sd.String())
			}
		}
		if !seenUser || !seenSystem {
			t.Fatal("missing current user/SYSTEM ACL", sd.String())
		}
	}
}
