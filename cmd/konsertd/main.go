package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/controller"
	"github.com/BryanSant/konsert/internal/logx"
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/worker"
	"github.com/BryanSant/konsert/internal/worker/drivers"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var version = "dev"

func main() {
	rootCmd := &cobra.Command{
		// Runtime errors are printed once, by main, without the usage text.
		SilenceUsage:  true,
		SilenceErrors: true,
		Use:           "konsertd",
		Short:         "Konsert cross-platform worker daemon",
		Version:       version,
	}

	rootCmd.AddCommand(newRunCmd(), newIDCmd())
	rootCmd.AddCommand(newDoctorCmd())
	rootCmd.AddCommand(newInstallHooksCmd())
	rootCmd.AddCommand(newHookEventCmd())
	rootCmd.AddCommand(newDaemonMCPCmd())
	rootCmd.AddCommand(newMCPBridgeCmd())
	rootCmd.AddCommand(newJoinCmd(), newInstallCmd(), newUninstallCmd(), newStatusCmd())

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", "konsertd", err)
		os.Exit(1)
	}
}

// runOptions are the `run` flags. `install` and `id` accept the same set, so the config
// they resolve is exactly the one `run` would use.
type runOptions struct {
	configPath       string
	controllerURL    string
	credentialStdin  bool
	workerID         string
	name             string
	hostGroup        string
	isolation        string
	sandbox          bool
	sandboxShell     bool
	allowedRoots     []string
	tags             []string
	maxConcurrent    int
	enableMockDriver bool
	logFile          string
}

func (o *runOptions) bind(fs *pflag.FlagSet) {
	fs.StringVarP(&o.configPath, "config", "c", "", "Path to worker YAML config file")
	fs.StringVar(&o.controllerURL, "controller", "ws://127.0.0.1:8420/ws/worker", "Controller WebSocket URL")
	fs.BoolVar(&o.credentialStdin, "credential-stdin", false, "Read this machine's credential from stdin and save it (the hub uses this; people use konsertd join)")
	fs.StringVar(&o.workerID, "id", "", "Stable worker id, a UUID (defaults to the persisted identity in ~/.konsert/worker-id)")
	fs.StringVar(&o.name, "name", "", "Worker display name (defaults to hostname)")
	fs.StringVar(&o.hostGroup, "host-group", "", "Physical host group name (defaults to name)")
	fs.StringVar(&o.isolation, "isolation", "", "Isolation tier: host, container, or vm")
	fs.BoolVar(&o.sandbox, "sandbox", false, "Opt-in sandboxed mode (disables YOLO/skip-permissions flags)")
	fs.BoolVar(&o.sandboxShell, "sandbox-allow-shell", false, "With --sandbox, still accept the unrestricted shell driver (env KONSERTD_SANDBOX_ALLOW_SHELL=1)")
	fs.StringSliceVar(&o.allowedRoots, "allowed-roots", nil, "Opt-in comma-separated list of allowed workdir roots")
	fs.StringSliceVar(&o.tags, "tag", nil, "Worker tags in key=value format (repeatable)")
	fs.IntVar(&o.maxConcurrent, "max-concurrent", 2, "Maximum concurrent tasks on this worker")
	fs.BoolVar(&o.enableMockDriver, "enable-mock-driver", false, "Expose the built-in deterministic mock driver")
	fs.StringVar(&o.logFile, "log-file", "", "Append logs to this file instead of stderr (used by the Windows logon task and service)")
}

