package sshutil

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SSHHost represents a host entry discovered from ~/.ssh/config.
type SSHHost struct {
	Alias    string `json:"alias"`
	HostName string `json:"hostname"`
	User     string `json:"user,omitempty"`
	Port     string `json:"port,omitempty"`
	Display  string `json:"display"` // e.g. "bsant@bocabox" or "daybreak"
}

// DiscoverHosts parses ~/.ssh/config on the controller host and returns non-wildcard host entries.
func DiscoverHosts() ([]SSHHost, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return []SSHHost{}, nil
	}
	return ParseSSHConfigFile(filepath.Join(home, ".ssh", "config"))
}

// ParseSSHConfigFile parses an OpenSSH client config file into SSHHost entries.
func ParseSSHConfigFile(path string) ([]SSHHost, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []SSHHost{}, nil
		}
		return nil, err
	}
	return ParseSSHConfig(bytes.NewReader(raw)), nil
}

// ParseSSHConfig parses OpenSSH client config lines from r.
func ParseSSHConfig(r *bytes.Reader) []SSHHost {
	var hosts []SSHHost
	seen := make(map[string]bool)
	var current []*SSHHost

	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Strip inline comments outside quotes (simple split)
		if idx := strings.Index(line, " #"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := strings.ToLower(fields[0])
		val := strings.Join(fields[1:], " ")

		if key == "host" {
			current = nil
			for _, pattern := range fields[1:] {
				if strings.ContainsAny(pattern, "*?!") {
					continue
				}
				if seen[pattern] {
					continue
				}
				seen[pattern] = true
				hosts = append(hosts, SSHHost{Alias: pattern})
				current = append(current, &hosts[len(hosts)-1])
			}
			continue
		}

		for _, h := range current {
			switch key {
			case "hostname":
				if h.HostName == "" {
					h.HostName = val
				}
			case "user":
				if h.User == "" {
					h.User = val
				}
			case "port":
				if h.Port == "" {
					h.Port = val
				}
			}
		}
	}

	for i := range hosts {
		target := hosts[i].HostName
		if target == "" {
			target = hosts[i].Alias
		}
		if hosts[i].User != "" {
			hosts[i].Display = hosts[i].User + "@" + target
		} else {
			hosts[i].Display = target
		}
	}

	sort.SliceStable(hosts, func(i, j int) bool {
		return strings.ToLower(hosts[i].Alias) < strings.ToLower(hosts[j].Alias)
	})
	return hosts
}
