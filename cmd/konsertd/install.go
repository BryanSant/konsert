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
	"strings"
	"syscall"
	"time"

	"github.com/BryanSant/konsert/internal/config"
	"github.com/BryanSant/konsert/internal/elevate"
	"github.com/BryanSant/konsert/internal/install"
	"github.com/BryanSant/konsert/internal/worker"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
)

// installOnlyFlags are flags of install (and join) itself, never persisted as run flags.
var installOnlyFlags = map[string]bool{
	"status": true, "service": true, "user": true, "password-stdin": true,
	"no-start": true, "no-linger": true, "dry-run": true,
	"foreground": true, "no-elevate": true,
}

// installFlags are install's own flags; join accepts them too.
type installFlags struct {
	showStatus    bool
	service       bool
	account       string
	passwordStdin bool
	noStart       bool
	noLinger      bool
	dryRun        bool
	noElevate     bool
}

func (f *installFlags) bind(fs *pflag.FlagSet) {
	fs.BoolVar(&f.showStatus, "status", false, "Show whether konsertd is installed and running (same as `konsertd status`)")
	fs.BoolVar(&f.service, "service", false, "Windows: install a native service running as --user instead of a scheduled task (needs administrator rights: asks for approval (UAC) at the desktop)")
	fs.StringVar(&f.account, "user", "", `Windows --service: account to run as, e.g. .\alice or DOMAIN\alice`)
	fs.BoolVar(&f.passwordStdin, "password-stdin", false, "Windows --service: read the account password from stdin instead of prompting")
	fs.BoolVar(&f.noStart, "no-start", false, "Register only; start at the next login/boot")
	fs.BoolVar(&f.noLinger, "no-linger", false, "Linux: do not run `loginctl enable-linger`")
	fs.BoolVar(&f.dryRun, "dry-run", false, "Print the definition and config that would be installed and change nothing")
	fs.BoolVar(&f.noElevate, "no-elevate", false, "Windows --service: never ask for approval (UAC); without administrator rights, print what to do instead")
}

func newInstallCmd() *cobra.Command {
	var f installFlags
	// install accepts every `run` flag; sharing run's flag set keeps them in sync.
	runCmd := newRunCmd()
	cmd := &cobra.Command{
		Use:   "install [run flags]",
		Short: "Start konsertd automatically as the current user (systemd user unit, LaunchAgent, or Windows scheduled task/service)",
		Long: `Register konsertd to start automatically as the current user, so agent CLIs use your
logins (profile, keychain, Credential Manager), and start it now.

  Linux    systemd user unit ~/.config/systemd/user/konsertd.service, enabled with
           systemctl --user enable; lingering is enabled when allowed (--no-linger to skip)
  macOS    LaunchAgent ~/Library/LaunchAgents/dev.konsert.konsertd.plist in gui/<uid>,
           logs in ~/Library/Logs/konsert/
  Windows  Scheduled Task \konsertd (no console window), logs in
           %LOCALAPPDATA%\konsert\logs. Run elevated (an administrator over SSH, or
           an elevated terminal) it is an S4U task: it starts at boot and runs whether
           or not you are signed in, with no stored password. Otherwise it is a logon
           task that only runs while you are signed in. Or, with --service --user .\NAME, a
           native service running as that account (needs administrator rights: in
           a terminal at the desktop it asks for approval (UAC) and shows the result
           there; over SSH, connect as an administrator)

The run flags given here (--controller, --name, --sandbox, --tag, ...) are written to a
per-user config file readable only by you (0600, or a user-only ACL on Windows), which
the daemon reads with --config. --config merges an existing YAML file into it.

Without --controller the daemon serves the hub on its local socket and the hub
attaches to it (directly on its own machine, over SSH elsewhere): this is how the hub
installs konsertd itself. With --controller it connects out to the hub with this
machine's own credential: to add a machine the hub cannot reach over SSH, use
` + "`konsertd join <hub-url> <code>`" + ` with a code from Add machine → Manual, which saves the
credential and runs this install.

--name other than the hostname installs a separate instance (konsertd-<name>), so
several daemons can run side by side. Re-running install updates the instance in place.`,
		Example: `  konsertd join https://hub.example.ts.net ABCD-EFGH
  konsertd install --controller wss://hub.example.ts.net/ws/worker --sandbox
  konsertd install --name sandboxed --sandbox --controller wss://hub/ws/worker
  konsertd.exe install --service --user .\builder --controller wss://hub/ws/worker
  konsertd status
  konsertd uninstall`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInstall(cmd, &f)
		},
	}
	cmd.Flags().AddFlagSet(runCmd.Flags())
	_ = cmd.Flags().MarkHidden("log-file")
	f.bind(cmd.Flags())
	return cmd
}

