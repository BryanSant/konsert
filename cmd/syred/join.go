package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/worker"
	"github.com/spf13/cobra"
)

// newJoinCmd is `syred join <hub-url> [code]` (SYR-RK9YD): trade a short-lived join
// code from Add machine → Manual for this machine's own credential, save it, then
// install syred to start automatically (or run it here with --foreground).
func newJoinCmd() *cobra.Command {
	var (
		o          runOptions
		f          installFlags
		foreground bool
	)
	cmd := &cobra.Command{
		Use:   "join <hub-url> [code]",
		Short: "Add this machine to a Syre hub with a join code",
		Long: `Add this machine to a Syre hub. Get a code from the hub's Add machine →
Manual tab (it works once, for 15 minutes) and run the command shown there.

join trades the code for this machine's own credential, saves it in ~/.syre
(readable only by you), and installs syred to start automatically, exactly like
` + "`syred install`" + ` with the same flags. --foreground runs it in this terminal
instead. Without a code argument, join reads it from stdin.`,
		Example: `  syred join https://desk.example.ts.net ABCD-EFGH
  syred join http://desk:8420 ABCD-EFGH --sandbox
  syred join http://desk:8420 ABCD-EFGH --foreground`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			hub, wsURL, err := parseHubURL(args[0])
			if err != nil {
				return err
			}
			code := ""
			if len(args) == 2 {
				code = args[1]
			} else {
				fmt.Fprint(cmd.ErrOrStderr(), "Join code: ")
				if code, err = readLine(bufio.NewReader(cmd.InOrStdin())); err != nil {
					return fmt.Errorf("read the join code: %w", err)
				}
			}
			fs := cmd.Flags()
			if !fs.Changed("controller") {
				if err := fs.Set("controller", wsURL); err != nil {
					return err
				}
			}
			cfg, err := o.resolve(fs)
			if err != nil {
				return err
			}
			workerID := worker.WorkerIDFor(cfg)
			host, _ := os.Hostname()
			res, err := requestJoin(cmd.Context(), hub, joinRequest{
				Code: code, WorkerID: workerID, Name: cfg.Name, HostGroup: cfg.HostGroup,
				Hostname: host, OS: runtime.GOOS, Arch: runtime.GOARCH,
			})
			if err != nil {
				return err
			}
			if err := worker.SaveCredential("", workerID, res.Credential); err != nil {
				return fmt.Errorf("save the machine credential: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Joined %s as %s (%s).\n", hub, res.Name, res.WorkerSID)
			if foreground {
				cfg.Credential = res.Credential
				return runWorker(cmd, &o, cfg)
			}
			return runInstall(cmd, &f)
		},
	}
	o.bind(cmd.Flags())
	f.bind(cmd.Flags())
	cmd.Flags().BoolVar(&foreground, "foreground", false, "Run syred in this terminal instead of installing it")
	for _, name := range []string{"status", "credential-stdin", "id", "dry-run"} {
		_ = cmd.Flags().MarkHidden(name)
	}
	return cmd
}

// parseHubURL accepts the hub's base URL (http(s)://host[:port][/path], or a bare
// host[:port] meaning http) and returns it with the worker WebSocket URL.
func parseHubURL(raw string) (base, ws string, err error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", "", fmt.Errorf("hub URL %q: want http(s)://host[:port]", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("hub URL %q: want only scheme, host, port and path", raw)
	}
	path := strings.TrimRight(u.Path, "/")
	base = u.Scheme + "://" + u.Host + path
	scheme := "ws"
	if u.Scheme == "https" {
		scheme = "wss"
	}
	return base, scheme + "://" + u.Host + path + "/ws/worker", nil
}

type joinRequest struct {
	Code      string `json:"code"`
	WorkerID  string `json:"worker_id"`
	Name      string `json:"name"`
	HostGroup string `json:"host_group"`
	Hostname  string `json:"hostname"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

type joinResponse struct {
	Credential string `json:"credential"`
	WorkerID   string `json:"worker_id"`
	WorkerSID  string `json:"worker_sid"`
	Name       string `json:"name"`
}

// requestJoin posts the join request to the hub.
func requestJoin(ctx context.Context, hub string, req joinRequest) (joinResponse, error) {
	var res joinResponse
	body, err := json.Marshal(req)
	if err != nil {
		return res, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, hub+"/api/machines/join", bytes.NewReader(body))
	if err != nil {
		return res, err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		return res, fmt.Errorf("reach the hub: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return res, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return res, fmt.Errorf("join refused (HTTP %d): %s", resp.StatusCode, msg)
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return res, fmt.Errorf("unexpected reply from the hub: %w", err)
	}
	if res.Credential == "" {
		return res, errors.New("the hub sent no credential")
	}
	return res, nil
}
