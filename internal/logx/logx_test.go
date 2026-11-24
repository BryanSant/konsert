package logx

import (
	"bytes"
	"log/slog"
	"testing"
)

func TestConsoleHandler(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(NewConsoleHandler(&buf, slog.LevelInfo, false))
	l.Debug("hidden")
	l.With("worker", "desk").WithGroup("g").Warn("no fleet token", "addr", ":8420", "note", "two words")
	got := buf.String()
	want := " WARN no fleet token worker=desk g.addr=:8420 g.note=\"two words\"\n"
	if len(got) != len("15:04:05")+len(want) || got[8:] != want {
		t.Fatalf("got %q, want hh:mm:ss%q", got, want)
	}
}