// runInstall is `konsertd install` with the run flags and install flags parsed into cmd's
// flag set (join runs it too, after saving the credential).
func runInstall(cmd *cobra.Command, f *installFlags) error {
	ctx := cmd.Context()
	fs := cmd.Flags()
	if f.showStatus {
		name, _ := fs.GetString("name")
		return printInstallStatus(ctx, cmd.OutOrStdout(), name, f.service, statusStart{})
	}
	if f.passwordStdin && !f.service {
		return errors.New("--password-stdin only applies with --service")
	}
	stdin := bufio.NewReader(cmd.InOrStdin())
	if f.service && !f.dryRun && install.ServiceSupported {
		if strings.TrimSpace(f.account) == "" {
			return errors.New("--service needs --user, the account to run as (e.g. --user .\\alice); agents use that account's CLI logins")
		}
		args := append([]string{"install"}, flagArgs(fs, newInstallCmd().Flags(), map[string]bool{"password-stdin": true, "no-elevate": true})...)
		handled, err := maybeElevate(cmd, elevate.OpInstall, f.noElevate, args, func() (string, error) {
			return readServicePassword(stdin, f)
		})
		if handled {
			return err
		}
	}

	cfg, extra, err := buildInstallConfig(fs)
	if err != nil {
		return err
	}
	data, err := install.MarshalWorkerConfig(cfg)
	if err != nil {
		return err
	}
	runCfg, err := installedRunConfig(data)
	if err != nil {
		return err
	}
	// An outbound daemon authenticates with the credential of the identity it runs as.
	if !f.dryRun {
		// Installing (again) supersedes an earlier removal by the hub.
		worker.ClearRemovedMarker("", worker.WorkerIDFor(runCfg))
	}
	if runCfg.ControllerURL != "" && !f.dryRun {
		if _, err := worker.LoadCredential("", worker.WorkerIDFor(runCfg)); err != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), "Warning: no machine credential yet; run konsertd join <hub-url> <code>")
		}
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
	spec := install.Spec{
		Instance:  instance,
		Exe:       exe,
		Config:    data,
		ExtraArgs: extra,
		NoStart:   f.noStart,
		NoLinger:  f.noLinger,
	}

	if f.dryRun {
		return printDryRun(cmd.OutOrStdout(), in, spec, f.service)
	}
	if f.service {
		opts := install.ServiceOptions{Account: f.account}
		// Without Windows, elevation or an account, fail before asking for a password.
		if !install.ServiceSupported || !install.IsElevated() || strings.TrimSpace(f.account) == "" {
			return in.InstallService(ctx, spec, opts)
		}
		if opts.Password, err = readServicePassword(stdin, f); err != nil {
			return err
		}
		err = in.InstallService(ctx, spec, opts)
		opts.Password = ""
		if err != nil {
			return err
		}
	} else if err := in.Install(ctx, spec); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Check it with: konsertd status%s\n", statusHint(cfg.Name, instance, f.service))
	return nil
}

// readServicePassword is the --service account password: the one the unelevated parent
// handed over the elevation pipe, else from stdin (--password-stdin) or a no-echo
// console prompt. It is never logged, put in argv or the environment, or written down.
func readServicePassword(stdin *bufio.Reader, f *installFlags) (string, error) {
	if elevatedRun.child && elevatedRun.password != "" {
		return elevatedRun.password, nil
	}
	if f.passwordStdin {
		return readLine(stdin)
	}
	return install.ReadPassword("Password for " + f.account + ": ")
}

