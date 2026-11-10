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
	logClaudeRuntimeWindowsToken(t)
	path, err := writeClaudeRuntimeSettings(`{"env":{"ANTHROPIC_AUTH_TOKEN":"synthetic"}}`)
	if err != nil {
		t.Fatal(err)
	}
	defer removeClaudeRuntimeSettings(path)
	assertClaudeRuntimeWindowsACL(t, path)
}

func TestClaudeRuntimeSettingsWindowsOwnedModifyOnlyTemp(t *testing.T) {
	logClaudeRuntimeWindowsToken(t)
	root := filepath.Join(t.TempDir(), "owned")
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	// Give this exact runtime directory the user owner. A parent's owner is not
	// inherited: elevated tokens may otherwise create an Administrators-owned
	// child, which correctly requires WRITE_OWNER to repair.
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
	if err := protectClaudeRuntimeDir(root); err != nil {
		t.Fatalf("protect genuinely user-owned Modify-only directory: %v", err)
	}
	path := filepath.Join(root, "settings.json")
	defer removeClaudeRuntimeSettings(path)
	// Both identities and ACLs must be private before the first secret write.
	assertClaudeRuntimeWindowsACL(t, path)
	if contents, err := os.ReadFile(path); err != nil || len(contents) != 0 {
		t.Fatal("runtime file was not empty before the credential write")
	}
	if err := os.WriteFile(path, []byte(`{"env":{"ANTHROPIC_AUTH_TOKEN":"synthetic"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(path); err != nil || string(contents) != `{"env":{"ANTHROPIC_AUTH_TOKEN":"synthetic"}}` {
		t.Fatal("protected runtime contents were not saved")
	}
}

func TestClaudeRuntimeSettingsWindowsExistingFileIsNotReplaced(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "settings.json")
	if err := os.WriteFile(path, []byte("existing fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := protectClaudeRuntimeDir(root); err == nil {
		t.Fatal("protection reused an existing settings file")
	}
	if contents, err := os.ReadFile(path); err != nil || string(contents) != "existing fixture" {
		t.Fatal("protection replaced existing file contents")
	}
}

func TestClaudeRuntimeSettingsWindowsProtectionFailureRemovesEmptyRuntime(t *testing.T) {
	root := filepath.Join(t.TempDir(), "restricted-temp")
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("O:" + user.User.Sid.String() + "D:P(D;OICI;WDWO;;;OW)(A;OICI;0x1301bf;;;" + user.User.Sid.String() + ")(A;OICI;FA;;;SY)")
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
	if err == nil || path != "" {
		if path != "" {
			removeClaudeRuntimeSettings(path)
		}
		t.Fatal("writer accepted a runtime whose directory cannot be protected")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("protection failure retained a runtime directory or credential file")
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
			t.Fatalf("runtime %s owner differs from current user: actual=%v expected=%s error=%v", filepath.Base(target), owner, user.User.Sid, err)
		}
		control, _, err := sd.Control()
		if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
			t.Fatal("runtime ACL is not protected")
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

func logClaudeRuntimeWindowsToken(t *testing.T) {
	t.Helper()
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	var size uint32
	if err := windows.GetTokenInformation(token, windows.TokenOwner, nil, 0, &size); err != windows.ERROR_INSUFFICIENT_BUFFER || size == 0 {
		t.Fatalf("query token default owner size: %v", err)
	}
	buffer := make([]byte, size)
	if err := windows.GetTokenInformation(token, windows.TokenOwner, &buffer[0], size, &size); err != nil {
		t.Fatal(err)
	}
	owner := *(**windows.SID)(unsafe.Pointer(&buffer[0]))
	if owner == nil || !owner.IsValid() {
		t.Fatal("token default owner is invalid")
	}
	t.Logf("runtime ACL token: user=%s defaultOwner=%s elevated=%v", user.User.Sid, owner, token.IsElevated())
}
