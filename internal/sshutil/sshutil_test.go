package sshutil

import (
	"bytes"
	"testing"
)

func TestParseSSHConfig(t *testing.T) {
	sample := `
# Personal homelab hosts
Host *
    ServerAliveInterval 60

Host bocabox
    HostName bocabox.local
    User bsant

Host daybreak-wsl
    HostName daybreak
    User bsant
    Port 2222

Host starscream
    HostName starscream.gyggly.com
`
	hosts := ParseSSHConfig(bytes.NewReader([]byte(sample)))
	if len(hosts) != 3 {
		t.Fatalf("expected 3 non-wildcard hosts, got %d: %+v", len(hosts), hosts)
	}
	if hosts[0].Alias != "bocabox" || hosts[0].Display != "bsant@bocabox.local" {
		t.Fatalf("unexpected first host: %+v", hosts[0])
	}
	if hosts[1].Alias != "daybreak-wsl" || hosts[1].Port != "2222" {
		t.Fatalf("unexpected second host: %+v", hosts[1])
	}
	if hosts[2].Alias != "starscream" || hosts[2].Display != "starscream.gyggly.com" {
		t.Fatalf("unexpected third host: %+v", hosts[2])
	}
}
