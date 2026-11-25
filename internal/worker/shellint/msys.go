package shellint

import "strings"

// TranslateMSYSPath converts MSYS2 / Git Bash paths ("/c/Users/alice", "/c",
// "//server/share/dir") into Windows paths ("C:\Users\alice", "C:\",
// "\\server\share\dir"). Paths that are not MSYS drive or UNC paths (such as
// "/tmp", "/usr/bin", or already-Windows "C:\Users\alice") are returned unchanged.
func TranslateMSYSPath(p string) string {
	if len(p) < 2 || p[0] != '/' {
		return p
	}
	// UNC path: //server/share[/...]
	if p[1] == '/' {
		if len(p) > 2 && p[2] != '/' {
			return `\\` + strings.ReplaceAll(p[2:], "/", `\`)
		}
		return p
	}
	// Drive path: /<drive> or /<drive>/...
	drive := p[1]
	isLetter := (drive >= 'a' && drive <= 'z') || (drive >= 'A' && drive <= 'Z')
	if !isLetter {
		return p
	}
	if len(p) == 2 {
		return strings.ToUpper(string(drive)) + `:\`
	}
	if p[2] != '/' {
		return p
	}
	rest := strings.TrimRight(p[3:], "/")
	if rest == "" {
		return strings.ToUpper(string(drive)) + `:\`
	}
	return strings.ToUpper(string(drive)) + `:\` + strings.ReplaceAll(rest, "/", `\`)
}
