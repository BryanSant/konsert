package sshutil

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Target is a validated SSH destination of the form [user@]host[:port], where host
// is an ~/.ssh/config alias, a DNS name, an IPv4 address, or an IPv6 address
// (bracketed when a port is given).
type Target struct {
	User string
	Host string // never bracketed
	Port string // empty means the ssh default / ssh_config value
}

var (
	userRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
	hostRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
)

// ParseTarget validates a user-supplied SSH target. It rejects anything ssh could
// interpret as an option (a leading '-'), whitespace, control characters and shell or
// ssh_config metacharacters, so the value is safe as a positional argument.
func ParseTarget(raw string) (Target, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Target{}, fmt.Errorf("target host is required")
	}
	if len(s) > 255 {
		return Target{}, fmt.Errorf("target is too long")
	}
	if strings.HasPrefix(s, "-") {
		return Target{}, fmt.Errorf("target %q must not start with '-'", s)
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r > unicode.MaxASCII {
			return Target{}, fmt.Errorf("target %q contains whitespace, control or non-ASCII characters", s)
		}
	}

	var t Target
	hostPort := s
	if user, rest, ok := strings.Cut(s, "@"); ok {
		if !userRe.MatchString(user) {
			return Target{}, fmt.Errorf("target user %q is invalid", user)
		}
		t.User, hostPort = user, rest
	}

	switch {
	case strings.HasPrefix(hostPort, "["):
		end := strings.Index(hostPort, "]")
		if end < 0 {
			return Target{}, fmt.Errorf("target %q has an unterminated IPv6 address", s)
		}
		ip := net.ParseIP(hostPort[1:end])
		if ip == nil || ip.To4() != nil {
			return Target{}, fmt.Errorf("target %q has an invalid IPv6 address", s)
		}
		t.Host = hostPort[1:end]
		rest := hostPort[end+1:]
		if rest != "" {
			if !strings.HasPrefix(rest, ":") {
				return Target{}, fmt.Errorf("target %q is malformed", s)
			}
			t.Port = rest[1:]
		}
	case strings.Count(hostPort, ":") > 1:
		// Bare IPv6 without a port.
		ip := net.ParseIP(hostPort)
		if ip == nil {
			return Target{}, fmt.Errorf("target %q is malformed", s)
		}
		t.Host = hostPort
	default:
		host, port, hasPort := strings.Cut(hostPort, ":")
		if !hostRe.MatchString(host) {
			return Target{}, fmt.Errorf("target host %q is invalid", host)
		}
		t.Host = host
		if hasPort {
			t.Port = port
		}
	}
	if t.Port != "" {
		n, err := strconv.Atoi(t.Port)
		if err != nil || n < 1 || n > 65535 {
			return Target{}, fmt.Errorf("target port %q is invalid", t.Port)
		}
	}
	return t, nil
}

// Destination is the ssh positional destination ([user@]host, IPv6 unbracketed).
func (t Target) Destination() string {
	if t.User != "" {
		return t.User + "@" + t.Host
	}
	return t.Host
}

// SCPDestination is the scp remote argument for path ([user@]host:path; IPv6 bracketed).
func (t Target) SCPDestination(path string) string {
	host := t.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if t.User != "" {
		host = t.User + "@" + host
	}
	return host + ":" + path
}

// String renders the target as the operator typed it (normalized).
func (t Target) String() string {
	host := t.Host
	if t.Port != "" {
		host = net.JoinHostPort(t.Host, t.Port)
	}
	if t.User != "" {
		return t.User + "@" + host
	}
	return host
}

// baseSSHOptions are applied to every ssh/scp invocation. StrictHostKeyChecking=yes
// means unknown host keys are never trusted implicitly; see BootstrapOptions.AcceptHostKey.
var baseSSHOptions = []string{
	"-o", "BatchMode=yes",
	"-o", "ConnectTimeout=8",
	"-o", "StrictHostKeyChecking=yes",
}

// sshArgs builds the ssh argv. "--" separates options from the destination so a
// target can never be parsed as an option.
func sshArgs(t Target, extraOpts []string, remoteCmd string) []string {
	args := append([]string{}, baseSSHOptions...)
	args = append(args, extraOpts...)
	if t.Port != "" {
		args = append(args, "-p", t.Port)
	}
	return append(args, "--", t.Destination(), remoteCmd)
}

// scpArgs builds the scp argv, with "--" before the source and destination operands.
func scpArgs(t Target, extraOpts []string, src, remotePath string) []string {
	args := append([]string{}, baseSSHOptions...)
	args = append(args, extraOpts...)
	if t.Port != "" {
		args = append(args, "-P", t.Port)
	}
	return append(args, "--", src, t.SCPDestination(remotePath))
}
