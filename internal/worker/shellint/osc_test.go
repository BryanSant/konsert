package shellint

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

const testNonce = "N0NCE"

func osc(body string) string { return "\x1b]" + body + "\a" }

// feedAll feeds input to a parser in the given chunk sizes (cycled) and returns the
// forwarded bytes and marks.
func feedAll(input string, sizes ...int) (string, []Mark) {
	var p Parser
	var out bytes.Buffer
	var marks []Mark
	data := []byte(input)
	for i, k := 0, 0; i < len(data); k++ {
		n := sizes[k%len(sizes)]
		if i+n > len(data) {
			n = len(data) - i
		}
		o, m := p.Feed(data[i : i+n])
		out.Write(o)
		marks = append(marks, m...)
		i += n
	}
	out.Write(p.Flush())
	return out.String(), marks
}

func TestParserExtractsMarksAndStripsSyreOSC(t *testing.T) {
	cmd := "echo 'a;b' \\ héllo 🎉\n"
	input := "prompt" + osc("633;P;Cwd=/home/u/x\\x3by;"+testNonce) + osc("133;A") + "$ " + osc("133;B") +
		osc("633;E;"+escapeForTest(cmd)+";"+testNonce) + osc("133;C") +
		"output\r\n" + osc("133;D;42") + "\x1b]0;title\x1b\\" + "\x1b[31mred\x1b[0m"
	wantOut := "prompt" + osc("133;A") + "$ " + osc("133;B") + osc("133;C") + "output\r\n" + osc("133;D;42") +
		"\x1b]0;title\x1b\\" + "\x1b[31mred\x1b[0m"
	wantMarks := []Mark{
		{Kind: MarkCwd, Value: "/home/u/x;y", Nonce: testNonce},
		{Kind: MarkPromptStart},
		{Kind: MarkPromptEnd},
		{Kind: MarkCommandLine, Value: cmd, Nonce: testNonce},
		{Kind: MarkPreExec},
		{Kind: MarkCommandDone, ExitCode: 42, HasExit: true},
	}
	// Every split point, including inside multi-byte runes and escape sequences.
	for _, sizes := range [][]int{{len(input)}, {1}, {2}, {3}, {5, 1, 7}, {13}, {64}} {
		out, marks := feedAll(input, sizes...)
		if out != wantOut {
			t.Errorf("sizes %v: out\n got %q\nwant %q", sizes, out, wantOut)
		}
		if !reflect.DeepEqual(marks, wantMarks) {
			t.Errorf("sizes %v: marks\n got %+v\nwant %+v", sizes, marks, wantMarks)
		}
	}
}

func TestParserSTTerminatorAndPartialPrefixes(t *testing.T) {
	input := "\x1b]633;E;ls;" + testNonce + "\x1b\\" + // ST-terminated 633: stripped
		"\x1b]63x\a" + // looks like 633 for two bytes: passed through
		"\x1b]6\a" + // short OSC: passed through
		"\x1b[?25h" + // CSI
		"\x1b\x1b]133;D\a" + // ESC ESC: first ESC passes, then an OSC
		"\x1b]133;D;notanumber\a"
	want := "\x1b]63x\a" + "\x1b]6\a" + "\x1b[?25h" + "\x1b\x1b]133;D\a" + "\x1b]133;D;notanumber\a"
	for _, n := range []int{1, 2, 4, len(input)} {
		out, marks := feedAll(input, n)
		if out != want {
			t.Errorf("chunk %d: out\n got %q\nwant %q", n, out, want)
		}
		wantMarks := []Mark{
			{Kind: MarkCommandLine, Value: "ls", Nonce: testNonce},
			{Kind: MarkCommandDone},
			{Kind: MarkCommandDone},
		}
		if !reflect.DeepEqual(marks, wantMarks) {
			t.Errorf("chunk %d: marks %+v", n, marks)
		}
	}
}

