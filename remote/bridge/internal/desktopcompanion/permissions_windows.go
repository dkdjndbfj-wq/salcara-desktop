//go:build windows

package desktopcompanion

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Read and verify; never repair a descriptor's ACL while reading its token.
func checkPrivatePermissions(path string) error {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return checkPrivateSecurityDescriptor(sd, user.User.Sid)
}

func checkPrivateSecurityDescriptor(sd *windows.SECURITY_DESCRIPTOR, userSID *windows.SID) error {
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(userSID) {
		return errors.New("not current owner")
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("unprotected DACL")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 2 {
		return errors.New("unexpected DACL")
	}
	// SDDL may abbreviate a well-known account SID (for example local
	// Administrator as LA). Inspect actual ACE identities, not formatted text.
	seenUser, seenSystem := false, false
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(dacl, i, &ace) != nil || ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || int(ace.Header.AceSize) < 16 {
			return errors.New("unexpected private ACE")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() || sid.Len()+int(unsafe.Offsetof(ace.SidStart)) > int(ace.Header.AceSize) {
			return errors.New("invalid private ACE identity")
		}
		switch {
		case sid.Equals(userSID) && !seenUser:
			seenUser = true
		case sid.IsWellKnown(windows.WinLocalSystemSid) && !seenSystem:
			seenSystem = true
		default:
			return errors.New("not owner and SYSTEM only")
		}
	}
	if !seenUser || !seenSystem {
		return errors.New("not owner and SYSTEM only")
	}
	return nil
}

// A mode of 0600 is not an ACL on Windows. Protect every backup before writing
// contents, allowing only the current user and SYSTEM to access installer data.
func setPrivatePermissions(path string, directory bool) error {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	flags := ""
	if directory {
		flags = "OICI"
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;" + flags + ";FA;;;SY)(A;" + flags + ";FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	// An elevated token may default new-object ownership to Administrators.
	// Set the actual installing user explicitly; the read path must not repair
	// or accept a foreign owner just because its DACL looks private.
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, user.User.Sid, nil, dacl, nil)
}
