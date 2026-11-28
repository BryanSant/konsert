package main

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/BryanSant/syre/internal/auth"
	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/controller"
	"github.com/BryanSant/syre/internal/logx"
	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	rootCmd := &cobra.Command{
		// Runtime errors are printed once, by main, without the usage text.
		SilenceUsage:  true,
		SilenceErrors: true,
		Use:           "syre",
		Short:         "Syre distributed agent control plane",
		Version:       version,
	}

	rootCmd.AddCommand(
		newServeCmd(),
		newPairCmd(),
		newLoginCmd(),
		newLogoutCmd(),
		newMCPCmd(),
		newTokenCmd(),
		newStatusCmd(),
		newDispatchCmd(),
		newTasksCmd(),
		newSteerCmd(),
		newCancelCmd(),
		newLogsCmd(),
		newPolicyCmd(),
	)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", "syre", err)
		os.Exit(1)
	}
}

func newServeCmd() *cobra.Command {
	var (
		configPath            string
		listenAddr            string
		dbPath                string
		recoveryKey           string
		tokensFile            string
		publicURL             string
		allowedOrigins        []string
		noAuth                bool
		trustTailscaleHeaders bool
		enableMCPHTTP         bool
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the Syre central controller hub, embedded PWA, REST/WS API, and MCP endpoint",
		Long: "Start the hub. Unless --no-auth is set it prints a one-time sign-in link (and a QR\n" +
			"code in a terminal): open it in a browser or scan it with a phone to sign in. Run\n" +
			"`syre pair` for another link at any time; signed-in browsers can add devices\n" +
			"from Settings → Devices.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadControllerConfig(configPath)
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("listen") {
				cfg.ListenAddr = listenAddr
			}
			if cmd.Flags().Changed("db") {
				cfg.DBPath = dbPath
			}
			if cmd.Flags().Changed("recovery-key") {
				cfg.RecoveryKey = recoveryKey
			}
			if cmd.Flags().Changed("no-auth") {
				cfg.NoAuth = noAuth
			}
			if cmd.Flags().Changed("trust-tailscale-headers") {
				cfg.TrustTailscaleHeaders = trustTailscaleHeaders
			}
			if cmd.Flags().Changed("enable-mcp-http") {
				cfg.EnableMCPHTTP = enableMCPHTTP
			}
			if cmd.Flags().Changed("tokens-file") {
				cfg.TokensFile = tokensFile
			}
			if cmd.Flags().Changed("public-url") {
				cfg.PublicURL = publicURL
			}
			if cmd.Flags().Changed("allowed-origin") {
				cfg.AllowedOrigins = allowedOrigins
			}

			logger := logx.New(os.Stderr, slog.LevelInfo)
			if err := prepareServeConfig(&cfg, logger); err != nil {
				return err
			}

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return runServe(ctx, cfg, logger, func(addr net.Addr) {
				// Let the syre CLI on this machine find the hub and its tokens file.
				if cfg.NoAuth {
					return
				}
				if err := recordLocalHub(addr.String(), resolveTokensFile(cfg)); err != nil {
					logger.Debug("could not record the local hub for the CLI", "err", err)
				}
			})
		},
	}

	cmd.Flags().StringVarP(&configPath, "config", "c", "", "Path to controller YAML config")
	cmd.Flags().StringVar(&listenAddr, "listen", "", "HTTP/WS listen address (default :8420, or 127.0.0.1:8420 with --no-auth)")
	cmd.Flags().StringVar(&dbPath, "db", "syre.db", "SQLite database path")
	cmd.Flags().StringVar(&recoveryKey, "recovery-key", "", "Emergency recovery key (default: generated into the tokens file, never printed)")
	_ = cmd.Flags().MarkHidden("recovery-key")
	cmd.Flags().StringVar(&tokensFile, "tokens-file", "", "File that keeps the recovery key, mode 0600 (default syre-tokens.yaml next to --db)")
	cmd.Flags().BoolVar(&noAuth, "no-auth", false, "Disable authentication (isolated local development only; binds to loopback by default)")
	cmd.Flags().BoolVar(&trustTailscaleHeaders, "trust-tailscale-headers", false, "Trust Tailscale-User-Login headers from Tailscale Serve (honored only from loopback peers)")
	cmd.Flags().BoolVar(&enableMCPHTTP, "enable-mcp-http", true, "Enable Streamable HTTP/SSE MCP endpoint at /mcp")
	cmd.Flags().StringVar(&publicURL, "public-url", "", "Externally reachable base URL of this controller (e.g. https://syre.example.ts.net); used in join links and as an allowed browser origin")
	cmd.Flags().StringSliceVar(&allowedOrigins, "allowed-origin", nil, "Extra browser origin allowed to use the REST API and /ws/client (repeatable; e.g. http://localhost:4200)")

	return cmd
}

func newMCPCmd() *cobra.Command {
	var (
		controllerURL string
		token         string
	)
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run the Syre MCP server over stdio bridged to a syre controller",
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			client := newHubClient(resolveHub(controllerURL, token, ""), "mcp")
			return controller.RunMCPStdio(ctx, client, version)
		},
	}
	addClientFlags(cmd, &controllerURL, &token)
	return cmd
}