func newUninstallCmd() *cobra.Command {
	var o uninstallOptions
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop konsertd and remove what `konsertd install` registered",
		Long: `Stop konsertd and remove the unit, LaunchAgent, scheduled task or service that
` + "`konsertd install`" + ` created, plus its installed config, logs, identity file,
credential and per-identity state. Pass the same --name used at install time to remove a
named instance, or --worker to remove whatever is installed for that konsertd identity
(the hub does this when you remove a machine; with --hub it also drops that hub's key
grant).

Shared files (the agent hooks ` + "`konsertd install-hooks`" + ` added,
~/.konsert/hook-events.jsonl, orphaned spool archives, the konsertd binary if konsertd
installed it, and empty konsert directories) are removed only when no other konsertd
identity remains on this account (hosts sharing a home directory, such as a container and
its host, share them); pass --keep-hooks to keep the agent hooks regardless. The rest of
~/.claude/settings.json, ~/.konsert/machine-id, and any files konsertd did not create are
left untouched.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			err := runUninstall(cmd.Context(), cmd.OutOrStdout(), o)
			if o.service && errors.Is(err, install.ErrServiceAccessDenied) {
				// Installed without granting this account the right to remove it
				// (another account's service): elevate the way install does.
				if handled, eerr := maybeElevate(cmd, elevate.OpUninstall, o.noElevate, o.args(), nil); handled {
					return eerr
				}
			}
			return err
		},
	}
	cmd.Flags().StringVar(&o.name, "name", "", "Instance to remove: the --name given to install")
	cmd.Flags().StringVar(&o.workerID, "worker", "", "Remove everything installed for this konsertd identity (worker id)")
	cmd.Flags().StringVar(&o.hubID, "hub", "", "With --worker: also remove this hub's key grant for the identity")
	cmd.Flags().BoolVar(&o.keepHooks, "keep-hooks", false, "Keep the agent hooks install-hooks added")
	cmd.Flags().BoolVar(&o.service, "service", false, "Windows: remove the native service instead of the logon task (no elevation needed: install granted the account the right)")
	cmd.Flags().BoolVar(&o.noElevate, "no-elevate", false, "Windows --service: never ask for approval (UAC) when the account may not remove the service; print what to do instead")
	return cmd
}

type uninstallOptions struct {
	name, workerID, hubID         string
	keepHooks, service, noElevate bool
}

// args is the uninstall command line for the elevated child.
func (o uninstallOptions) args() []string {
	args := []string{"uninstall"}
	if o.name != "" {
		args = append(args, "--name="+o.name)
	}
	if o.workerID != "" {
		args = append(args, "--worker="+o.workerID)
	}
	if o.hubID != "" {
		args = append(args, "--hub="+o.hubID)
	}
	if o.keepHooks {
		args = append(args, "--keep-hooks")
	}
	if o.service {
		args = append(args, "--service")
	}
	return args
}

// runUninstall is `konsertd uninstall`.
func runUninstall(ctx context.Context, out io.Writer, o uninstallOptions) error {
	host, _ := os.Hostname()
	instance := install.InstanceFor(o.name, host)
	env, err := install.DefaultEnv()
	if err != nil {
		return err
	}
	in := &install.Installer{Runner: install.ExecRunner{}, Env: env, Out: out}
	if o.service {
		if err := in.UninstallService(ctx, instance); err != nil {
			return err
		}
		o.workerID = ""
	}
	u := &worker.Uninstaller{Installer: in, KeepHooks: o.keepHooks}
	workerID := strings.TrimSpace(o.workerID)
	if workerID == "" && !o.service {
		workerID, _ = worker.LookupWorkerID(config.WorkerConfig{Name: o.name})
	}
	var rep worker.UninstallReport
	switch {
	case workerID != "":
		rep, err = u.RemoveIdentity(ctx, workerID, o.hubID)
		if err == nil && !rep.Found && o.workerID == "" && !o.service {
			// No config names this identity: remove a leftover registration of the named
			// instance, unless its config belongs to another identity (a shared home).
			if _, statErr := os.Stat(in.ConfigPath(instance)); statErr != nil {
				err = in.Uninstall(ctx, instance)
			}
		}
	case !o.service:
		if err = in.Uninstall(ctx, instance); err == nil {
			rep.Found = true
			for _, lp := range []string{in.LogPath(instance), in.LogPath(instance) + ".1"} {
				if os.Remove(lp) == nil {
					rep.Removed = append(rep.Removed, lp)
				}
			}
			err = u.RemoveSharedIfLast(&rep)
		}
	default:
		rep.Found = true
		err = u.RemoveSharedIfLast(&rep)
	}
	for _, l := range rep.Lines() {
		fmt.Fprintln(out, l)
	}
	return err
}

func newStatusCmd() *cobra.Command {
	var (
		name    string
		service bool
		start   statusStart
	)
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether konsertd is installed to start automatically, and whether it is running",
		Long: `Show whether konsertd is installed to start automatically, and whether it is running:
the start mechanism's state, or the daemon answering on its local socket.

--start starts an installed instance that is not running (Windows: schtasks /Run) and
waits up to --wait for the daemon before reporting.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return printInstallStatus(cmd.Context(), cmd.OutOrStdout(), name, service, start)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Instance to inspect: the --name given to install")
	cmd.Flags().BoolVar(&service, "service", false, "Windows: inspect the native service instead of the logon task")
	cmd.Flags().BoolVar(&start.on, "start", false, "Start the installed instance if it is not running, and wait for it")
	cmd.Flags().DurationVar(&start.wait, "wait", 20*time.Second, "With --start: how long to wait for the daemon")
	return cmd
}