// resolve loads the config file and overlays every explicitly set flag, then validates.
func (o *runOptions) resolve(fs *pflag.FlagSet) (config.WorkerConfig, error) {
	cfg, err := config.LoadWorkerConfig(o.configPath)
	if err != nil {
		return cfg, err
	}
	if fs.Changed("controller") {
		cfg.ControllerURL = o.controllerURL
	}
	if fs.Changed("id") {
		cfg.WorkerID = o.workerID
	}
	if fs.Changed("name") {
		cfg.Name = o.name
	}
	if fs.Changed("host-group") {
		cfg.HostGroup = o.hostGroup
	}
	if fs.Changed("isolation") {
		cfg.Isolation = o.isolation
	}
	if fs.Changed("sandbox") {
		cfg.Sandbox = o.sandbox
	}
	if fs.Changed("allowed-roots") {
		cfg.AllowedRoots = o.allowedRoots
	}
	if fs.Changed("max-concurrent") {
		cfg.MaxConcurrent = o.maxConcurrent
	}
	if fs.Changed("enable-mock-driver") {
		cfg.EnableMockDriver = o.enableMockDriver
	}
	for _, kv := range o.tags {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.TrimSpace(k) != "" {
			cfg.Tags[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("invalid worker config: %w", err)
	}
	return cfg, nil
}

func newRunCmd() *cobra.Command {
	var o runOptions
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Start the konsertd worker daemon and connect outbound to konsert",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := o.resolve(cmd.Flags())
			if err != nil {
				return err
			}
			if o.credentialStdin {
				cred, err := readLine(bufio.NewReader(cmd.InOrStdin()))
				if err != nil {
					return fmt.Errorf("read the credential from stdin: %w", err)
				}
				cfg.Credential = cred
				if err := worker.SaveCredential("", worker.WorkerIDFor(cfg), cred); err != nil {
					return fmt.Errorf("save the machine credential: %w", err)
				}
			}
			return runWorker(cmd, &o, cfg)
		},
	}
	o.bind(cmd.Flags())
	return cmd
}

// runWorker runs the daemon with cfg until it stops.
func runWorker(cmd *cobra.Command, o *runOptions, cfg config.WorkerConfig) error {
	var logOut io.Writer = os.Stderr
	if o.logFile != "" {
		f, err := openLogFile(o.logFile)
		if err != nil {
			return fmt.Errorf("open --log-file: %w", err)
		}
		defer func() { _ = f.Close() }()
		logOut = f
	}
	logger := logx.New(logOut, slog.LevelInfo)
	if cfg.Credential == "" {
		cfg.Credential, _ = worker.LoadCredential("", worker.WorkerIDFor(cfg))
	}
	if cfg.Credential == "" {
		logger.Warn("no machine credential; only a --no-auth hub accepts this")
	}
	daemon := worker.NewDaemon(cfg, drivers.DefaultRegistry, logger)
	sandboxShell := o.sandboxShell
	if !cmd.Flags().Changed("sandbox-allow-shell") {
		sandboxShell = envBool("KONSERTD_SANDBOX_ALLOW_SHELL")
	}
	if cfg.Sandbox && sandboxShell {
		logger.Warn("sandboxed worker opted in to the unrestricted shell driver (--sandbox-allow-shell)")
	}
	daemon.SetAllowSandboxShell(sandboxShell)
	daemon.SetVersion(version)

	err := runDaemon(daemon.Run)
	if err != nil && err != context.Canceled {
		return err
	}
	return nil
}

