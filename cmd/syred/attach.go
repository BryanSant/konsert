package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/BryanSant/syre/internal/hubkey"
	"github.com/BryanSant/syre/internal/worker"
	"github.com/spf13/cobra"
)

// newAttachCmd is the hub key's forced command (SYR-K1B60): it bridges the SSH
// session's stdin/stdout to the local socket of the syred the hub asked for.
func newAttachCmd() *cobra.Command {
	var allow []string
	cmd := &cobra.Command{
		Use:    "attach [worker-id]",
		Short:  "Bridge stdin/stdout to a running syred's local socket (the hub's SSH transport)",
		Hidden: true,
		Args:   cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// sshd hands the command the hub sent ("syred attach <id>") to the forced
			// command in SSH_ORIGINAL_COMMAND.
			requested := os.Getenv("SSH_ORIGINAL_COMMAND")
			if len(args) == 1 {
				requested = args[0]
			}
			id, err := worker.AttachRequest(requested, allow)
			if err != nil {
				return err
			}
			path := worker.LocalSocketPath("", id)
			if path == "" {
				return errors.New("no state directory for the local socket")
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return worker.Attach(ctx, path, os.Stdin, os.Stdout)
		},
	}
	cmd.Flags().StringSliceVar(&allow, "allow", nil, "Worker ids this key may attach to (set in authorized_keys)")
	return cmd
}

// newHubKeyCmd manages the hub key's line in this user's authorized_keys. The hub's
// bootstrap runs it over the user's own SSH access.
func newHubKeyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "hub-key",
		Short:  "Allow or remove a hub's SSH key in authorized_keys (used by the hub's SSH bootstrap)",
		Hidden: true,
	}
	var hubID, workerID string
	add := &cobra.Command{
		Use:   "add",
		Short: "Allow the hub public key on stdin to attach to --worker (forced command, restricted)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pub, err := readLine(bufio.NewReader(cmd.InOrStdin()))
			if err != nil {
				return fmt.Errorf("read the hub public key from stdin: %w", err)
			}
			exe, err := installExecutable()
			if err != nil {
				return err
			}
			path, err := hubkey.Install(hubkey.Entry{HubID: hubID, PublicKey: pub, Exe: exe, GOOS: runtime.GOOS, WorkerID: workerID})
			if err != nil {
				return err
			}
			// The machine is being added (again): forget an earlier removal.
			worker.ClearRemovedMarker("", workerID)
			fmt.Fprintln(cmd.OutOrStdout(), "hub key allowed in "+path)
			return nil
		},
	}
	remove := &cobra.Command{
		Use:   "remove",
		Short: "Remove --worker from the hub's line (the whole line without --worker)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !hubkey.ValidHubID(hubID) {
				return fmt.Errorf("invalid --hub %q", hubID)
			}
			path, changed, err := hubkey.Uninstall(hubID, workerID)
			if err != nil {
				return err
			}
			if changed {
				fmt.Fprintln(cmd.OutOrStdout(), "hub key removed from "+path)
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "hub key not present in "+path)
			}
			return nil
		},
	}
	for _, c := range []*cobra.Command{add, remove} {
		c.Flags().StringVar(&hubID, "hub", "", "The hub's id")
		c.Flags().StringVar(&workerID, "worker", "", "The syred identity (worker id)")
		_ = c.MarkFlagRequired("hub")
	}
	_ = add.MarkFlagRequired("worker")
	cmd.AddCommand(add, remove)
	return cmd
}