// statusStart is `konsertd status --start [--wait]`.
type statusStart struct {
	on   bool
	wait time.Duration
}

func printInstallStatus(ctx context.Context, w io.Writer, name string, service bool, start statusStart) error {
	host, _ := os.Hostname()
	instance := install.InstanceFor(name, host)
	env, err := install.DefaultEnv()
	if err != nil {
		return err
	}
	in := &install.Installer{Runner: install.ExecRunner{}, Env: env, Out: w}
	var st install.Status
	switch {
	case service:
		st, err = in.ServiceStatus(ctx, instance)
	case start.on:
		st, err = in.StartAndWait(ctx, instance, install.WaitOptions{
			Wait:  start.wait,
			Alive: func(ctx context.Context) bool { return daemonAnswering(ctx, in.ConfigPath(instance), name) },
		})
	default:
		st, err = in.Status(ctx, instance)
	}
	if err != nil {
		return err
	}
	if !st.Running && daemonAnswering(ctx, st.ConfigPath, name) {
		st.Running = true
		st.Detail = strings.TrimSpace(st.Detail + " (daemon answering)")
	}
	writeStatus(w, st)
	printLinkStatus(w, st.ConfigPath, name)
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
// the --config file's contents (if any), overlaid with every explicitly set flag. Flags
// without a config field are returned as extra run arguments.
func buildInstallConfig(fs *pflag.FlagSet) (config.WorkerConfig, []string, error) {
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
	return cfg, extra, nil
}

// installedRunConfig is the config `run` will load from the persisted data: defaults
// overlaid with it, validated.
func installedRunConfig(data []byte) (config.WorkerConfig, error) {
	full := config.DefaultWorkerConfig()
	if err := yaml.Unmarshal(data, &full); err != nil {
		return full, err
	}
	if full.MaxConcurrent <= 0 {
		full.MaxConcurrent = 2
	}
	if full.Tags == nil {
		full.Tags = map[string]string{}
	}
	if err := full.Validate(); err != nil {
		return full, fmt.Errorf("invalid worker config: %w", err)
	}
	return full, nil
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
	_, _ = w.Write(spec.Config)
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
