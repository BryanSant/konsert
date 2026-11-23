package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/install"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
)

// installOnlyFlags are flags of install itself, never persisted as run flags.
var installOnlyFlags = map[string]bool{
	"status": true, "service": true, "user": true, "password-stdin": true,
	"token-stdin": true, "no-start": true, "no-linger": true, "dry-run": true,
}

func newInstallCmd() *cobra.Command {
	var (
		showStatus    bool
		service       bool
		account       string
		passwordStdin bool
		tokenStdin    bool
		noStart       bool
		noLinger      bool
		dryRun        bool
	)
	// install accepts every `run` flag; sharing run's flag set keeps them in sync.
	runCmd := newRunCmd()
	cmd := &cobra.Command{
		Use:   "install [run flags]",
		Short: "Start konsertd automatically as the current user (systemd user unit, LaunchAgent, or Windows logon task/service)",
		Long: `Register konsertd to start automatically as the current user, so agent CLIs use your
logins (profile, keychain, Credential Manager), and start it now.

  Linux    systemd user unit ~/.config/systemd/user/konsertd.service, enabled with
           systemctl --user enable; lingering is enabled when allowed (--no-linger to skip)
  macOS    LaunchAgent ~/Library/LaunchAgents/dev.konsert.konsertd.plist in gui/<uid>,
           logs in ~/Library/Logs/konsert/
  Windows  Scheduled Task \konsertd at your logon (not elevated, no console window),
           logs in %LOCALAPPDATA%\konsert\logs; or, with --service --user .\NAME, a
           native service running as that account (needs an elevated prompt)

The run flags given here (--controller, --name, --sandbox, --tag, ...) and the fleet
token are written to a per-user config file readable only by you (0600, or a user-only
ACL on Windows), which the daemon reads with --config. The token never appears in the
unit, plist or task definition. Pass it with --token, KONSERT_FLEET_TOKEN or
--token-stdin; a reinstall without one keeps the token already installed. --config
merges an existing YAML file into the installed one.

--name other than the hostname installs a separate instance (konsertd-<name>), so
several daemons can run side by side. Re-running install updates the instance in place.`,
		Example: `  konsertd install --controller wss://hub.example.ts.net/ws/worker --token-stdin < token.txt
  KONSERT_FLEET_TOKEN=... konsertd install --controller wss://hub/ws/worker --sandbox
  konsertd install --name sandboxed --sandbox --controller wss://hub/ws/worker
  konsertd.exe install --service --user .\builder --controller wss://hub/ws/worker
  konsertd status
  konsertd uninstall`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			fs := cmd.Flags()
			if showStatus {
				name, _ := fs.GetString("name")
				return printInstallStatus(ctx, cmd.OutOrStdout(), name, service)
			}
			if passwordStdin && tokenStdin {
				return errors.New("--password-stdin and --token-stdin both read stdin; pass the token with KONSERT_FLEET_TOKEN instead")
			}
			if passwordStdin && !service {
				return errors.New("--password-stdin only applies with --service")
			}
			stdin := bufio.NewReader(cmd.InOrStdin())

			cfg, extra, err := buildInstallConfig(fs, os.Getenv)
			if err != nil {
				return err
			}
			if tokenStdin {
				tok, err := readLine(stdin)
				if err != nil {
					return fmt.Errorf("read token from stdin: %w", err)
				}
				cfg.FleetToken = tok
			}

			exe, err := installExecutable()
			if err != nil {
				return err
			}
			host, _ := os.Hostname()
			instance := install.InstanceFor(cfg.Name, host)
			env, err := install.DefaultEnv()
			if err != nil {
				return err
			}
			in := &install.Installer{Runner: install.ExecRunner{}, Env: env, Out: cmd.OutOrStdout()}

			if cfg.FleetToken == "" && !service {
				if old, ok := in.ExistingConfig(instance); ok {
					if prev, err := install.ParseWorkerConfig(old); err == nil && prev.FleetToken != "" {
						cfg.FleetToken = prev.FleetToken
						fmt.Fprintln(cmd.OutOrStdout(), "Keeping the fleet token from the existing install.")
					}
				}
			}
			if cfg.FleetToken == "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "Warning: no fleet token (--token, KONSERT_FLEET_TOKEN or --token-stdin); only a hub started with --no-auth will accept this worker.")
			}
			if cfg.ControllerURL == "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "Warning: no --controller given; the daemon will connect to the default ws://127.0.0.1:8420/ws/worker.")
			}
			data, err := install.MarshalWorkerConfig(cfg)
			if err != nil {
				return err
			}
			if err := validateInstalledConfig(data); err != nil {
				return err
			}
			spec := install.Spec{
				Instance:  instance,
				Exe:       exe,
				Config:    data,
				ExtraArgs: extra,
				NoStart:   noStart,
				NoLinger:  noLinger,
			}

			if dryRun {
				return printDryRun(cmd.OutOrStdout(), in, spec, service)
			}
			if service {
				opts := install.ServiceOptions{Account: account}
				// Without Windows, elevation or an account, fail before asking for a password.
				if !install.ServiceSupported || !install.IsElevated() || strings.TrimSpace(account) == "" {
					return in.InstallService(ctx, spec, opts)
				}
				if passwordStdin {
					opts.Password, err = readLine(stdin)
				} else {
					opts.Password, err = install.ReadPassword("Password for " + account + ": ")
				}
				if err != nil {
					return err
				}
				if err := in.InstallService(ctx, spec, opts); err != nil {
					return err
				}
			} else if err := in.Install(ctx, spec); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Check it with: konsertd status%s\n", statusHint(cfg.Name, instance, service))
			return nil
		},
	}
	cmd.Flags().AddFlagSet(runCmd.Flags())
	_ = cmd.Flags().MarkHidden("log-file")
	cmd.Flags().BoolVar(&showStatus, "status", false, "Show whether konsertd is installed and running (same as `konsertd status`)")
	cmd.Flags().BoolVar(&service, "service", false, "Windows: install a native service running as --user instead of a logon task (needs an elevated prompt)")
	cmd.Flags().StringVar(&account, "user", "", `Windows --service: account to run as, e.g. .\alice or DOMAIN\alice`)
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false, "Windows --service: read the account password from stdin instead of prompting")
	cmd.Flags().BoolVar(&tokenStdin, "token-stdin", false, "Read the fleet token from stdin (keeps it out of shell history and the process list)")
	cmd.Flags().BoolVar(&noStart, "no-start", false, "Register only; start at the next login/boot")
	cmd.Flags().BoolVar(&noLinger, "no-linger", false, "Linux: do not run `loginctl enable-linger`")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print the definition and config that would be installed (token redacted) and change nothing")
	return cmd
}

