package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/BryanSant/syre/internal/controller"
	"github.com/spf13/cobra"
)

// syre open (SYR-1TDWB): the one action a desktop launcher runs. It starts the hub if it
// isn't running, makes sure this machine's syred is up, and opens the UI already signed
// in, with nothing to copy or type. Run again while everything is up, it only opens the
// UI (about a second). Its own log goes to <config dir>/logs/open.log, since a launcher
// shows no terminal.

const (
	hubStartTimeout = 30 * time.Second
	healthTimeout   = time.Second
)

// openURL opens a link with the desktop's default handler (tests replace it). With an
// installed Syre PWA, the web manifest's launch handler brings that window forward.
var openURL = func(link string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", link)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", link)
	default:
		cmd = exec.Command("xdg-open", link)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// startHubProcess starts `syre serve` in dir, detached, logging to logFile (tests
// replace it).
var startHubProcess = func(dir, logFile string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(self, "serve")
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = logf, logf
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

type openOptions struct {
	noBrowser bool
}

func newOpenCmd() *cobra.Command {
	var o openOptions
	cmd := &cobra.Command{
		Use:   "open",
		Short: "Start Syre if needed and open it, already signed in",
		Long: "Start the hub on this machine if it isn't running, make sure this machine's syred\n" +
			"is up, and open Syre in the installed app or the default browser, signed in.\n" +
			"Nothing to copy: the browser gets a single-use sign-in link that only works on\n" +
			"this machine and expires in a minute. Run it again to bring Syre back up front.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runOpen(cmd.Context(), cmd.OutOrStdout(), o)
		},
	}
	cmd.Flags().BoolVar(&o.noBrowser, "no-browser", false, "Print the sign-in link instead of opening it")
	return cmd
}

func runOpen(ctx context.Context, out io.Writer, o openOptions) error {
	dir, err := configDir()
	if err != nil {
		return err
	}
	logs := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		return err
	}
	logw := out
	if f, err := os.OpenFile(filepath.Join(logs, "open.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		defer f.Close()
		logw = io.MultiWriter(out, f)
	}
	logf := func(format string, args ...any) {
		fmt.Fprintf(logw, "%s "+format+"\n", append([]any{time.Now().Format(time.RFC3339)}, args...)...)
	}

	local := loadLocalHub()
	hubURL := cmpOrStr(local.URL, defaultHubURL)
	if !hubHealthy(ctx, hubURL) {
		data := filepath.Join(dir, "hub")
		if local.TokensFile != "" {
			data = filepath.Dir(local.TokensFile) // where the hub ran before
		}
		if err := os.MkdirAll(data, 0o700); err != nil {
			return err
		}
		logf("starting the hub in %s (log: %s)", data, filepath.Join(logs, "hub.log"))
		if err := startHubProcess(data, filepath.Join(logs, "hub.log")); err != nil {
			return fmt.Errorf("start the hub: %w", err)
		}
		deadline := time.Now().Add(hubStartTimeout)
		for {
			// The hub records where it listens once it is up.
			hubURL = cmpOrStr(loadLocalHub().URL, defaultHubURL)
			if hubHealthy(ctx, hubURL) {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("the hub didn't come up within %s: see %s", hubStartTimeout, filepath.Join(logs, "hub.log"))
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
	}

	client := newHubClient(resolveHub(hubURL, "", ""), auditSourceCLI)
	if note := ensureLocalMachine(ctx, client); note != "" {
		logf("%s", note)
	}
	var pc controller.PairingCode
	if err := client.DoJSON(ctx, http.MethodPost, "/api/pairing-codes", map[string]any{"launcher": true}, &pc); err != nil {
		return friendlyAPIError(fmt.Errorf("sign-in link: %w", err))
	}
	if o.noBrowser {
		fmt.Fprintln(out, pc.URL)
		return nil
	}
	logf("opening Syre at %s", hubURL)
	if err := openURL(pc.URL); err != nil {
		return fmt.Errorf("open the browser: %w (sign in at %s within a minute)", err, pc.URL)
	}
	return nil
}

// hubHealthy reports whether a hub answers at hubURL.
func hubHealthy(ctx context.Context, hubURL string) bool {
	hctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(hctx, http.MethodGet, hubURL+"/api/health", nil)
	if err != nil {
		return false
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_ = res.Body.Close()
	return res.StatusCode == http.StatusOK
}

// ensureLocalMachine starts this machine's syred through the hub when it should run and
// doesn't: the hub has no machine yet (first run), or this machine is one the hub
// launches locally and it is offline. A machine the user removed, or a fleet without
// this machine, is left alone. It returns a note for the log ("" when nothing to say).
func ensureLocalMachine(ctx context.Context, c *controller.RemoteFleetClient) string {
	fleet, err := c.FleetStatus(ctx)
	if err != nil {
		return "could not read the fleet: " + err.Error()
	}
	start := len(fleet.Workers) == 0
	for _, w := range fleet.Workers {
		if w.Launch == "local" {
			start = w.Status != "online"
			break
		}
	}
	if !start {
		return ""
	}
	if err := c.DoJSON(ctx, http.MethodPost, "/api/workers/local", struct{}{}, nil); err != nil {
		var apiErr *controller.APIError
		if errors.As(err, &apiErr) && apiErr.Message != "" {
			return "this machine's syred was not started: " + apiErr.Message
		}
		return "this machine's syred was not started: " + err.Error()
	}
	return "starting this machine's syred"
}

func cmpOrStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
