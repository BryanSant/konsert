package main

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/BryanSant/syre/internal/controller"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/store"
	"github.com/spf13/cobra"
)

// addClientFlags registers the --url/--token flags shared by commands that talk to a
// running hub. Both are normally unnecessary: see creds.go for how the hub and a
// credential are found.
func addClientFlags(cmd *cobra.Command, url, token *string) {
	cmd.Flags().StringVar(url, "url", "", "Hub URL (default $SYRE_URL, the hub you logged in to, or this machine's hub)")
	cmd.Flags().StringVar(token, "token", "", "Device credential (default $SYRE_TOKEN or the one saved by syre login)")
}

// friendlyAPIError turns common controller rejections into actionable messages.
func friendlyAPIError(err error) error {
	var apiErr *controller.APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	var hint string
	switch apiErr.StatusCode {
	case 401:
		hint = "not signed in: run syre login <hub-url> (on the hub's machine, make sure you run as the hub's user)"
	case 403:
		hint = "request rejected by the hub (forbidden): check the credential and that --url points at the hub itself"
	case 404:
		if apiErr.Message != "" {
			return fmt.Errorf("%s (HTTP 404)", apiErr.Message)
		}
		hint = "not found"
	case 413:
		hint = "request too large for the controller; send less text"
	case 415:
		hint = "controller rejected the request content type (this syre CLI may be older than the controller)"
	case 429:
		hint = "rate limited by the controller; wait a moment and retry"
	default:
		return err
	}
	if apiErr.Message != "" {
		return fmt.Errorf("%s (HTTP %d: %s)", hint, apiErr.StatusCode, apiErr.Message)
	}
	return fmt.Errorf("%s (HTTP %d)", hint, apiErr.StatusCode)
}

func newSteerCmd() *cobra.Command {
	var url, token string
	var noNewline, asJSON bool
	cmd := &cobra.Command{
		Use:   "steer <task-id> <text...>",
		Short: "Send input to a running interactive task",
		Long: "Write text to a running task's terminal, followed by a newline unless --no-newline is set.\n" +
			"Pass \"-\" as the text to read it from stdin.",
		Example: "  syre steer t-7qk3m \"also add tests\"\n" +
			"  syre steer --no-newline t-7qk3m y\n" +
			"  git diff | syre steer t-7qk3m -",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			taskID := args[0]
			text := strings.Join(args[1:], " ")
			if len(args) == 2 && args[1] == "-" {
				b, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return fmt.Errorf("read stdin: %w", err)
				}
				text = string(b)
			}
			client := remoteClientFromFlags(url, token)
			res, err := client.SteerTask(cmd.Context(), taskID, text, !noNewline)
			if err != nil {
				return friendlyAPIError(err)
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSONOut(out, res)
			}
			fmt.Fprintf(out, "sent input to task %s (status: %s)\n", cmp.Or(res.TaskSID, res.TaskID), res.Status)
			return nil
		},
	}
	addClientFlags(cmd, &url, &token)
	cmd.Flags().BoolVarP(&noNewline, "no-newline", "n", false, "Do not append a newline (Enter) after the text")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output raw JSON")
	return cmd
}

func newCancelCmd() *cobra.Command {
	var url, token string
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "cancel <task-id>",
		Short:   "Cancel a queued or running task",
		Example: "  syre cancel t-7qk3m",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := remoteClientFromFlags(url, token)
			res, err := client.CancelTask(cmd.Context(), args[0])
			if err != nil {
				return friendlyAPIError(err)
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSONOut(out, res)
			}
			fmt.Fprintf(out, "cancel requested for task %s (status: %s)\n", cmp.Or(res.TaskSID, res.TaskID), res.Status)
			return nil
		},
	}
	addClientFlags(cmd, &url, &token)
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output raw JSON")
	return cmd
}

