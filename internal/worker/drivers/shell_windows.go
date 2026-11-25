//go:build windows

package drivers

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// gitForWindowsRegistryPath returns the Git for Windows InstallPath from the registry,
// or "" when Git for Windows is not registered.
func gitForWindowsRegistryPath() string {
	type regLoc struct {
		root registry.Key
		path string
	}
	locs := []regLoc{
		{registry.LOCAL_MACHINE, `SOFTWARE\GitForWindows`},
		{registry.CURRENT_USER, `SOFTWARE\GitForWindows`},
		{registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\GitForWindows`},
	}
	for _, loc := range locs {
		k, err := registry.OpenKey(loc.root, loc.path, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		val, _, err := k.GetStringValue("InstallPath")
		_ = k.Close()
		if err == nil {
			if p := strings.TrimSpace(val); p != "" {
				return p
			}
		}
	}
	return ""
}
