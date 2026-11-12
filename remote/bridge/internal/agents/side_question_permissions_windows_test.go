//go:build windows

package agents

import (
	"context"
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
	dir := t.TempDir()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	// Modify permits normal history access but deliberately omits WRITE_OWNER.
	// The current owner retains the right to protect its own DACL.
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;0x1201bf;;;" + user.User.Sid.String() + ")(A;OICI;FA;;;SY)")
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
	dacl, _, err = verified.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 2 {
		t.Fatal("protected history DACL changed", err)
	}
}
