//go:build windows

package agents

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
	"salcara/bridge/internal/protocol"
)

func TestPersistentSideQuestionWindowsHistoryACL(t *testing.T) {
	r, snapshot, binding, chain := persistentSideFixture(t)
	_, err := r.conversation(context.Background(), "codex:parent", "phone", snapshot, protocol.SideQuestionConversationRequest{Action: "open", ChainID: chain}, binding, func() error { return nil }, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{r.persistentDir, filepath.Join(r.persistentDir, sideConversationStorageKey("phone", "codex:parent", chain)+".json")} {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		dacl, _, err := sd.DACL()
		if err != nil || dacl == nil || dacl.AceCount != 2 {
			t.Fatal("history grants unexpected principals")
		}
		seenUser, seenSystem := false, false
		for i := uint32(0); i < uint32(dacl.AceCount); i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if windows.GetAce(dacl, i, &ace) != nil || ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || int(ace.Header.AceSize) < 16 {
				t.Fatal("unexpected history ACL")
			}
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if !sid.IsValid() || sid.Len()+int(unsafe.Offsetof(ace.SidStart)) > int(ace.Header.AceSize) {
				t.Fatal("invalid history identity")
			}
			switch {
			case sid.Equals(user.User.Sid) && !seenUser:
				seenUser = true
			case sid.IsWellKnown(windows.WinLocalSystemSid) && !seenSystem:
				seenSystem = true
			default:
				t.Fatal("history grants another principal")
			}
		}
		if !seenUser || !seenSystem {
			t.Fatal("private history identities missing")
		}
	}
}

func TestProtectSideConversationAlreadyOwnedWithoutWriteOwner(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	dir := createUserOwnedSideQuestionDirectory(t, user.User.Sid)
	// Normal history access deliberately omits WRITE_OWNER and DELETE.
	// The current owner retains the right to protect its own DACL.
	// The explicit deny also makes the regression meaningful for group grants.
	sd, err := windows.SecurityDescriptorFromString("D:P(D;OICI;WO;;;" + user.User.Sid.String() + ")(A;OICI;0x1201bf;;;" + user.User.Sid.String() + ")(A;OICI;FA;;;SY)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	// The previous unconditional owner assignment must fail on this real ACL.
	// Prove the missing right instead of merely assuming the fixture excludes it.
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, user.User.Sid, nil, nil, nil); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("fixture must reject even an unchanged owner assignment: %v", err)
	}
	if err := protectSideConversationDirectory(dir); err != nil {
		t.Fatal("same owner must not require WRITE_OWNER", err)
	}
	verified, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := verified.Owner()
	if err != nil || owner == nil || !owner.Equals(user.User.Sid) {
		t.Fatal("current owner was not retained", err)
	}
	control, _, err := verified.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("history DACL is not protected", err)
	}
	dacl, _, err = verified.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 2 {
		t.Fatal("protected history DACL changed", err)
	}
	assertSideQuestionDirectoryPrincipals(t, dacl, user.User.Sid)
}

func TestProtectSideConversationDeniedDACLRemainsUnchanged(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	dir := createUserOwnedSideQuestionDirectory(t, user.User.Sid)
	// OWNER RIGHTS suppresses the owner's implicit WRITE_DAC. Normal file
	// access remains available; protection must fail, not reuse a weaker ACL.
	sd, err := windows.SecurityDescriptorFromString("D:P(D;OICI;WDWO;;;OW)(A;OICI;0x1201bf;;;" + user.User.Sid.String() + ")(A;OICI;FA;;;SY)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	before, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if err := protectSideConversationDirectory(dir); !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("unprotectable directory must fail closed: %v", err)
	}
	after, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || before.String() != after.String() {
		t.Fatal("failed protection changed ownership or ACL", err)
	}
}

func createUserOwnedSideQuestionDirectory(t *testing.T, user *windows.SID) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "owned")
	// A new directory uses TokenOwner, not its parent's owner. Elevated CI
	// tokens may default to Administrators even though TokenUser is different.
	// Establish and verify this exact fixture's user owner before narrowing ACLs.
	sd, err := windows.SecurityDescriptorFromString("O:" + user.String() + "D:P(A;OICI;FA;;;" + user.String() + ")(A;OICI;FA;;;SY)")
	if err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		t.Fatal(err)
	}
	security := windows.SecurityAttributes{SecurityDescriptor: sd}
	security.Length = uint32(unsafe.Sizeof(security))
	if err := windows.CreateDirectory(name, &security); err != nil {
		t.Fatal(err)
	}
	created, err := windows.GetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := created.Owner()
	if err != nil || owner == nil || !owner.Equals(user) {
		t.Fatal("fixture directory is not genuinely owned by the token user", err)
	}
	return dir
}

func assertSideQuestionDirectoryPrincipals(t *testing.T, dacl *windows.ACL, user *windows.SID) {
	t.Helper()
	const fileAllAccess = 0x1f01ff
	seenUser, seenSystem := false, false
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(dacl, i, &ace) != nil || ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE || int(ace.Header.AceSize) < 16 || ace.Mask != fileAllAccess {
			t.Fatal("unexpected protected history directory ACE")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() || sid.Len()+int(unsafe.Offsetof(ace.SidStart)) > int(ace.Header.AceSize) {
			t.Fatal("invalid protected history identity")
		}
		switch {
		case sid.Equals(user) && !seenUser:
			seenUser = true
		case sid.IsWellKnown(windows.WinLocalSystemSid) && !seenSystem:
			seenSystem = true
		default:
			t.Fatal("protected history grants another principal")
		}
	}
	if !seenUser || !seenSystem {
		t.Fatal("protected history identities missing")
	}
}
