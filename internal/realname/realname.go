// Package realname finds a person's display name for a login on this machine
// (SYR-98VFV): the global git user.name, else the OS account's full name (Linux GECOS,
// macOS RealName, Windows display name), else nothing. It never blocks a request: callers
// look it up in the background and cache it.
package realname

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// lookupTimeout bounds each external command.
const lookupTimeout = 3 * time.Second

// Detect returns the best display name for login, or "" when nothing better than the
// login itself is known.
func Detect(ctx context.Context, login string) string {
	return pick(login, gitUserName(ctx), osFullName(ctx, login))
}

// pick returns the first candidate that is a name: not empty, not the login, not an email.
func pick(login string, candidates ...string) string {
	for _, c := range candidates {
		if isName(c, login) {
			return strings.Join(strings.Fields(c), " ")
		}
	}
	return ""
}

// isName reports whether v looks like a person's name rather than a login or email.
func isName(v, login string) bool {
	v = strings.TrimSpace(v)
	if v == "" || strings.EqualFold(v, strings.TrimSpace(login)) {
		return false
	}
	if strings.Contains(v, "@") && !strings.Contains(v, " ") {
		return false // a plain email address
	}
	return true
}

// ParseGECOS returns the full name from a passwd line ("user:x:1000:1000:Bryan Sant,,,:/home/user:/bin/zsh")
// or from a bare GECOS field: the first comma-separated part.
func ParseGECOS(s string) string {
	s = strings.TrimSpace(s)
	if parts := strings.Split(s, ":"); len(parts) >= 7 {
		s = parts[4]
	}
	name, _, _ := strings.Cut(s, ",")
	// "&" stands for the capitalised login in old GECOS entries.
	return strings.TrimSpace(name)
}

func gitUserName(ctx context.Context) string {
	return run(ctx, "git", "config", "--global", "user.name")
}

func run(ctx context.Context, name string, args ...string) string {
	if _, err := exec.LookPath(name); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
