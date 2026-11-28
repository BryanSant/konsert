package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/BryanSant/syre/internal/policy"
	"github.com/BryanSant/syre/internal/protocol"
)

func TestSetPolicyKeys(t *testing.T) {
	var p protocol.MachinePolicy
	for _, kv := range []string{
		"role=interactive", "max_sessions=2", "reserve_cpu=4", "reserve_ram_mb=8192", "allowed_work=chat, ticket",
		"quiet_hours=22:00-07:00@Mon,tue; 12:00-13:00", "time_zone=America/Denver",
	} {
		if err := setPolicyKey(&p, kv); err != nil {
			t.Fatal(err)
		}
	}
	if err := policy.Validate(&p); err != nil {
		t.Fatal(err)
	}
	if p.Role != "interactive" || p.MaxSessions != 2 || p.ReserveCPU != 4 || p.ReserveRAMMB != 8192 ||
		strings.Join(p.AllowedWork, ",") != "chat,ticket" || len(p.QuietHours) != 2 ||
		strings.Join(p.QuietHours[0].Days, ",") != "mon,tue" || p.QuietHours[1].Start != "12:00" {
		t.Fatalf("policy = %+v", p)
	}
	if err := setPolicyKey(&p, "max_sessions="); err != nil || p.MaxSessions != 0 {
		t.Fatalf("clearing a key: %v %+v", err, p)
	}
	var out bytes.Buffer
	printPolicy(&out, "box", p)
	if !strings.Contains(out.String(), "reserve_ram_mb   8192") || !strings.Contains(out.String(), "(America/Denver)") {
		t.Fatalf("printed:\n%s", out.String())
	}
	for _, bad := range []string{"nokey", "colour=red", "max_load=high", "quiet_hours=22:00"} {
		if setPolicyKey(&protocol.MachinePolicy{}, bad) == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	out.Reset()
	printPolicy(&out, "box", protocol.MachinePolicy{})
	if !strings.Contains(out.String(), "no policy") {
		t.Fatalf("empty printed %q", out.String())
	}
}
