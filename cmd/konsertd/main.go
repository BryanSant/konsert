package main

import (
	"context"
	"fmt"
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
	"github.com/BryanSant/konsert/internal/protocol"
	"github.com/BryanSant/konsert/internal/worker"
	"github.com/BryanSant/konsert/internal/worker/drivers"
	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	rootCmd := &cobra.Command{
		Use:     "konsertd",
		Short:   "Konsert cross-platform worker daemon",
		Version: version,
	}

	rootCmd.AddCommand(newRunCmd())
	rootCmd.AddCommand(newDoctorCmd())
	rootCmd.AddCommand(newInstallHooksCmd())
	rootCmd.AddCommand(newHookEventCmd())
	rootCmd.AddCommand(newDaemonMCPCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func newRunCmd() *cobra.Command {
	var (
		configPath       string
		controllerURL    string
		fleetToken       string
		workerID         string
		name             string
		hostGroup        string
		isolation        string
		sandbox          bool
		allowedRoots     []string
		tags             []string
		maxConcurrent    int
		enableMockDriver bool
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Start the konsertd worker daemon and connect outbound to konsert",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadWorkerConfig(configPath)
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("controller") {
				cfg.ControllerURL = controllerURL
			}
			if cmd.Flags().Changed("token") {
				cfg.FleetToken = fleetToken
			}
			if cmd.Flags().Changed("id") {
				cfg.WorkerID = workerID
			}
			if cmd.Flags().Changed("name") {
				cfg.Name = name
			}
			if cmd.Flags().Changed("host-group") {
				cfg.HostGroup = hostGroup
			}
			if cmd.Flags().Changed("isolation") {
				cfg.Isolation = isolation
			}
			if cmd.Flags().Changed("sandbox") {
				cfg.Sandbox = sandbox
			}
			if cmd.Flags().Changed("allowed-roots") {
				cfg.AllowedRoots = allowedRoots
			}
			if cmd.Flags().Changed("max-concurrent") {
				cfg.MaxConcurrent = maxConcurrent
			}
			if cmd.Flags().Changed("enable-mock-driver") {
				cfg.EnableMockDriver = enableMockDriver
			}
			for _, kv := range tags {
				k, v, ok := strings.Cut(kv, "=")
				if ok && strings.TrimSpace(k) != "" {
					cfg.Tags[strings.TrimSpace(k)] = strings.TrimSpace(v)
				}
			}

			logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
			daemon := worker.NewDaemon(cfg, drivers.DefaultRegistry, logger)

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			err = daemon.Run(ctx)
			if err != nil && err != context.Canceled {
				return err
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&configPath, "config", "c", "", "Path to worker YAML config file")
	cmd.Flags().StringVar(&controllerURL, "controller", "ws://127.0.0.1:8420/ws/worker", "Controller WebSocket URL")
	cmd.Flags().StringVar(&fleetToken, "token", "", "Pre-shared fleet authentication token")
	cmd.Flags().StringVar(&workerID, "id", "", "Stable worker ID (defaults to persisted ~/.konsert/worker-id)")
	cmd.Flags().StringVar(&name, "name", "", "Worker display name (defaults to hostname)")
	cmd.Flags().StringVar(&hostGroup, "host-group", "", "Physical host group name (defaults to name)")
	cmd.Flags().StringVar(&isolation, "isolation", "", "Isolation tier: host, container, or vm")
	cmd.Flags().BoolVar(&sandbox, "sandbox", false, "Opt-in sandboxed mode (disables YOLO/skip-permissions flags)")
	cmd.Flags().StringSliceVar(&allowedRoots, "allowed-roots", nil, "Opt-in comma-separated list of allowed workdir roots")
	cmd.Flags().StringSliceVar(&tags, "tag", nil, "Worker tags in key=value format (repeatable)")
	cmd.Flags().IntVar(&maxConcurrent, "max-concurrent", 2, "Maximum concurrent tasks on this worker")
	cmd.Flags().BoolVar(&enableMockDriver, "enable-mock-driver", false, "Expose the built-in deterministic mock driver")

	return cmd
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
					fmt.Printf("  [OK] %-8s -> %s\n", d.Name(), p)
				} else {
					fmt.Printf("  [--] %-8s -> not found in PATH\n", d.Name())
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
			return worker.RecordHookEvent("", worker.HookEvent{
				SessionID: sessionID,
				Agent:     agent,
				Event:     event,
				Cwd:       cwd,
				FilePath:  filePath,
				Model:     model,
				Message:   message,
			})
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "claude", "Agent name (claude, agy, codex, grok)")
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
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadWorkerConfig(configPath)
			if err != nil {
				return err
			}
			base := cfg.ControllerURL
			if cmd.Flags().Changed("controller") {
				base = controllerURL
			}
			base = strings.Replace(base, "ws://", "http://", 1)
			base = strings.Replace(base, "wss://", "https://", 1)
			base = strings.TrimSuffix(base, "/ws/worker")
			tok := cfg.FleetToken
			if cmd.Flags().Changed("token") {
				tok = token
			}
			client := controller.NewRemoteFleetClient(base, tok)
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return controller.RunMCPStdio(ctx, client, version)
		},
	}
	cmd.Flags().StringVarP(&configPath, "config", "c", "", "Path to worker YAML config file")
	cmd.Flags().StringVar(&controllerURL, "controller", "http://127.0.0.1:8420", "Konsert controller HTTP/WS URL")
	cmd.Flags().StringVar(&token, "token", "", "Konsert MCP/Admin auth token")
	return cmd
}
