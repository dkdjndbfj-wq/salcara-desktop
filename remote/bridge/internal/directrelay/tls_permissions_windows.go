//go:build windows

package directrelay

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func protectDirectTLSIdentityFile(path string) error {
	if err := checkDirectTLSIdentityPath(path); err != nil {
		return err
	}
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return fmt.Errorf("read current user for direct TLS ACL: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("read current user for direct TLS ACL: %w", err)
	}
	current, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read direct TLS private key owner: %w", err)
	}
	owner, _, err := current.Owner()
	if err != nil || owner == nil || !owner.Equals(user.User.Sid) {
		return errors.New("direct TLS identity file is not owned by current user")
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;SY)(A;;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return fmt.Errorf("create direct TLS private ACL: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("read direct TLS private ACL: %w", err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		// The owner can restrict its own DACL without WRITE_OWNER. Reassigning
		// the same owner unnecessarily fails on standard non-system drives.
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("protect direct TLS private key for current user and SYSTEM: %w", err)
	}
	return verifyDirectTLSIdentityFile(path)
}

func verifyDirectTLSIdentityFile(path string) error {
	if err := checkDirectTLSIdentityPath(path); err != nil {
		return err
	}
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return fmt.Errorf("read current user for direct TLS ACL: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("read current user for direct TLS ACL: %w", err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read direct TLS private ACL: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(user.User.Sid) {
		return errors.New("direct TLS identity file is not owned by current user")
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("direct TLS identity ACL is not protected from inheritance")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 2 {
		return errors.New("direct TLS identity ACL contains unexpected principals")
	}
	seenUser, seenSystem := false, false
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(dacl, i, &ace) != nil || ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 || int(ace.Header.AceSize) < 16 {
			return errors.New("direct TLS identity ACL contains an invalid entry")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() || sid.Len()+int(unsafe.Offsetof(ace.SidStart)) > int(ace.Header.AceSize) {
			return errors.New("direct TLS identity ACL contains an invalid principal")
		}
		switch {
		case sid.Equals(user.User.Sid) && !seenUser:
			seenUser = true
		case sid.IsWellKnown(windows.WinLocalSystemSid) && !seenSystem:
			seenSystem = true
		default:
			return errors.New("direct TLS identity ACL grants access beyond current user and SYSTEM")
		}
	}
	if !seenUser || !seenSystem {
		return errors.New("direct TLS identity ACL must grant access only to current user and SYSTEM")
	}
	return nil
}

func checkDirectTLSIdentityPath(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect direct TLS identity file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("direct TLS identity path must be a regular file")
	}
	attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(path))
	if err != nil {
		return fmt.Errorf("inspect direct TLS identity attributes: %w", err)
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || attributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return errors.New("direct TLS identity path must be a regular file")
	}
	return nil
}
