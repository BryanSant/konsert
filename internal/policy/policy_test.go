package policy

import (
	"strings"
	"testing"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

func base() State {
	return State{Work: protocol.WorkTask, Hosted: 0, LoadAvg: 1, CPUs: 8, AvailRAMMB: 8000, SurvivalReserveMB: 800,
		Now: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)} // a Wednesday
}

func TestCheckRules(t *testing.T) {
	cases := []struct {
		name string
		p    protocol.MachinePolicy
		st   func(*State)
		rule string
	}{
		{"no rules", protocol.MachinePolicy{}, nil, ""},
		{"control only", protocol.MachinePolicy{Role: protocol.MachineRoleControlOnly}, nil, RuleRole},
		{"interactive runs work", protocol.MachinePolicy{Role: protocol.MachineRoleInteractive}, nil, ""},
		{"work class refused", protocol.MachinePolicy{AllowedWork: []string{protocol.WorkChat}}, nil, RuleAllowedWork},
		{"work class allowed", protocol.MachinePolicy{AllowedWork: []string{protocol.WorkChat, protocol.WorkTask}}, nil, ""},
		{"sessions under cap", protocol.MachinePolicy{MaxSessions: 2}, func(s *State) { s.Hosted = 1 }, ""},
		{"sessions at cap", protocol.MachinePolicy{MaxSessions: 2}, func(s *State) { s.Hosted = 2 }, RuleMaxSessions},
		{"load over", protocol.MachinePolicy{MaxLoad: 4}, func(s *State) { s.LoadAvg = 4.5 }, RuleMaxLoad},
		{"load unknown (Windows)", protocol.MachinePolicy{MaxLoad: 4, ReserveCPU: 2}, func(s *State) { s.LoadAvg = 0 }, ""},
		{"cores kept for the user", protocol.MachinePolicy{ReserveCPU: 2}, func(s *State) { s.LoadAvg = 6.5 }, RuleReserveCPU},
		{"cores to spare", protocol.MachinePolicy{ReserveCPU: 2}, func(s *State) { s.LoadAvg = 5.5 }, ""},
		{"RAM above the raised floor", protocol.MachinePolicy{ReserveRAMMB: 4000, MinFreeRAMMB: 1000}, func(s *State) { s.AvailRAMMB = 5900 }, ""},
		{"RAM below the raised floor", protocol.MachinePolicy{ReserveRAMMB: 4000, MinFreeRAMMB: 1000}, func(s *State) { s.AvailRAMMB = 5700 }, RuleFreeRAM},
		{"RAM unknown", protocol.MachinePolicy{ReserveRAMMB: 4000}, func(s *State) { s.AvailRAMMB = -1 }, ""},
	}
	for _, c := range cases {
		st := base()
		if c.st != nil {
			c.st(&st)
		}
		got := Check(&c.p, st)
		switch {
		case c.rule == "" && got != nil:
			t.Errorf("%s: refused: %s", c.name, got.Message)
		case c.rule != "" && (got == nil || got.Rule != c.rule):
			t.Errorf("%s: got %+v, want rule %s", c.name, got, c.rule)
		case got != nil && !strings.HasPrefix(got.Error(), "machine policy: "):
			t.Errorf("%s: message %q", c.name, got.Error())
		}
	}
	if Check(nil, base()) != nil {
		t.Fatal("a nil policy refused work")
	}
}

func TestQuietHours(t *testing.T) {
	p := &protocol.MachinePolicy{TimeZone: "America/Denver", QuietHours: []protocol.QuietWindow{
		{Start: "22:00", End: "07:00", Days: []string{"mon", "tue", "wed", "thu", "fri"}},
		{Start: "12:00", End: "13:00"},
	}}
	den, _ := time.LoadLocation("America/Denver")
	at := func(day, hh, mm int) time.Time { return time.Date(2026, 11, day, hh, mm, 0, 0, den) }
	cases := []struct {
		at    time.Time
		quiet bool
	}{
		{at(25, 23, 0), true},  // Wed 23:00
		{at(26, 6, 59), true},  // Thu 06:59, window started Wed
		{at(26, 7, 0), false},  // Thu 07:00, over
		{at(28, 23, 0), false}, // Sat 23:00: weekdays only
		{at(30, 2, 0), false},  // Mon 02:00: started Sun
		{at(29, 12, 30), true}, // Sun lunch: every day
		{at(25, 21, 59), false},
	}
	for _, c := range cases {
		if _, q := InQuietHours(p, c.at.UTC()); q != c.quiet {
			t.Errorf("%s: quiet=%v, want %v", c.at.Format("Mon 15:04"), q, c.quiet)
		}
	}
	st := base()
	st.Now = at(25, 23, 30)
	if v := Check(p, st); v == nil || v.Rule != RuleQuietHours {
		t.Fatalf("quiet hours not enforced: %+v", v)
	}
}

