package main

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/BryanSant/konsert/internal/auth"
	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/controller"
	"github.com/BryanSant/konsert/internal/logx"
	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	rootCmd := &cobra.Command{
		// Runtime errors are printed once, by main, without the usage text.
		SilenceUsage:  true,
		SilenceErrors: true,
		Use:           "konsert",
		Short:         "Konsert distributed agent control plane",
		Version:       version,
	}

	rootCmd.AddCommand(
		newServeCmd(),
		newMCPCmd(),
		newTokenCmd(),
		newStatusCmd(),
		newDispatchCmd(),
		newTasksCmd(),
		newSteerCmd(),
		newCancelCmd(),
		newLogsCmd(),
	)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", "konsert", err)
		os.Exit(1)
	}
}

func newServeCmd() *cobra.Command {
	var (
		configPath            string
		listenAddr            string
		dbPath                string
		adminToken            string
		mcpToken              string
		tokensFile            string
		publicURL             string
		allowedOrigins        []string
		noAuth                bool
		trustTailscaleHeaders bool
		enableMCPHTTP         bool
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the Konsert central controller hub, embedded PWA, REST/WS API, and MCP endpoint",
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
			if cmd.Flags().Changed("admin-token") {
				cfg.AdminToken = adminToken
			}
			if cmd.Flags().Changed("mcp-token") {
				cfg.MCPToken = mcpToken
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
			if err := prepareServeConfig(&cfg, os.Stderr, logger); err != nil {
				return err
			}

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return runServe(ctx, cfg, logger, nil)
		},
	}

	cmd.Flags().StringVarP(&configPath, "config", "c", "", "Path to controller YAML config")
	cmd.Flags().StringVar(&listenAddr, "listen", "", "HTTP/WS listen address (default :8420, or 127.0.0.1:8420 with --no-auth)")
	cmd.Flags().StringVar(&dbPath, "db", "konsert.db", "SQLite database path")
	cmd.Flags().StringVar(&adminToken, "admin-token", "", "Admin passphrase/token for PWA and REST API (auto-generated if unset)")
	cmd.Flags().StringVar(&mcpToken, "mcp-token", "", "Bearer token for /mcp HTTP endpoint (defaults to admin-token)")
	cmd.Flags().StringVar(&tokensFile, "tokens-file", "", "File that persists auto-generated tokens (default konsert-tokens.yaml next to --db)")
	cmd.Flags().BoolVar(&noAuth, "no-auth", false, "Disable authentication (isolated local development only; binds to loopback by default)")
	cmd.Flags().BoolVar(&trustTailscaleHeaders, "trust-tailscale-headers", false, "Trust Tailscale-User-Login headers from Tailscale Serve (honored only from loopback peers)")
	cmd.Flags().BoolVar(&enableMCPHTTP, "enable-mcp-http", true, "Enable Streamable HTTP/SSE MCP endpoint at /mcp")
	cmd.Flags().StringVar(&publicURL, "public-url", "", "Externally reachable base URL of this controller (e.g. https://konsert.example.ts.net); used for SSH-bootstrapped workers and as an allowed browser origin")
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
		Short: "Run the Konsert MCP server over stdio bridged to a konsert controller",
		RunE: func(_ *cobra.Command, _ []string) error {
			if token == "" {
				token = os.Getenv("KONSERT_ADMIN_TOKEN")
			}
			if controllerURL == "" {
				if v := os.Getenv("KONSERT_URL"); v != "" {
					controllerURL = v
				} else {
					controllerURL = "http://127.0.0.1:8420"
				}
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			client := controller.NewRemoteFleetClient(controllerURL, token)
			client.Source = "mcp"
			return controller.RunMCPStdio(ctx, client, version)
		},
	}
	cmd.Flags().StringVar(&controllerURL, "url", "", "Konsert controller HTTP URL (default http://127.0.0.1:8420)")
	cmd.Flags().StringVar(&token, "token", "", "Bearer token for authenticating with konsert")
	return cmd
}

func newTokenCmd() *cobra.Command {
	tokenCmd := &cobra.Command{
		Use:   "token",
		Short: "Manage authentication tokens",
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

func remoteClientFromFlags(url, token string) *controller.RemoteFleetClient {
	if url == "" {
		if v := os.Getenv("KONSERT_URL"); v != "" {
			url = v
		} else {
			url = "http://127.0.0.1:8420"
		}
	}
	if token == "" {
		token = os.Getenv("KONSERT_ADMIN_TOKEN")
	}
	client := controller.NewRemoteFleetClient(url, token)
	client.Source = "cli"
	return client
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
				fmt.Fprintf(out, "\nwarning: %d worker(s) run a konsertd version different from the controller (%s)\n",
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
	cmd.Flags().StringVar(&url, "url", "", "Konsert controller HTTP URL")
	cmd.Flags().StringVar(&token, "token", "", "Admin bearer token")
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
	cmd.Flags().StringVar(&url, "url", "", "Konsert controller HTTP URL")
	cmd.Flags().StringVar(&token, "token", "", "Admin bearer token")
	cmd.Flags().StringVar(&statusFilter, "status", "", "Filter by status")
	cmd.Flags().StringVar(&workerID, "worker", "", "Filter by worker (id or m- sid)")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum tasks to display")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output raw JSON (ids and sids)")
	return cmd
}
