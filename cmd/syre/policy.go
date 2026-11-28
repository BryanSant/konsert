package main

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/BryanSant/syre/internal/controller"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/spf13/cobra"
)

func newPolicyCmd() *cobra.Command {
	var url, token string
	var sets []string
	var clear, asJSON bool
	cmd := &cobra.Command{
		Use:   "policy <machine>",
		Short: "Show or change a machine's policy for new work",
		Long: "Show a machine's policy, or change it with --set (repeatable) or --clear.\n" +
			"The hub admits no work that breaks it, and the machine refuses such work too.\n" +
			"Every change is audited. Agents read the policy but cannot change it.\n\n" +
			"Keys:\n" +
			"  role             dedicated | interactive | control_only\n" +
			"  max_sessions     managed sessions at once (0: no cap)\n" +
			"  max_load         refuse new work above this 1-minute load average\n" +
			"  reserve_cpu      cores kept for the user\n" +
			"  min_free_ram_mb  memory kept free, on top of the guardrails' survival reserve\n" +
			"  reserve_ram_mb   memory reserved for the user, on top of that too\n" +
			"  allowed_work     comma list of chat, task, ticket (empty: all)\n" +
			"  quiet_hours      ;-separated windows HH:MM-HH:MM[@mon,tue,...] (empty: none)\n" +
			"  time_zone        IANA zone for quiet hours, e.g. America/Denver",
		Example: "  syre policy bocabox\n" +
			"  syre policy daybreak --set role=interactive --set reserve_ram_mb=8192 --set reserve_cpu=4\n" +
			"  syre policy bocamacmini --set quiet_hours='22:00-07:00@mon,tue,wed,thu,fri' --set time_zone=America/Denver\n" +
			"  syre policy bocabox --clear",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := remoteClientFromFlags(url, token)
			fleet, err := client.FleetStatus(cmd.Context())
			if err != nil {
				return friendlyAPIError(err)
			}
			wk, ok := findMachine(fleet.Workers, args[0])
			if !ok {
				return fmt.Errorf("no machine %q", args[0])
			}
			p := protocol.MachinePolicy{}
			if wk.Policy != nil {
				p = *wk.Policy
			}
			if clear || len(sets) > 0 {
				if clear {
					p = protocol.MachinePolicy{}
				}
				for _, kv := range sets {
					if err := setPolicyKey(&p, kv); err != nil {
						return err
					}
				}
				if p, err = client.SetWorkerPolicy(cmd.Context(), wk.ID, p); err != nil {
					return friendlyAPIError(err)
				}
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSONOut(out, p)
			}
			printPolicy(out, cmp.Or(wk.Name, wk.ID), p)
			return nil
		},
	}
	addClientFlags(cmd, &url, &token)
	cmd.Flags().StringArrayVar(&sets, "set", nil, "Set key=value (repeatable; an empty value clears the key)")
	cmd.Flags().BoolVar(&clear, "clear", false, "Remove every rule (applied before --set)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output raw JSON")
	return cmd
}

func findMachine(list []controller.WorkerStatusView, ref string) (controller.WorkerStatusView, bool) {
	for _, w := range list {
		if w.ID == ref || w.SID == ref || strings.EqualFold(w.Name, ref) {
			return w, true
		}
	}
	return controller.WorkerStatusView{}, false
}

// setPolicyKey applies one key=value to p.
func setPolicyKey(p *protocol.MachinePolicy, kv string) error {
	key, val, ok := strings.Cut(kv, "=")
	if !ok {
		return fmt.Errorf("--set %q: want key=value", kv)
	}
	key, val = strings.TrimSpace(key), strings.TrimSpace(val)
	num := func() (float64, error) {
		if val == "" {
			return 0, nil
		}
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return 0, fmt.Errorf("--set %s: %q is not a number", key, val)
		}
		return f, nil
	}
	var err error
	var f float64
	switch key {
	case "role":
		p.Role = val
	case "max_sessions":
		f, err = num()
		p.MaxSessions = int(f)
	case "max_load":
		p.MaxLoad, err = num()
	case "reserve_cpu":
		p.ReserveCPU, err = num()
	case "min_free_ram_mb":
		f, err = num()
		p.MinFreeRAMMB = int(f)
	case "reserve_ram_mb":
		f, err = num()
		p.ReserveRAMMB = int(f)
	case "allowed_work":
		p.AllowedWork = nil
		for w := range strings.SplitSeq(val, ",") {
			if w = strings.TrimSpace(w); w != "" {
				p.AllowedWork = append(p.AllowedWork, w)
			}
		}
	case "time_zone":
		p.TimeZone = val
	case "quiet_hours":
		p.QuietHours = nil
		for win := range strings.SplitSeq(val, ";") {
			if win = strings.TrimSpace(win); win == "" {
				continue
			}
			span, days, _ := strings.Cut(win, "@")
			start, end, ok := strings.Cut(span, "-")
			if !ok {
				return fmt.Errorf("--set quiet_hours: %q: want HH:MM-HH:MM[@mon,tue,...]", win)
			}
			w := protocol.QuietWindow{Start: strings.TrimSpace(start), End: strings.TrimSpace(end)}
			for d := range strings.SplitSeq(days, ",") {
				if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
					w.Days = append(w.Days, d)
				}
			}
			p.QuietHours = append(p.QuietHours, w)
		}
	default:
		return fmt.Errorf("--set: unknown key %q (see syre policy --help)", key)
	}
	return err
}

func printPolicy(out io.Writer, machine string, p protocol.MachinePolicy) {
	var lines []string
	add := func(k, v string) { lines = append(lines, fmt.Sprintf("  %-16s %s", k, v)) }
	if p.Role != "" {
		add("role", p.Role)
	}
	if p.MaxSessions > 0 {
		add("max_sessions", strconv.Itoa(p.MaxSessions))
	}
	if p.MaxLoad > 0 {
		add("max_load", strconv.FormatFloat(p.MaxLoad, 'g', -1, 64))
	}
	if p.ReserveCPU > 0 {
		add("reserve_cpu", strconv.FormatFloat(p.ReserveCPU, 'g', -1, 64))
	}
	if p.MinFreeRAMMB > 0 {
		add("min_free_ram_mb", strconv.Itoa(p.MinFreeRAMMB))
	}
	if p.ReserveRAMMB > 0 {
		add("reserve_ram_mb", strconv.Itoa(p.ReserveRAMMB))
	}
	if len(p.AllowedWork) > 0 {
		add("allowed_work", strings.Join(p.AllowedWork, ","))
	}
	if len(p.QuietHours) > 0 {
		var ws []string
		for _, w := range p.QuietHours {
			s := w.Start + "-" + w.End
			if len(w.Days) > 0 {
				s += "@" + strings.Join(w.Days, ",")
			}
			ws = append(ws, s)
		}
		add("quiet_hours", strings.Join(ws, "; ")+" ("+p.TimeZone+")")
	}
	if len(lines) == 0 {
		fmt.Fprintf(out, "%s: no policy (any work, guardrails only)\n", machine)
		return
	}
	slices.Sort(lines)
	fmt.Fprintf(out, "%s policy:\n%s\n", machine, strings.Join(lines, "\n"))
}
