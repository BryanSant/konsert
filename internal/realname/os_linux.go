package realname

import (
	"context"
	"os"
	"strings"
)

// osFullName reads the GECOS field: getent covers LDAP/SSSD users, /etc/passwd the rest.
func osFullName(ctx context.Context, login string) string {
	if login == "" {
		return ""
	}
	if line := run(ctx, "getent", "passwd", login); line != "" {
		return ParseGECOS(line)
	}
	data, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, login+":") {
			return ParseGECOS(line)
		}
	}
	return ""
}
