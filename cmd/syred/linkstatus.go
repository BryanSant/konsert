package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker"
)

// printLinkStatus adds the daemon's link to the hub to `syred status`, from the state
// file the running daemon keeps (<state dir>/run/<worker id>.link.json).
func printLinkStatus(w io.Writer, configPath, name string) {
	path := ""
	if configPath != "" {
		if _, err := os.Stat(configPath); err == nil {
			path = configPath
		}
	}
	cfg, err := config.LoadWorkerConfig(path)
	if err != nil {
		return
	}
	if path == "" && name != "" {
		cfg.Name = name // `status --name` of an instance run by hand, without a config file
	}
	id, ok := worker.LookupWorkerID(cfg)
	if !ok {
		fmt.Fprintf(w, "  Link       : unknown (this machine has no worker identity yet)\n")
		return
	}
	ls, err := worker.ReadLinkStatus("", id)
	if err != nil {
		fmt.Fprintf(w, "  Link       : unknown (no link state from the daemon)\n")
		return
	}
	writeLinkStatus(w, ls, time.Now(), processAlive(ls.PID))
}

// daemonAnswering reports whether the instance's daemon is up whatever its start
// mechanism says: it accepts connections on its local socket (the one the hub attaches
// to; a bare connect, no attach), or the process its link state names is alive.
func daemonAnswering(ctx context.Context, configPath, name string) bool {
	path := ""
	if configPath != "" {
		if _, err := os.Stat(configPath); err == nil {
			path = configPath
		}
	}
	cfg, err := config.LoadWorkerConfig(path)
	if err != nil {
		return false
	}
	if path == "" && name != "" {
		cfg.Name = name
	}
	id, ok := worker.LookupWorkerID(cfg)
	if !ok {
		return false
	}
	if sock := worker.LocalSocketPath("", id); sock != "" {
		d := net.Dialer{Timeout: time.Second}
		if c, err := d.DialContext(ctx, "unix", sock); err == nil {
			_ = c.Close()
			return true
		}
	}
	ls, err := worker.ReadLinkStatus("", id)
	return err == nil && processAlive(ls.PID)
}

func writeLinkStatus(w io.Writer, ls worker.LinkStatus, now time.Time, alive bool) {
	ago := func(t time.Time) string {
		return t.Local().Format("2006-01-02 15:04:05") + " (" + agoText(now.Sub(t)) + ")"
	}
	if !alive {
		fmt.Fprintf(w, "  Link       : not running (pid %d is gone; last state %s since %s)\n", ls.PID, ls.State, ago(ls.Since))
		if ls.LastError != "" {
			fmt.Fprintf(w, "  Last error : %s\n", ls.LastError)
		}
		return
	}
	fmt.Fprintf(w, "  Link       : %s\n", ls.State)
	transport := ls.Transport
	if ls.Peer != "" && ls.Peer != ls.Transport {
		transport += " (" + ls.Peer + ")"
	}
	fmt.Fprintf(w, "  Transport  : %s\n", transport)
	fmt.Fprintf(w, "  Since      : %s\n", ago(ls.Since))
	if ls.LastError != "" {
		fmt.Fprintf(w, "  Last error : %s\n", ls.LastError)
	}
	if !ls.NextAttempt.IsZero() {
		next := "dialing now"
		if d := ls.NextAttempt.Sub(now); d > 0 {
			next = "in " + d.Round(time.Second).String()
		}
		fmt.Fprintf(w, "  Next try   : %s (%d failed so far)\n", next, ls.Attempts)
	}
	writeSpoolStatus(w, ls.Spool, now)
}

// writeSpoolStatus adds the outbound spool (what is kept for the hub) to `syred status`.
func writeSpoolStatus(w io.Writer, sp *protocol.SpoolStatus, now time.Time) {
	if sp == nil {
		return
	}
	line := fmt.Sprintf("%s of %s", mb(sp.Bytes), mb(sp.BudgetBytes))
	if sp.Records > 0 {
		line += fmt.Sprintf(", %d records not yet acknowledged", sp.Records)
	}
	if !sp.OldestUnsentAt.IsZero() {
		line += ", oldest unsent " + agoText(now.Sub(sp.OldestUnsentAt))
	} else {
		line += ", everything sent"
	}
	fmt.Fprintf(w, "  Spool      : %s\n", line)
	if sp.Replaying {
		fmt.Fprintf(w, "  Replay     : %d of %d records sent\n", sp.ReplaySent, sp.ReplayTotal)
	}
	if sp.Full {
		fmt.Fprintf(w, "  SPOOL FULL : %s: vitals and activity are no longer kept (%d dropped); transcripts, task events, statuses and questions still are\n", sp.FullReason, sp.Dropped)
	}
	if sp.Error != "" {
		fmt.Fprintf(w, "  Spool error: %s (messages are sent without being kept)\n", sp.Error)
	}
}

func mb(n int64) string {
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

func agoText(d time.Duration) string {
	switch {
	case d < time.Second:
		return "just now"
	case d < time.Hour:
		return d.Round(time.Second).String() + " ago"
	default:
		return d.Round(time.Minute).String() + " ago"
	}
}