func newTokenCmd() *cobra.Command {
	tokenCmd := &cobra.Command{
		Use:   "token",
		Short: "Rotate the recovery key or generate random tokens",
	}
	var bytesLen int
	genCmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate a cryptographically random hex token",
		RunE: func(_ *cobra.Command, _ []string) error {
			tok, err := auth.GenerateToken(bytesLen)
			if err != nil {
				return err
			}
			fmt.Println(tok)
			return nil
		},
	}
	genCmd.Flags().IntVar(&bytesLen, "bytes", 32, "Number of random bytes")
	tokenCmd.AddCommand(genCmd, newTokenRotateCmd())
	return tokenCmd
}

// remoteClientFromFlags returns a client for the hub named by --url/--token or found
// automatically (see creds.go).
func remoteClientFromFlags(url, token string) *controller.RemoteFleetClient {
	return newHubClient(resolveHub(url, token, ""), auditSourceCLI)
}

func newStatusCmd() *cobra.Command {
	var url, token string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Display registered workers and fleet status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client := remoteClientFromFlags(url, token)
			res, err := client.FleetStatus(cmd.Context())
			if err != nil {
				return friendlyAPIError(err)
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return writeJSONOut(out, res)
			}
			ctrlVersion := strings.TrimSpace(res.ControllerVersion)
			if ctrlVersion == "" {
				ctrlVersion = "unknown"
			}
			fmt.Fprintf(out, "Controller version: %s\n\n", ctrlVersion)
			mismatches := 0
			w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME\tHOST_GROUP\tOS/ARCH\tISOLATION\tPERMISSION\tAGENTS\tACTIVE\tSTATUS\tVERSION")
			for _, wk := range res.Workers {
				if controller.VersionMismatch(res.ControllerVersion, wk.Version) {
					mismatches++
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s/%s\t%s\t%s\t%v\t%d/%d\t%s\t%s\n",
					cmp.Or(wk.SID, wk.ID), wk.Name, wk.HostGroup, wk.OS, wk.Arch, wk.Isolation, wk.PermissionMode,
					wk.Agents, wk.ActiveTasks, wk.MaxConcurrent, wk.Status,
					workerVersionLabel(res.ControllerVersion, wk.Version))
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if mismatches > 0 {
				fmt.Fprintf(out, "\nwarning: %d worker(s) run a syred version different from the controller (%s)\n",
					mismatches, ctrlVersion)
			}
			return nil
		},
	}
	addClientFlags(cmd, &url, &token)
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output raw JSON")
	return cmd
}

func newDispatchCmd() *cobra.Command {
	var (
		url       string
		token     string
		agent     string
		repoPath  string
		branch    string
		prompt    string
		workerID  string
		hostGroup string
		mode      string
		wait      bool
	)
	cmd := &cobra.Command{
		Use:   "dispatch",
		Short: "Dispatch a coding agent task to the fleet",
		RunE: func(_ *cobra.Command, _ []string) error {
			client := remoteClientFromFlags(url, token)
			res, err := client.DispatchTask(context.Background(), controller.DispatchRequest{
				WorkerSelector: controller.WorkerSelector{
					WorkerID:  workerID,
					HostGroup: hostGroup,
				},
				Agent:    agent,
				RepoPath: repoPath,
				Branch:   branch,
				Prompt:   prompt,
				Mode:     mode,
			})
			if err != nil {
				return err
			}
			if !wait {
				return writeJSONOut(os.Stdout, res)
			}
			final, err := client.GetTaskResult(context.Background(), res.TaskID, true, 300)
			if err != nil {
				return err
			}
			return writeJSONOut(os.Stdout, final)
		},
	}
	addClientFlags(cmd, &url, &token)
	cmd.Flags().StringVar(&agent, "agent", "claude", "Agent driver (claude, agy, grok, mock)")
	cmd.Flags().StringVar(&repoPath, "repo", ".", "Repository / working directory path on worker")
	cmd.Flags().StringVar(&branch, "branch", "", "Optional git branch (creates isolated worktree)")
	cmd.Flags().StringVar(&prompt, "prompt", "", "Task prompt")
	cmd.Flags().StringVar(&workerID, "worker", "", "Target worker (id or m- sid)")
	cmd.Flags().StringVar(&hostGroup, "host-group", "", "Target host group")
	cmd.Flags().StringVar(&mode, "mode", "oneshot", "Execution mode (oneshot|interactive)")
	cmd.Flags().BoolVar(&wait, "wait", false, "Wait for task completion and print structured result")
	_ = cmd.MarkFlagRequired("prompt")
	return cmd
}

func newTasksCmd() *cobra.Command {
	var url, token, statusFilter, workerID string
	var limit int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "tasks",
		Short: "List recent and active fleet tasks",
		RunE: func(_ *cobra.Command, _ []string) error {
			client := remoteClientFromFlags(url, token)
			tasks, err := client.ListTasks(context.Background(), statusFilter, workerID, limit)
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSONOut(os.Stdout, tasks)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "TASK_ID\tWORKER\tAGENT\tSTATUS\tBRANCH\tELAPSED\tLAST_MILESTONE")
			for _, t := range tasks {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%ds\t%s\n",
					cmp.Or(t.TaskSID, t.TaskID), cmp.Or(t.WorkerSID, t.WorkerID), t.Agent, t.Status, t.Branch, t.ElapsedSeconds, t.LastMilestone)
			}
			return w.Flush()
		},
	}
	addClientFlags(cmd, &url, &token)
	cmd.Flags().StringVar(&statusFilter, "status", "", "Filter by status")
	cmd.Flags().StringVar(&workerID, "worker", "", "Filter by worker (id or m- sid)")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum tasks to display")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output raw JSON (ids and sids)")
	return cmd
}
