//go:build windows

package agents

import (
	"errors"

	"golang.org/x/sys/windows"
)

func protectClaudeRuntimeDir(path string) error {
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
	information := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	var newOwner *windows.SID
	// An owner can replace its DACL without WRITE_OWNER. Custom temporary
	// directories may grant Modify only; asking to set the unchanged owner
	// would fail before the private ACL is applied. A different owner still
	// requires the original ownership-change permission.
	if owner == nil || !owner.Equals(user.User.Sid) {
		information |= windows.OWNER_SECURITY_INFORMATION
		newOwner = user.User.Sid
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, information, newOwner, nil, dacl, nil); err != nil {
		return err
	}
	current, err = windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err = current.Owner()
	if err != nil {
		return err
	}
	if owner == nil || !owner.Equals(user.User.Sid) {
		return errors.New("Claude runtime directory owner changed")
	}
	return nil
}