func newUninstallCmd() *cobra.Command {
	var (
		name    string
		service bool
	)
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop konsertd and remove what `konsertd install` registered",
		Long: `Stop konsertd and remove the unit, LaunchAgent, scheduled task or service that
` + "`konsertd install`" + ` created, plus its installed config file. Pass the same --name used at
install time to remove a named instance. Log files and the state directory
(~/.konsert: worker ID, machine ID) are kept, so a reinstall keeps the same identity.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			host, _ := os.Hostname()
			instance := install.InstanceFor(name, host)
			env, err := install.DefaultEnv()
			if err != nil {
				return err
			}
			in := &install.Installer{Runner: install.ExecRunner{}, Env: env, Out: cmd.OutOrStdout()}
			if service {
				return in.UninstallService(cmd.Context(), instance)
			}
			return in.Uninstall(cmd.Context(), instance)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Instance to remove: the --name given to install")
	cmd.Flags().BoolVar(&service, "service", false, "Windows: remove the native service instead of the logon task (needs an elevated prompt)")
	return cmd
}

func newStatusCmd() *cobra.Command {
	var (
		name    string
		service bool
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether konsertd is installed to start automatically, and whether it is running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return printInstallStatus(cmd.Context(), cmd.OutOrStdout(), name, service)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Instance to inspect: the --name given to install")
	cmd.Flags().BoolVar(&service, "service", false, "Windows: inspect the native service instead of the logon task")
	return cmd
}

func printInstallStatus(ctx context.Context, w io.Writer, name string, service bool) error {
	host, _ := os.Hostname()
	instance := install.InstanceFor(name, host)
	env, err := install.DefaultEnv()
	if err != nil {
		return err
	}
	in := &install.Installer{Runner: install.ExecRunner{}, Env: env, Out: w}
	var st install.Status
	if service {
		st, err = in.ServiceStatus(ctx, instance)
	} else {
		st, err = in.Status(ctx, instance)
	}
	if err != nil {
		return err
	}
	writeStatus(w, st)
	return nil
}

func writeStatus(w io.Writer, st install.Status) {
	yesNo := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	fmt.Fprintf(w, "konsertd %s\n", st.Name)
	fmt.Fprintf(w, "  Mechanism  : %s\n", st.Mechanism)
	fmt.Fprintf(w, "  Installed  : %s\n", yesNo(st.Installed))
	running := yesNo(st.Running)
	if st.Running && st.PID > 0 {
		running += fmt.Sprintf(" (pid %d)", st.PID)
	}
	fmt.Fprintf(w, "  Running    : %s\n", running)
	if st.Detail != "" {
		fmt.Fprintf(w, "  State      : %s\n", st.Detail)
	}
	if st.DefinitionPath != "" && st.DefinitionPath != st.Name {
		fmt.Fprintf(w, "  Definition : %s\n", st.DefinitionPath)
	}
	fmt.Fprintf(w, "  Config     : %s\n", st.ConfigPath)
	fmt.Fprintf(w, "  Logs       : %s\n", st.LogPath)
}

func statusHint(name, instance string, service bool) string {
	s := ""
	if instance != "" {
		s += " --name " + quoteIfNeeded(name)
	}
	if service {
		s += " --service"
	}
	return s
}

func quoteIfNeeded(s string) string {
	if strings.ContainsAny(s, " \t\"'") {
		return fmt.Sprintf("%q", s)
	}
	return s
}

// buildInstallConfig turns the run flags given to install into the config to persist:
// the --config file's contents (if any), overlaid with every explicitly set flag, plus
// KONSERT_FLEET_TOKEN when no token was given otherwise. Flags without a config field
// are returned as extra run arguments.
func buildInstallConfig(fs *pflag.FlagSet, getenv func(string) string) (config.WorkerConfig, []string, error) {
	var cfg config.WorkerConfig
	if fs.Changed("config") {
		path, _ := fs.GetString("config")
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, nil, fmt.Errorf("read --config: %w", err)
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, nil, fmt.Errorf("parse --config %s: %w", path, err)
		}
	}
	var extra []string
	var ferr error
	fs.Visit(func(f *pflag.Flag) {
		if installOnlyFlags[f.Name] {
			return
		}
		var err error
		switch f.Name {
		case "config", "log-file":
		case "controller":
			cfg.ControllerURL, err = fs.GetString(f.Name)
		case "token":
			cfg.FleetToken, err = fs.GetString(f.Name)
		case "id":
			cfg.WorkerID, err = fs.GetString(f.Name)
		case "name":
			cfg.Name, err = fs.GetString(f.Name)
		case "host-group":
			cfg.HostGroup, err = fs.GetString(f.Name)
		case "isolation":
			cfg.Isolation, err = fs.GetString(f.Name)
		case "sandbox":
			cfg.Sandbox, err = fs.GetBool(f.Name)
		case "allowed-roots":
			cfg.AllowedRoots, err = fs.GetStringSlice(f.Name)
		case "max-concurrent":
			cfg.MaxConcurrent, err = fs.GetInt(f.Name)
		case "enable-mock-driver":
			cfg.EnableMockDriver, err = fs.GetBool(f.Name)
		case "tag":
			var tags []string
			tags, err = fs.GetStringSlice(f.Name)
			for _, kv := range tags {
				k, v, ok := strings.Cut(kv, "=")
				if !ok || strings.TrimSpace(k) == "" {
					err = fmt.Errorf("--tag %q: want key=value", kv)
					break
				}
				if cfg.Tags == nil {
					cfg.Tags = map[string]string{}
				}
				cfg.Tags[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		default:
			// A run flag with no config field (e.g. --sandbox-allow-shell): keep it on
			// the command line. None of these are secrets.
			val := f.Value.String()
			if sv, ok := f.Value.(pflag.SliceValue); ok {
				val = strings.Join(sv.GetSlice(), ",")
			}
			extra = append(extra, "--"+f.Name+"="+val)
		}
		if err != nil && ferr == nil {
			ferr = err
		}
	})
	if ferr != nil {
		return cfg, nil, ferr
	}
	if cfg.FleetToken == "" {
		cfg.FleetToken = strings.TrimSpace(getenv("KONSERT_FLEET_TOKEN"))
	}
	return cfg, extra, nil
}

// validateInstalledConfig checks the persisted config the way `run` will load it.
func validateInstalledConfig(data []byte) error {
	full := config.DefaultWorkerConfig()
	if err := yaml.Unmarshal(data, &full); err != nil {
		return err
	}
	if full.MaxConcurrent <= 0 {
		full.MaxConcurrent = 2
	}
	if err := full.Validate(); err != nil {
		return fmt.Errorf("invalid worker config: %w", err)
	}
	return nil
}

// installExecutable is the absolute, symlink-resolved path of this konsertd binary,
// which the definition will run. A binary from `go run` or a temp dir would vanish.
func installExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate the konsertd binary: %w", err)
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return "", err
	}
	if strings.Contains(exe, "go-build") {
		return "", fmt.Errorf("%s looks like a `go run` build that will be deleted; build or install konsertd first (go build ./cmd/konsertd) and run install from that binary", exe)
	}
	if tmp := os.TempDir(); tmp != "" && strings.HasPrefix(strings.ToLower(exe), strings.ToLower(filepath.Clean(tmp))+string(filepath.Separator)) {
		fmt.Fprintf(os.Stderr, "Warning: %s is in the temp directory; the installed daemon runs this exact file, so move it somewhere permanent first if the temp dir gets cleaned.\n", exe)
	}
	return exe, nil
}

var tokenLineRe = regexp.MustCompile(`(?m)^(fleet_token:\s*).+$`)

// redactToken hides the fleet token in a rendered config.
func redactToken(data []byte) []byte {
	return tokenLineRe.ReplaceAll(data, []byte("${1}<redacted>"))
}

func printDryRun(w io.Writer, in *install.Installer, spec install.Spec, service bool) error {
	if service {
		fmt.Fprintln(w, "--dry-run with --service: would create the Windows service konsertd"+suffix(spec.Instance)+" running:")
		fmt.Fprintf(w, "  %s run --config <%%ProgramData%%\\konsert\\konsertd%s.yaml> --log-file <...> %s\n", spec.Exe, suffix(spec.Instance), strings.Join(spec.ExtraArgs, " "))
	} else {
		path, content, err := in.Render(spec)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "# %s\n%s\n", path, content)
		fmt.Fprintf(w, "# %s (readable only by you)\n", in.ConfigPath(spec.Instance))
	}
	_, _ = w.Write(redactToken(spec.Config))
	return nil
}

func suffix(instance string) string {
	if instance == "" {
		return ""
	}
	return "-" + instance
}

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		if errors.Is(err, io.EOF) {
			return "", errors.New("stdin was empty")
		}
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// runDaemon runs the daemon until it stops: under the Windows service control manager
// when started by it, otherwise until SIGINT/SIGTERM (on Windows also console close,
// logoff and shutdown, which Go delivers as SIGTERM).
func runDaemon(run func(context.Context) error) error {
	if handled, err := install.RunAsServiceIfNeeded(run); handled {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// Under the Windows logon task, also stop cleanly when the hidden conhost is ended.
	ctx, stop := install.StopWithHeadlessHost(ctx)
	defer stop()
	return run(ctx)
}

// logRotateSize is the size above which openLogFile moves the old log aside.
const logRotateSize = 10 << 20

// openLogFile opens --log-file for appending (a detached daemon under Task Scheduler or
// the SCM has no usable stderr), keeping one previous generation once it exceeds 10 MiB.
func openLogFile(path string) (io.WriteCloser, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > logRotateSize {
		_ = os.Remove(path + ".1")
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(f, "\n--- konsertd %s starting %s ---\n", version, time.Now().Format(time.RFC3339))
	return f, nil
}
