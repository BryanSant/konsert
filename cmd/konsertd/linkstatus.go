package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/worker"
)

// printLinkStatus adds the daemon's link to the hub to `konsertd status`, from the state
// file the running daemon keeps (<state dir>/run/<worker id>.link.json).
func printLinkStatus(w io.Writer, configPath string) {
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
		next := "now"
		if d := ls.NextAttempt.Sub(now); d > 0 {
			next = "in " + d.Round(time.Second).String()
		}
		fmt.Fprintf(w, "  Next try   : %s (%d failed so far)\n", next, ls.Attempts)
	}
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
