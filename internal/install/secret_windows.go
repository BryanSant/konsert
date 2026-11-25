//go:build windows

package install

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// restrictFile replaces path's DACL with a protected (non-inherited) one granting full
// control to SYSTEM and the current user only.
func restrictFile(path string) error {
	user, err := currentUserSID()
	if err != nil {
		return err
	}
	return setProtectedDACL(path, userFileSDDL(user))
}

// userFileSDDL is the DACL of a per-user secret file: SYSTEM and the owner, full
// control. "P" protects it from inheriting the parent's ACEs.
func userFileSDDL(owner string) string {
	return "D:P(A;;FA;;;SY)(A;;FA;;;" + owner + ")"
}

// serviceDirSDDL is the DACL of the service log directory: SYSTEM and Administrators
// full control, the service account modify (0x1301bf), inherited by files.
func serviceDirSDDL(account string) string {
	return "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1301bf;;;" + account + ")"
}

func setProtectedDACL(path, sddl string) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return fmt.Errorf("parse SDDL: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("read DACL: %w", err)
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil)
}

// currentUserSID returns the SID string of the user running this process.
func currentUserSID() (string, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("current user SID: %w", err)
	}
	return tu.User.Sid.String(), nil
}

// RestrictDir replaces dir's DACL with a protected one granting SYSTEM and the current
// user full control, inherited by everything created in it.
func RestrictDir(dir string) error {
	user, err := currentUserSID()
	if err != nil {
		return err
	}
	return setProtectedDACL(dir, "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;"+user+")")
}
