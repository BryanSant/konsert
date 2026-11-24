// Package logx builds the slog handlers the konsert and konsertd commands log with.
package logx

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-isatty"
)

// New returns a logger for w. A terminal gets short console lines
// ("20:29:56 WARN message key=value"); anything else (a file, a pipe, a service log)
// gets slog's standard key=value text with full timestamps.
func New(w io.Writer, level slog.Level) *slog.Logger {
	if f, ok := w.(*os.File); ok && (isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())) {
		return slog.New(NewConsoleHandler(w, level, os.Getenv("NO_COLOR") == ""))
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

// ConsoleHandler writes compact, human-oriented log lines.
type ConsoleHandler struct {
	mu     *sync.Mutex
	w      io.Writer
	level  slog.Level
	color  bool
	attrs  []slog.Attr
	groups []string
}

// NewConsoleHandler returns a ConsoleHandler writing to w at or above level.
func NewConsoleHandler(w io.Writer, level slog.Level, color bool) *ConsoleHandler {
	return &ConsoleHandler{mu: &sync.Mutex{}, w: w, level: level, color: color}
}

// Enabled implements slog.Handler.
func (h *ConsoleHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

// Handle implements slog.Handler.
func (h *ConsoleHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	t := r.Time
	if t.IsZero() {
		t = time.Now()
	}
	b.WriteString(t.Format("15:04:05"))
	b.WriteByte(' ')
	b.WriteString(h.levelText(r.Level))
	b.WriteByte(' ')
	b.WriteString(r.Message)
	prefix := ""
	if len(h.groups) > 0 {
		prefix = strings.Join(h.groups, ".") + "."
	}
	for _, a := range h.attrs {
		writeAttr(&b, "", a)
	}
	r.Attrs(func(a slog.Attr) bool {
		writeAttr(&b, prefix, a)
		return true
	})
	b.WriteByte('\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *ConsoleHandler) levelText(l slog.Level) string {
	s, code := "INFO", ""
	switch {
	case l >= slog.LevelError:
		s, code = "ERROR", "31"
	case l >= slog.LevelWarn:
		s, code = "WARN", "33"
	case l < slog.LevelInfo:
		s, code = "DEBUG", "2"
	}
	if h.color && code != "" {
		return "\x1b[" + code + "m" + s + "\x1b[0m"
	}
	return s
}

func writeAttr(b *strings.Builder, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p += a.Key + "."
		}
		for _, g := range a.Value.Group() {
			writeAttr(b, p, g)
		}
		return
	}
	v := a.Value.String()
	if v == "" || strings.ContainsAny(v, " \t\"=") {
		v = fmt.Sprintf("%q", v)
	}
	b.WriteByte(' ')
	b.WriteString(prefix + a.Key)
	b.WriteByte('=')
	b.WriteString(v)
}

// WithAttrs implements slog.Handler.
func (h *ConsoleHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	prefix := ""
	if len(h.groups) > 0 {
		prefix = strings.Join(h.groups, ".") + "."
	}
	c.attrs = slices.Clone(h.attrs)
	for _, a := range attrs {
		c.attrs = append(c.attrs, slog.Attr{Key: prefix + a.Key, Value: a.Value})
	}
	return &c
}

// WithGroup implements slog.Handler.
func (h *ConsoleHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.groups = append(slices.Clone(h.groups), name)
	return &c
}
