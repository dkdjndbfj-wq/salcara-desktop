//go:build windows

package agents

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestClaudeRuntimeSettingsWindowsACL(t *testing.T) {
	path, err := writeClaudeRuntimeSettings(`{"env":{"ANTHROPIC_AUTH_TOKEN":"synthetic"}}`)
	if err != nil {
		t.Fatal(err)
	}
	defer removeClaudeRuntimeSettings(path)
	assertClaudeRuntimeWindowsACL(t, path)
}

func TestClaudeRuntimeSettingsWindowsOwnedModifyOnlyTemp(t *testing.T) {
	root := filepath.Join(t.TempDir(), "owned")
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce an owned custom TEMP which allows creating/removing files but
	// not changing ownership. Protection must still result in the exact private
	// owner/SYSTEM ACL before any credential contents are written.
	sd, err := windows.SecurityDescriptorFromString("O:" + user.User.Sid.String() + "D:P(D;OICI;WO;;;" + user.User.Sid.String() + ")(A;OICI;0x1301bf;;;" + user.User.Sid.String() + ")(A;OICI;FA;;;SY)")
	if err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(root)
	if err != nil {
		t.Fatal(err)
	}
	security := windows.SecurityAttributes{SecurityDescriptor: sd}
	security.Length = uint32(unsafe.Sizeof(security))
	if err := windows.CreateDirectory(name, &security); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"TEMP", "TMP"} {
		t.Setenv(key, root)
	}
	path, err := writeClaudeRuntimeSettings(`{"env":{"ANTHROPIC_AUTH_TOKEN":"synthetic"}}`)
	if err != nil {
		t.Fatal(err)
	}
	defer removeClaudeRuntimeSettings(path)
	assertClaudeRuntimeWindowsACL(t, path)
	if contents, err := os.ReadFile(path); err != nil || string(contents) != `{"env":{"ANTHROPIC_AUTH_TOKEN":"synthetic"}}` {
		t.Fatal("protected runtime contents were not saved")
	}
}

func assertClaudeRuntimeWindowsACL(t *testing.T, path string) {
	t.Helper()
	const fileAllAccess = 0x1f01ff // FILE_ALL_ACCESS after generic access mapping.
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{filepath.Dir(path), path} {
		sd, err := windows.GetNamedSecurityInfo(target, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		owner, _, err := sd.Owner()
		if err != nil || owner == nil || !owner.Equals(user.User.Sid) {
			t.Fatal("runtime owner differs from the current user")
		}
		if target == filepath.Dir(path) {
			control, _, err := sd.Control()
			if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
				t.Fatal("runtime directory ACL is not protected")
			}
		}
		dacl, _, err := sd.DACL()
		if err != nil || dacl == nil || dacl.AceCount != 2 {
			t.Fatal("private runtime grants unexpected principals")
		}
		seenUser, seenSystem := false, false
		for i := uint32(0); i < uint32(dacl.AceCount); i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if windows.GetAce(dacl, i, &ace) != nil || ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || int(ace.Header.AceSize) < 16 || ace.Mask != fileAllAccess {
				t.Fatal("unexpected private runtime ACE")
			}
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if !sid.IsValid() || sid.Len()+int(unsafe.Offsetof(ace.SidStart)) > int(ace.Header.AceSize) {
				t.Fatal("invalid ACL identity")
			}
			switch {
			case sid.Equals(user.User.Sid) && !seenUser:
				seenUser = true
			case sid.IsWellKnown(windows.WinLocalSystemSid) && !seenSystem:
				seenSystem = true
			default:
				t.Fatal("runtime file grants another principal")
			}
		}
		if !seenUser || !seenSystem {
			t.Fatal("runtime permission identities missing")
		}
	}
}
