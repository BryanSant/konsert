//go:build windows

package hubkey

import (
	"fmt"
	"os/user"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/windows"
)

// isAdmin reports membership of the local Administrators group, which is what the
// default sshd_config's "Match Group administrators" tests.
func isAdmin() bool {
	u, err := user.Current()
	if err != nil {
		return false
	}
	groups, err := u.GroupIds()
	if err != nil {
		return false
	}
	return slices.Contains(groups, AdminsSID)
}

// restrictKeysFile applies AdminKeysSDDL to administrators_authorized_keys; sshd
// ignores that file unless only SYSTEM and Administrators can access it. A user's own
// authorized_keys keeps the profile's inherited ACL.
func restrictKeysFile(path string) error {
	if !strings.EqualFold(filepath.Base(path), "administrators_authorized_keys") {
		return nil
	}
	sd, err := windows.SecurityDescriptorFromString(AdminKeysSDDL)
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