func newLogsCmd() *cobra.Command {
	var url, token string
	var follow, asJSON bool
	var afterID int64
	var interval time.Duration
	cmd := &cobra.Command{
		Use:   "logs <task-id>",
		Short: "Print a task's structured telemetry events",
		Long: "Print a task's structured telemetry events (milestones, tool calls, file edits, errors, exit).\n" +
			"With --follow, keep polling for new events until the task finishes or you press Ctrl-C.",
		Example: "  syre logs t-7qk3m\n" +
			"  syre logs -f t-7qk3m\n" +
			"  syre logs --json t-7qk3m | jq .message",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if interval <= 0 {
				return errors.New("--interval must be positive")
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			client := remoteClientFromFlags(url, token)
			err := streamTaskLogs(ctx, client, cmd.OutOrStdout(), args[0], afterID, follow, interval, asJSON)
			if err != nil && ctx.Err() != nil {
				return nil // interrupted while following
			}
			return friendlyAPIError(err)
		},
	}
	addClientFlags(cmd, &url, &token)
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Keep polling for new events until the task finishes")
	cmd.Flags().Int64Var(&afterID, "after-id", 0, "Only show events with an ID greater than this")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "Polling interval for --follow")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output one JSON object per event (NDJSON)")
	return cmd
}

// logsClient is the slice of RemoteFleetClient that `syre logs` needs.
type logsClient interface {
	ListTaskEvents(ctx context.Context, taskID string, afterID int64) ([]store.TaskEvent, error)
	GetTaskResult(ctx context.Context, taskID string, wait bool, timeoutSeconds int) (controller.TaskResult, error)
}

// streamTaskLogs prints events after afterID; with follow it polls (using after_id)
// until the task reaches a terminal status, then drains any remaining events.
func streamTaskLogs(ctx context.Context, c logsClient, out io.Writer, taskID string, afterID int64, follow bool, interval time.Duration, asJSON bool) error {
	// Fail fast with "not found" for a mistyped ID: the events endpoint returns an
	// empty list for unknown tasks.
	res, err := c.GetTaskResult(ctx, taskID, false, 0)
	if err != nil {
		return err
	}
	enc := jsontext.NewEncoder(out, cliJSONOpts)
	for {
		terminal := protocol.IsTerminalTaskStatus(res.Status)
		events, err := c.ListTaskEvents(ctx, taskID, afterID)
		if err != nil {
			return err
		}
		for _, ev := range events {
			if asJSON {
				if err := json.MarshalEncode(enc, ev); err != nil {
					return err
				}
			} else {
				fmt.Fprintln(out, formatEvent(ev))
			}
			if ev.ID > afterID {
				afterID = ev.ID
			}
		}
		if len(events) > 0 {
			continue // page through a backlog before sleeping
		}
		if !follow || terminal {
			if follow && !asJSON {
				fmt.Fprintf(out, "task %s %s\n", taskID, res.Status)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		if res, err = c.GetTaskResult(ctx, taskID, false, 0); err != nil {
			return err
		}
	}
}

func formatEvent(ev store.TaskEvent) string {
	ts := ev.CreatedAt.Local().Format("2006-01-02 15:04:05")
	msg := strings.TrimRight(ev.Message, "\r\n")
	return fmt.Sprintf("%s  %-10s  %s", ts, ev.Kind, msg)
}

// cliJSONOpts makes CLI JSON output stable (sorted map keys) and tolerant of
// invalid UTF-8 in remote data.
var cliJSONOpts = json.JoinOptions(json.Deterministic(true), jsontext.AllowInvalidUTF8(true))

// writeJSONOut writes v as indented JSON followed by a newline.
func writeJSONOut(w io.Writer, v any) error {
	return json.MarshalEncode(jsontext.NewEncoder(w, cliJSONOpts, jsontext.WithIndent("  ")), v)
}

// workerVersionLabel renders a worker's version for `syre status`, marking a
// mismatch against the controller's version ("dev"/empty count as unknown).
func workerVersionLabel(controllerVersion, workerVersion string) string {
	v := strings.TrimSpace(workerVersion)
	if controller.NormalizeVersion(v) == "" {
		if v == "" {
			return "unknown"
		}
		return v
	}
	if controller.VersionMismatch(controllerVersion, v) {
		return v + " (mismatch)"
	}
	return v
}
