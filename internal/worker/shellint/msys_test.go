package shellint

import (
	"testing"

	"github.com/BryanSant/syre/internal/protocol"
)

func TestTranslateMSYSPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"/", "/"},
		{"/c", `C:\`},
		{"/c/", `C:\`},
		{"/C", `C:\`},
		{"/C/", `C:\`},
		{"/c/Users/alice/repo", `C:\Users\alice\repo`},
		{"/d/Projects/my app/sub dir/", `D:\Projects\my app\sub dir`},
		{"//server/share", `\\server\share`},
		{"//server/share/dir/sub", `\\server\share\dir\sub`},
		{"///triple", "///triple"},
		{"/tmp", "/tmp"},
		{"/usr/bin", "/usr/bin"},
		{"/cfoo/bar", "/cfoo/bar"},
		{"/1/foo", "/1/foo"},
		{`C:\Users\alice`, `C:\Users\alice`},
	}
	for _, tc := range cases {
		if got := TranslateMSYSPath(tc.in); got != tc.want {
			t.Errorf("TranslateMSYSPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTrackerTranslatesMSYSCwd(t *testing.T) {
	const nonce = "n1"
	runOne := func(translate bool) (protocol.OperatorCommandPayload, Command) {
		hist := NewHistory()
		var got []protocol.OperatorCommandPayload
		tr := NewTracker("t1", nonce, hist, func(p protocol.OperatorCommandPayload) {
			got = append(got, p)
		})
		tr.SetTranslateMSYS(translate)
		tr.Handle([]Mark{
			{Kind: MarkCwd, Value: "/c/Users/alice/repo", Nonce: nonce},
			{Kind: MarkCommandLine, Value: "git status", Nonce: nonce},
			{Kind: MarkPreExec},
			{Kind: MarkCommandDone, ExitCode: 0, HasExit: true},
		})
		if len(got) != 2 {
			t.Fatalf("got %d events, want 2", len(got))
		}
		full, ok := hist.Get(got[0].CommandID)
		if !ok {
			t.Fatal("command missing from history")
		}
		return got[0], full
	}

	ev, full := runOne(true)
	if ev.Cwd != `C:\Users\alice\repo` || full.Cwd != `C:\Users\alice\repo` {
		t.Fatalf("translated cwd: ev=%q full=%q", ev.Cwd, full.Cwd)
	}

	ev, full = runOne(false)
	if ev.Cwd != "/c/Users/alice/repo" || full.Cwd != "/c/Users/alice/repo" {
		t.Fatalf("untranslated cwd: ev=%q full=%q", ev.Cwd, full.Cwd)
	}
}
