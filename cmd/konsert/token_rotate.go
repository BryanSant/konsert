package main

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/BryanSant/konsert/internal/controller"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/spf13/cobra"
)

// newTokenRotateCmd is `konsert token rotate`, a group with one subcommand per kind of
// credential (KON-W1YRP): `recovery` (the hub's recovery key) and `machines`.
func newTokenRotateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rotate",
		Short: "Rotate credentials",
	}
	cmd.AddCommand(newRotateRecoveryCmd(), newRotateMachinesCmd())
	return cmd
}

func newRotateMachinesCmd() *cobra.Command {
	var url, token, machine string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "machines",
		Short: "Revoke the credentials of machines that joined by hand",
		Long: `Retire the credentials of machines that joined with a code: each is revoked and
disconnected right away, and listed. Join each again with a new code from Add
machine → Manual (` + "`konsertd join <hub-url> <code>`" + `). Machines the hub installs
itself (its own machine, SSH machines) are not affected.`,
		Example: "  konsert token rotate machines\n  konsert token rotate machines --machine m-7qk3m",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client := remoteClientFromFlags(url, token)
			client.Source = "cli"
			res, err := client.RotateMachineCredentials(cmd.Context(), machine)
			if err != nil {
				return friendlyAPIError(err)
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return json.MarshalWrite(out, res, protocol.JSONOptions, json.Deterministic(true))
			}
			writeRotation(out, res)
			if res.Failed > 0 {
				return fmt.Errorf("%d machine(s) could not be rotated", res.Failed)
			}
			return nil
		},
	}
	addClientFlags(cmd, &url, &token)
	cmd.Flags().StringVar(&machine, "machine", "", "Rotate only this machine (m- sid or id)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print the result as JSON")
	return cmd
}

func writeRotation(w io.Writer, res controller.RotationResponse) {
	if len(res.Results) == 0 {
		fmt.Fprintln(w, "No joined machines to rotate.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "MACHINE\tSID\tRESULT")
	for _, r := range res.Results {
		result := "revoked: needs a new join code"
		if r.Action == "failed" {
			result = "failed: " + r.Error
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Name, r.WorkerSID, result)
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "\n%d need a join code, %d failed.\n", res.NeedsJoin, res.Failed)
	if res.NeedsJoin > 0 {
		fmt.Fprintln(w, "Join those again: Add machine → Manual, then konsertd join on each.")
	}
}
