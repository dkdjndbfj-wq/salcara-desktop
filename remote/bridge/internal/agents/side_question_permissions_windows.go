//go:build windows

package agents

import (
	"errors"

	"golang.org/x/sys/windows"
)

// Protect the directory before atomicfile creates any temporary history file.
// The inherited DACL, rather than a POSIX mode bit, protects Windows contents.
func protectSideConversationDirectory(path string) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + user.User.Sid.String() + ")(A;OICI;FA;;;SY)")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	current, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := current.Owner()
	if err != nil {
		return err
	}
	flags := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	var newOwner *windows.SID
	if owner == nil || !owner.Equals(user.User.Sid) {
		flags |= windows.OWNER_SECURITY_INFORMATION
		newOwner = user.User.Sid
	}
	// A creator can change its own DACL even when the inherited ACL does not
	// grant WRITE_OWNER. Do not request that extra right just to set the same
	// owner again; a different owner still requires the original strict change.
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, flags, newOwner, nil, dacl, nil); err != nil {
		return err
	}
	verified, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err = verified.Owner()
	if err != nil {
		return err
	}
	if owner == nil || !owner.Equals(user.User.Sid) {
		return errors.New("side question directory owner changed")
	}
	return nil
}
