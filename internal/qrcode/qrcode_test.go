package qrcode

import (
	"strings"
	"testing"
	"unicode/utf8"
)

const link = "http://192.168.1.20:8420/pair/7KQ2-M9TX"

func TestWriteTerminalIsCompact(t *testing.T) {
	for _, color := range []bool{false, true} {
		var b strings.Builder
		if err := WriteTerminal(&b, link, color); err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
		width := utf8.RuneCountInString(strings.NewReplacer("\x1b[30;107m", "", "\x1b[0m", "").Replace(lines[0]))
		// A ~40-character link is a version 3 code (29 modules) plus the margin.
		if width != 29+2*quiet || len(lines) != (width+1)/2 {
			t.Fatalf("color=%v: %d lines of %d columns", color, len(lines), width)
		}
		if width > 80 {
			t.Fatalf("QR wider than 80 columns: %d", width)
		}
	}
}

func TestEncodeSVG(t *testing.T) {
	s, err := EncodeSVG(link)
	if err != nil {
		t.Fatal(err)
	}
	if s.Size != 29+2*quiet || !strings.HasPrefix(s.Path, "M") || strings.Count(s.Path, "z") < 100 {
		t.Fatalf("svg: size %d, %d modules", s.Size, strings.Count(s.Path, "z"))
	}
}