func TestValidate(t *testing.T) {
	bad := []protocol.MachinePolicy{
		{Role: "server"},
		{MaxSessions: -1},
		{AllowedWork: []string{"build"}},
		{QuietHours: []protocol.QuietWindow{{Start: "22:00", End: "07:00"}}}, // no zone
		{TimeZone: "Mars/Olympus", QuietHours: []protocol.QuietWindow{{Start: "22:00", End: "07:00"}}},
		{TimeZone: "UTC", QuietHours: []protocol.QuietWindow{{Start: "25:00", End: "07:00"}}},
		{TimeZone: "UTC", QuietHours: []protocol.QuietWindow{{Start: "09:00", End: "09:00"}}},
		{TimeZone: "UTC", QuietHours: []protocol.QuietWindow{{Start: "22:00", End: "07:00", Days: []string{"monday"}}}},
	}
	for _, p := range bad {
		if Validate(&p) == nil {
			t.Errorf("accepted %+v", p)
		}
	}
	good := protocol.MachinePolicy{Role: protocol.MachineRoleInteractive, MaxSessions: 2, ReserveRAMMB: 4096,
		AllowedWork: []string{protocol.WorkChat}, TimeZone: "UTC",
		QuietHours: []protocol.QuietWindow{{Start: "22:00", End: "07:00", Days: []string{"fri"}}}}
	if err := Validate(&good); err != nil {
		t.Fatal(err)
	}
	if ExtraReserveMB(&protocol.MachinePolicy{MinFreeRAMMB: 512, ReserveRAMMB: 4096}) != 4608 || ExtraReserveMB(nil) != 0 {
		t.Fatal("extra reserve")
	}
}

func TestLiftedRules(t *testing.T) {
	p := &protocol.MachinePolicy{MaxSessions: 1, AllowedWork: []string{protocol.WorkChat}}
	st := base()
	st.Hosted = 3
	if v := Check(p, st); v == nil || v.Rule != RuleAllowedWork {
		t.Fatalf("not lifted: %+v", v)
	}
	st.Lifted = map[string]bool{RuleAllowedWork: true}
	if v := Check(p, st); v == nil || v.Rule != RuleMaxSessions {
		t.Fatalf("one lifted, the next rule decides: %+v", v)
	}
	st.Lifted[RuleMaxSessions] = true
	if v := Check(p, st); v != nil {
		t.Fatalf("both lifted: %+v", v)
	}
}

func TestInteractiveMachineHoldsAutomaticWorkWhileInUse(t *testing.T) {
	p := &protocol.MachinePolicy{Role: protocol.MachineRoleInteractive}
	base := State{LoadAvg: -1, AvailRAMMB: -1, Now: time.Now()}
	st := base
	st.Automatic, st.UserIdleS = true, 40
	if v := Check(p, st); v == nil || v.Rule != RuleUserActive {
		t.Fatalf("in use: %+v", v)
	}
	st.UserIdleS = InteractiveAwayS + 1
	if v := Check(p, st); v != nil {
		t.Fatalf("away: %+v", v)
	}
	st.UserIdleS, st.Automatic = 5, false // the user's own work
	if v := Check(p, st); v != nil {
		t.Fatalf("the user's own work: %+v", v)
	}
	st.Automatic, st.UserIdleS = true, -1 // idle time unknown (a server over SSH)
	if v := Check(p, st); v != nil {
		t.Fatalf("unknown idle: %+v", v)
	}
	st.UserIdleS = 5
	st.Lifted = map[string]bool{RuleUserActive: true}
	if v := Check(p, st); v != nil {
		t.Fatalf("lifted by an override: %+v", v)
	}
	if v := Check(&protocol.MachinePolicy{Role: protocol.MachineRoleDedicated}, State{Automatic: true, UserIdleS: 5, LoadAvg: -1, AvailRAMMB: -1}); v != nil {
		t.Fatalf("dedicated: %+v", v)
	}
}