func TestParserNestedEscapesAbortAndRestart(t *testing.T) {
	// An ESC inside a 633 sequence that is not ST aborts it (as in xterm); the ESC starts
	// the next sequence, which is processed normally.
	input := "\x1b]633;E;partial\x1b[1mbold" + osc("633;E;ok;"+testNonce) +
		"\x1b]633;E;x\x18after" + // CAN aborts and is swallowed with the 633 sequence
		"\x1b]2;t\x18z" // CAN aborts a foreign OSC and passes through
	want := "\x1b[1mbold" + "after" + "\x1b]2;t\x18z"
	for _, n := range []int{1, 3, len(input)} {
		out, marks := feedAll(input, n)
		if out != want {
			t.Errorf("chunk %d: out\n got %q\nwant %q", n, out, want)
		}
		if len(marks) != 1 || marks[0].Value != "ok" {
			t.Errorf("chunk %d: marks %+v", n, marks)
		}
	}
}

func TestParserLongLines(t *testing.T) {
	long := strings.Repeat("x", 100_000)
	input := osc("633;E;"+long+";"+testNonce) + "tail"
	out, marks := feedAll(input, 4096)
	if out != "tail" || len(marks) != 1 || marks[0].Value != long {
		t.Fatalf("long command: out %q, %d marks", out[:min(len(out), 20)], len(marks))
	}

	// A foreign OSC over the payload cap passes through intact and is not buffered.
	huge := "\x1b]1337;File=" + strings.Repeat("A", maxOSCPayload+10) + "\a" + "after"
	out, _ = feedAll(huge, 8192)
	if out != huge {
		t.Fatalf("over-long foreign OSC altered (len %d vs %d)", len(out), len(huge))
	}

	// An over-long 633 sequence is dropped without being buffered whole.
	var p Parser
	_, _ = p.Feed([]byte("\x1b]633;E;"))
	for range 10 {
		_, _ = p.Feed(bytes.Repeat([]byte("y"), maxOSCPayload/4))
	}
	if len(p.payload) > maxOSCPayload {
		t.Fatalf("payload grew to %d", len(p.payload))
	}
	o, m := p.Feed([]byte(";" + testNonce + "\adone"))
	if string(o) != "done" || len(m) != 0 {
		t.Fatalf("after over-long 633: out %q marks %+v", o, m)
	}
}

func TestParserFlushReleasesPartialForeignSequence(t *testing.T) {
	var p Parser
	o, _ := p.Feed([]byte("abc\x1b]63"))
	if string(o) != "abc" {
		t.Fatalf("held too little/much: %q", o)
	}
	if got := string(p.Flush()); got != "\x1b]63" {
		t.Fatalf("flush = %q", got)
	}
	_, _ = p.Feed([]byte("\x1b]633;E;secret"))
	if got := p.Flush(); len(got) != 0 {
		t.Fatalf("partial 633 must be dropped, got %q", got)
	}
}

func TestUnescape(t *testing.T) {
	for in, want := range map[string]string{
		`plain`:            "plain",
		`a\\b`:             `a\b`,
		`a\x3bb\x0a`:       "a;b\n",
		`trailing\`:        `trailing\`,
		`bad\xZZ`:          `bad\xZZ`,
		`short\x4`:         `short\x4`,
		"h\xc3\xa9":        "hé",
		`\xff`:             "\uFFFD",
		`\xe2\x9c\x93 ok`:  "✓ ok",
		`C:\\Users\\bryan`: `C:\Users\bryan`,
	} {
		if got := Unescape(in); got != want {
			t.Errorf("Unescape(%q) = %q, want %q", in, got, want)
		}
	}
}

// escapeForTest mirrors the shell scripts' escaping.
func escapeForTest(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			b.WriteString(`\\`)
		case c == ';':
			b.WriteString(`\x3b`)
		case c < 0x20 || c == 0x7f:
			b.WriteString(`\x`)
			b.WriteString(strings.ToLower(string("0123456789ABCDEF"[c>>4]) + string("0123456789ABCDEF"[c&15])))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