// newIDCmd prints the worker identity (the hub's workers.id) that `konsertd run` with the
// same flags/config would register with, creating the persisted identity if absent.
func newIDCmd() *cobra.Command {
	var o runOptions
	cmd := &cobra.Command{
		Use:   "id [run flags]",
		Short: "Print the worker id `konsertd run` with the same flags would use (creating it if needed)",
		Long: `Print the persistent worker identity (a UUIDv7, the hub's workers.id) that
` + "`konsertd run`" + ` with the same --config/--name/--isolation would connect with. The identity
file is created if it does not exist yet, so the daemon started afterwards uses the same
id. The hub runs this (locally or over SSH) to know a host before its daemon connects.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := o.resolve(cmd.Flags())
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), worker.WorkerIDFor(cfg))
			return nil
		},
	}
	o.bind(cmd.Flags())
	_ = cmd.Flags().MarkHidden("log-file")
	_ = cmd.Flags().MarkHidden("credential-stdin")
	return cmd
}

// envBool reports whether the named environment variable is set to a truthy value.
func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func newDoctorCmd() *cobra.Command {
	var (
		configPath       string
		controllerURL    string
		sandbox          bool
		enableMockDriver bool
	)

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Verify installed agent CLIs, git availability, PTY support, and controller connectivity",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadWorkerConfig(configPath)
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("controller") {
				cfg.ControllerURL = controllerURL
			}
			if cmd.Flags().Changed("sandbox") {
				cfg.Sandbox = sandbox
			}
			if cmd.Flags().Changed("enable-mock-driver") {
				cfg.EnableMockDriver = enableMockDriver
			}

			permMode := protocol.PermissionFullAccess
			if cfg.Sandbox {
				permMode = protocol.PermissionSandboxed
			}

			fmt.Printf("konsertd doctor (%s/%s)\n", runtime.GOOS, runtime.GOARCH)
			fmt.Printf("  Isolation Tier   : %s\n", cfg.Isolation)
			fmt.Printf("  Permission Mode  : %s\n", permMode)
			if len(cfg.AllowedRoots) > 0 {
				fmt.Printf("  Allowed Roots    : %s\n", strings.Join(cfg.AllowedRoots, ", "))
			} else {
				fmt.Printf("  Allowed Roots    : (unrestricted)\n")
			}

			fmt.Println("\nAgent Drivers:")
			for _, d := range drivers.DefaultRegistry.All() {
				if d.Name() == "mock" && !cfg.EnableMockDriver {
					continue
				}
				if p, ok := d.Detect(); ok {
					fmt.Printf("  [OK] %-12s -> %s\n", d.Name(), p)
				} else {
					fmt.Printf("  [--] %-12s -> not found in PATH\n", d.Name())
				}
			}

			fmt.Println("\nSystem Checks:")
			if gitPath, err := exec.LookPath("git"); err == nil {
				fmt.Printf("  [OK] git      -> %s\n", gitPath)
			} else {
				fmt.Printf("  [!!] git      -> not found (worktree isolation unavailable)\n")
			}

			// Verify PTY allocation works.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			mockDrv, _ := drivers.DefaultRegistry.Get("mock")
			spec := mockDrv.BuildCommand(drivers.LaunchOptions{Prompt: "doctor-check", PermissionMode: permMode})
			proc, ptyErr := worker.StartPty(ctx, worker.PtyStartOptions{
				Binary: spec.Binary,
				Args:   spec.Args,
				Cols:   80,
				Rows:   24,
			})
			if ptyErr != nil {
				fmt.Printf("  [!!] pty      -> failed: %v\n", ptyErr)
			} else {
				drainDone := make(chan struct{})
				go func() {
					defer close(drainDone)
					buf := make([]byte, 4096)
					for {
						if _, err := proc.Read(buf); err != nil {
							return
						}
					}
				}()
				_, _ = proc.Wait()
				_ = proc.Close()
				select {
				case <-drainDone:
				case <-time.After(500 * time.Millisecond):
				}
				fmt.Printf("  [OK] pty      -> native PTY allocation succeeded\n")
			}

			// Optional controller health check.
			healthURL := strings.Replace(cfg.ControllerURL, "ws://", "http://", 1)
			healthURL = strings.Replace(healthURL, "wss://", "https://", 1)
			healthURL = strings.TrimSuffix(healthURL, "/ws/worker") + "/api/health"
			client := &http.Client{Timeout: 3 * time.Second}
			resp, httpErr := client.Get(healthURL)
			if httpErr != nil {
				fmt.Printf("  [--] hub      -> %s unreachable (%v)\n", healthURL, httpErr)
			} else {
				_ = resp.Body.Close()
				fmt.Printf("  [OK] hub      -> %s (HTTP %d)\n", healthURL, resp.StatusCode)
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&configPath, "config", "c", "", "Path to worker YAML config file")
	cmd.Flags().StringVar(&controllerURL, "controller", "ws://127.0.0.1:8420/ws/worker", "Controller WebSocket URL")
	cmd.Flags().BoolVar(&sandbox, "sandbox", false, "Check with sandboxed permission posture")
	cmd.Flags().BoolVar(&enableMockDriver, "enable-mock-driver", false, "Include mock driver in output")
	return cmd
}

func newInstallHooksCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install-hooks",
		Short: "Idempotently install Konsert telemetry hooks and skills for local coding agents",
		RunE: func(_ *cobra.Command, _ []string) error {
			exe, _ := os.Executable()
			if exe == "" {
				exe = "konsertd"
			}
			rep, err := worker.InstallAgentHooks("", exe)
			if err != nil {
				return err
			}
			fmt.Printf("Konsert hooks installed:\n")
			fmt.Printf("  Claude Code hooks : %v\n", rep.ClaudeHooksInstalled)
			fmt.Printf("  Antigravity skill : %v\n", rep.AgySkillInstalled)
			fmt.Printf("  Notify shim       : %s\n", rep.NotifyScriptPath)
			fmt.Printf("  Events inbox      : %s\n", rep.EventsFilePath)
			return nil
		},
	}
}

func newHookEventCmd() *cobra.Command {
	var (
		agent     string
		event     string
		sessionID string
		cwd       string
		filePath  string
		model     string
		message   string
	)
	cmd := &cobra.Command{
		Use:   "hook-event",
		Short: "Record a lightweight agent hook telemetry event for konsertd",
		RunE: func(_ *cobra.Command, _ []string) error {
			if strings.TrimSpace(agent) == "" {
				agent = "claude"
			}
			if strings.TrimSpace(event) == "" {
				event = "working"
			}
			ev := worker.HookEvent{
				SessionID: sessionID,
				Agent:     agent,
				Event:     event,
				Cwd:       cwd,
				FilePath:  filePath,
				Model:     model,
				Message:   message,
			}
			// Claude Code (and compatible agents) pass the session, cwd and tool on stdin.
			worker.ReadHookStdin(&ev)
			return worker.RecordHookEvent("", ev)
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "claude", "Agent name (claude, agy, codex, grok, copilot, cursor)")
	cmd.Flags().StringVar(&event, "event", "working", "Hook event name or status")
	cmd.Flags().StringVar(&sessionID, "session-id", "", "External session identifier")
	cmd.Flags().StringVar(&cwd, "cwd", "", "Working directory (defaults to current directory)")
	cmd.Flags().StringVar(&filePath, "file", "", "Optional modified file path")
	cmd.Flags().StringVar(&model, "model", "", "Optional model name")
	cmd.Flags().StringVar(&message, "message", "", "Optional short milestone message")
	return cmd
}

func newDaemonMCPCmd() *cobra.Command {
	var (
		configPath    string
		controllerURL string
		token         string
	)
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run a local stdio MCP server bridge for coding agents on this machine to drive Konsert",
		Long: `Run a local stdio MCP server bridge for coding agents on this machine to drive Konsert.

The bridge calls the hub's /api endpoints, which require the hub's ADMIN token
(this machine's credential only authorizes its worker connection on /ws/worker
and is never used here).

Hub URL:   --controller, else $KONSERT_URL, else the worker config's controller_url
           (ws:// -> http://, trailing /ws/worker removed).
Token:     --token, else $KONSERT_ADMIN_TOKEN. With neither set, requests are sent
           unauthenticated, which only works against a hub started with --no-auth.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadWorkerConfig(configPath)
			if err != nil {
				return err
			}
			var flagURL, flagToken *string
			if cmd.Flags().Changed("controller") {
				flagURL = &controllerURL
			}
			if cmd.Flags().Changed("token") {
				flagToken = &token
			}
			base, tok := resolveMCPBridge(cfg, flagURL, flagToken, os.Getenv)
			if tok == "" {
				fmt.Fprintln(os.Stderr, "konsertd mcp: no admin token (--token or KONSERT_ADMIN_TOKEN); calls will fail unless the hub runs with --no-auth")
			}
			client := controller.NewRemoteFleetClient(base, tok)
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return controller.RunMCPStdio(ctx, client, version)
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", "", "Path to worker YAML config file")
	cmd.Flags().StringVar(&controllerURL, "controller", "", "Konsert hub URL (default $KONSERT_URL, else the worker config controller_url)")
	cmd.Flags().StringVar(&token, "token", "", "Konsert hub ADMIN token for /api calls (default $KONSERT_ADMIN_TOKEN; machine credentials are not accepted there)")
	return cmd
}

// resolveMCPBridge picks the hub base URL and bearer token for `konsertd mcp`. The token is
// the hub's admin token (flag, then KONSERT_ADMIN_TOKEN); the machine credential is never
// used because the hub's /api rejects it.
func resolveMCPBridge(cfg config.WorkerConfig, flagURL, flagToken *string, getenv func(string) string) (base, token string) {
	switch {
	case flagURL != nil && strings.TrimSpace(*flagURL) != "":
		base = *flagURL
	case strings.TrimSpace(getenv("KONSERT_URL")) != "":
		base = getenv("KONSERT_URL")
	default:
		base = cfg.ControllerURL
	}
	base = strings.TrimSpace(base)
	base = strings.Replace(base, "ws://", "http://", 1)
	base = strings.Replace(base, "wss://", "https://", 1)
	base = strings.TrimSuffix(strings.TrimSuffix(base, "/"), "/ws/worker")
	if base == "" {
		base = "http://127.0.0.1:8420"
	}

	if flagToken != nil {
		token = strings.TrimSpace(*flagToken)
	} else {
		token = strings.TrimSpace(getenv("KONSERT_ADMIN_TOKEN"))
	}
	return base, token
}
