package install

import (
	"fmt"
	"strings"
)

// BuildWindowsDeleteCmdLine builds the detached cmd.exe command line that deletes
// exePath (and emptyDir if non-empty and empty) shortly after the current process exits.
// Used on Windows where a running executable cannot be unlinked while open.
func BuildWindowsDeleteCmdLine(exePath, emptyDir string) (string, error) {
	if exePath == "" || strings.ContainsAny(exePath, "\"\r\n&|<>^%") {
		return "", fmt.Errorf("unsafe executable path for scheduled delete: %q", exePath)
	}
	cmd := fmt.Sprintf(`cmd.exe /c ping -n 3 127.0.0.1 >nul & del /f /q "%s"`, exePath)
	if emptyDir != "" {
		if strings.ContainsAny(emptyDir, "\"\r\n&|<>^%") {
			return "", fmt.Errorf("unsafe directory path for scheduled delete: %q", emptyDir)
		}
		cmd += fmt.Sprintf(` & rmdir "%s" 2>nul`, emptyDir)
	}
	return cmd, nil
}
