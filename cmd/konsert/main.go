package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/BryanSant/konsert/internal/auth"
	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/controller"
	"github.com/BryanSant/konsert/internal/store/sqlite"
	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	rootCmd := &cobra.Command{
		Use:     "konsert",
		Short:   "Konsert distributed agent control plane",
		Version: version,
	}

	rootCmd.AddCommand(
		newServeCmd(),
		newMCPCmd(),
		newTokenCmd(),
		newStatusCmd(),
		newDispatchCmd(),
		newTasksCmd(),
	)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func newServeCmd() *cobra.Command {
	var (
		configPath            string
		listenAddr            string
		dbPath                string
		fleetToken            string
		adminToken            string
		mcpToken              string
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
			if cmd.Flags().Changed("fleet-token") {
				cfg.FleetToken = fleetToken
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

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			st, err := sqlite.Open(ctx, cfg.DBPath)
			if err != nil {
				return err
			}
			defer st.Close()

			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
			srv := controller.NewServer(cfg, st, logger, version)
			go srv.Hub().RunReaper(ctx)

			httpSrv := &http.Server{
				Addr:              cfg.ListenAddr,
				Handler:           srv,
				ReadHeaderTimeout: 10 * time.Second,
			}

			go func() {
				<-ctx.Done()
				srv.Hub().Close()
				shCtx, shCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer shCancel()
				_ = httpSrv.Shutdown(shCtx)
			}()

			logger.Info("konsert controller listening",
				"addr", cfg.ListenAddr,
				"db", cfg.DBPath,
				"mcp_http", cfg.EnableMCPHTTP,
				"auth_enabled", !cfg.NoAuth && cfg.AdminToken != "",
			)
			if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				return err
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&configPath, "config", "c", "", "Path to controller YAML config")
	cmd.Flags().StringVar(&listenAddr, "listen", ":8420", "HTTP/WS listen address")
	cmd.Flags().StringVar(&dbPath, "db", "konsert.db", "SQLite database path")
	cmd.Flags().StringVar(&fleetToken, "fleet-token", "", "Pre-shared bearer token for konsertd workers")
	cmd.Flags().StringVar(&adminToken, "admin-token", "", "Admin passphrase/token for PWA and REST API")
	cmd.Flags().StringVar(&mcpToken, "mcp-token", "", "Bearer token for /mcp HTTP endpoint (defaults to admin-token)")
	cmd.Flags().BoolVar(&noAuth, "no-auth", false, "Disable authentication (for isolated local development)")
	cmd.Flags().BoolVar(&trustTailscaleHeaders, "trust-tailscale-headers", false, "Trust Tailscale-User-Login headers from Tailscale Serve")
	cmd.Flags().BoolVar(&enableMCPHTTP, "enable-mcp-http", true, "Enable Streamable HTTP/SSE MCP endpoint at /mcp")

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
		Short: "Generate a cryptographically random hex token for fleet or UI auth",
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
	tokenCmd.AddCommand(genCmd)
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
	return controller.NewRemoteFleetClient(url, token)
}

func newStatusCmd() *cobra.Command {
	var url, token string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Display registered workers and fleet status",
		RunE: func(_ *cobra.Command, _ []string) error {
			client := remoteClientFromFlags(url, token)
			res, err := client.FleetStatus(context.Background())
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME\tHOST_GROUP\tOS/ARCH\tISOLATION\tPERMISSION\tAGENTS\tACTIVE\tSTATUS")
			for _, wk := range res.Workers {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s/%s\t%s\t%s\t%v\t%d/%d\t%s\n",
					wk.ID, wk.Name, wk.HostGroup, wk.OS, wk.Arch, wk.Isolation, wk.PermissionMode,
					wk.Agents, wk.ActiveTasks, wk.MaxConcurrent, wk.Status)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "Konsert controller HTTP URL")
	cmd.Flags().StringVar(&token, "token", "", "Admin bearer token")
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
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}
			final, err := client.GetTaskResult(context.Background(), res.TaskID, true, 300)
			if err != nil {
				return err
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(final)
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "Konsert controller HTTP URL")
	cmd.Flags().StringVar(&token, "token", "", "Admin bearer token")
	cmd.Flags().StringVar(&agent, "agent", "claude", "Agent driver (claude, agy, grok, mock)")
	cmd.Flags().StringVar(&repoPath, "repo", ".", "Repository / working directory path on worker")
	cmd.Flags().StringVar(&branch, "branch", "", "Optional git branch (creates isolated worktree)")
	cmd.Flags().StringVar(&prompt, "prompt", "", "Task prompt")
	cmd.Flags().StringVar(&workerID, "worker", "", "Target worker ID")
	cmd.Flags().StringVar(&hostGroup, "host-group", "", "Target host group")
	cmd.Flags().StringVar(&mode, "mode", "oneshot", "Execution mode (oneshot|interactive)")
	cmd.Flags().BoolVar(&wait, "wait", false, "Wait for task completion and print structured result")
	_ = cmd.MarkFlagRequired("prompt")
	return cmd
}

func newTasksCmd() *cobra.Command {
	var url, token, statusFilter, workerID string
	var limit int
	cmd := &cobra.Command{
		Use:   "tasks",
		Short: "List recent and active fleet tasks",
		RunE: func(_ *cobra.Command, _ []string) error {
			client := remoteClientFromFlags(url, token)
			tasks, err := client.ListTasks(context.Background(), statusFilter, workerID, limit)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "TASK_ID\tWORKER\tAGENT\tSTATUS\tBRANCH\tELAPSED\tLAST_MILESTONE")
			for _, t := range tasks {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%ds\t%s\n",
					t.TaskID, t.WorkerID, t.Agent, t.Status, t.Branch, t.ElapsedSeconds, t.LastMilestone)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "Konsert controller HTTP URL")
	cmd.Flags().StringVar(&token, "token", "", "Admin bearer token")
	cmd.Flags().StringVar(&statusFilter, "status", "", "Filter by status")
	cmd.Flags().StringVar(&workerID, "worker", "", "Filter by worker ID")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum tasks to display")
	return cmd
}
