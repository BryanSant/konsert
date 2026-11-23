//go:build windows

package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// ServiceSupported reports whether --service is available on this OS.
const ServiceSupported = true

// ServiceOptions configures a native Windows service install.
type ServiceOptions struct {
	// Account is the account the service logs on as, e.g. `.\alice` or `DOMAIN\alice`.
	Account  string
	Password string
}

// ServiceName is the SCM name of an instance.
func ServiceName(instance string) string { return unitBase(instance) }

// serviceDir holds a service's config and logs: %ProgramData%\konsert. The account's
// own profile may not exist yet and is not writable by the installing administrator in
// a way that keeps the account the owner, so the files live here with explicit ACLs.
func serviceDir() string {
	if p, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0); err == nil && p != "" {
		return filepath.Join(p, "konsert")
	}
	return `C:\ProgramData\konsert`
}

func serviceConfigPath(instance string) string {
	return filepath.Join(serviceDir(), unitBase(instance)+".yaml")
}

func serviceLogPath(instance string) string {
	return filepath.Join(serviceDir(), "logs", unitBase(instance)+".log")
}

func serviceMarkerPath(instance string) string {
	return filepath.Join(serviceDir(), unitBase(instance)+".service.json")
}

// serviceMarker records what install changed, so uninstall only undoes its own work.
type serviceMarker struct {
	Account           string `json:"account"`
	AccountSID        string `json:"account_sid"`
	GrantedLogonRight bool   `json:"granted_logon_right"`
}

// IsElevated reports whether this process runs with an elevated (administrator) token.
func IsElevated() bool { return windows.GetCurrentProcessToken().IsElevated() }

// normalizeAccount turns `name` into `.\name` and resolves `.` to the computer name for
// SID lookups. It returns the SCM form and the lookup form.
func normalizeAccount(account string) (scm, lookup string) {
	account = strings.TrimSpace(account)
	if !strings.Contains(account, `\`) && !strings.Contains(account, "@") {
		account = `.\` + account
	}
	lookup = account
	if strings.HasPrefix(account, `.\`) {
		if host, err := windows.ComputerName(); err == nil {
			lookup = host + account[1:]
		}
	}
	return account, lookup
}

// InstallService registers konsertd as a native service running as opts.Account,
// grants that account "Log on as a service", sets restart-on-failure recovery and
// starts it. It needs an elevated prompt.
func (in *Installer) InstallService(ctx context.Context, spec Spec, opts ServiceOptions) error {
	if !IsElevated() {
		return errors.New("installing a Windows service needs administrator rights: run this from an elevated (\"Run as administrator\") terminal, or omit --service to install a logon task as the current user, which needs no elevation")
	}
	if strings.TrimSpace(opts.Account) == "" {
		return errors.New("--service needs --user, the account to run as (e.g. --user .\\alice); agents use that account's CLI logins")
	}
	if opts.Password == "" {
		return errors.New("the service account password is empty")
	}
	scmAccount, lookup := normalizeAccount(opts.Account)
	sid, _, _, err := windows.LookupSID("", lookup)
	if err != nil {
		return fmt.Errorf("look up account %s: %w", lookup, err)
	}
	sidStr := sid.String()

	marker := serviceMarker{Account: scmAccount, AccountSID: sidStr}
	if old, ok := readServiceMarker(spec.Instance); ok && old.AccountSID == sidStr {
		marker.GrantedLogonRight = old.GrantedLogonRight
	}
	granted, err := grantServiceLogonRight(sid)
	if err != nil {
		return fmt.Errorf("grant %s the \"Log on as a service\" right: %w", scmAccount, err)
	}
	if granted {
		marker.GrantedLogonRight = true
		in.printf("Granted %s the \"Log on as a service\" right (SeServiceLogonRight)\n", scmAccount)
	}

	// Config (with the token): SYSTEM/Administrators full, the account read-only.
	cfgPath := serviceConfigPath(spec.Instance)
	if err := writeRestricted(cfgPath, spec.Config, func(p string) error {
		return setProtectedDACL(p, serviceFileSDDL(sidStr))
	}); err != nil {
		return err
	}
	in.printf("Wrote config %s (readable by SYSTEM, Administrators and %s only)\n", cfgPath, scmAccount)
	logPath := serviceLogPath(spec.Instance)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	if err := setProtectedDACL(filepath.Dir(logPath), serviceDirSDDL(sidStr)); err != nil {
		return fmt.Errorf("set ACL on %s: %w", filepath.Dir(logPath), err)
	}
	if err := writeServiceMarker(spec.Instance, marker); err != nil {
		return err
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service manager: %w", err)
	}
	defer func() { _ = m.Disconnect() }()

	name := ServiceName(spec.Instance)
	args := runArgs(cfgPath, spec.ExtraArgs, logPath)
	display := "Konsert worker (konsertd)"
	if spec.Instance != "" {
		display = "Konsert worker (konsertd, " + spec.Instance + ")"
	}
	s, err := m.OpenService(name)
	if err == nil {
		// Reinstall: stop the old one and update it in place.
		_ = stopService(ctx, s, 30*time.Second)
		cfg, cerr := s.Config()
		if cerr != nil {
			_ = s.Close()
			return fmt.Errorf("read service %s: %w", name, cerr)
		}
		cfg.BinaryPathName = WindowsJoinArgs(append([]string{spec.Exe}, args...))
		cfg.ServiceStartName = scmAccount
		cfg.Password = opts.Password
		cfg.DisplayName = display
		cfg.StartType = mgr.StartAutomatic
		cfg.DelayedAutoStart = true
		if err := s.UpdateConfig(cfg); err != nil {
			_ = s.Close()
			return fmt.Errorf("update service %s: %w", name, err)
		}
		in.printf("Updated service %s\n", name)
	} else {
		s, err = m.CreateService(name, spec.Exe, mgr.Config{
			DisplayName:      display,
			Description:      "Runs Konsert agent sessions as " + scmAccount + ". Installed by `konsertd install --service`.",
			StartType:        mgr.StartAutomatic,
			DelayedAutoStart: true,
			ServiceStartName: scmAccount,
			Password:         opts.Password,
		}, args...)
		if err != nil {
			return fmt.Errorf("create service %s: %w", name, err)
		}
		in.printf("Created service %s (runs as %s)\n", name, scmAccount)
	}
	defer func() { _ = s.Close() }()

	// Restart on failure, including a non-zero exit (not just a crash).
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, 24*60*60); err != nil {
		return fmt.Errorf("set recovery actions: %w", err)
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("set recovery on non-crash failures: %w", err)
	}

	if spec.NoStart {
		in.printf("Not started now (--no-start); it starts at boot.\n")
		return nil
	}
	if err := s.Start(); err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_LOGON_FAILED) {
			return fmt.Errorf("start service %s: the service could not log on as %s (wrong password?): %w", name, scmAccount, err)
		}
		return fmt.Errorf("start service %s: %w", name, err)
	}
	if err := waitServiceState(ctx, s, svc.Running, 30*time.Second); err != nil {
		return fmt.Errorf("service %s did not reach Running (see %s): %w", name, logPath, err)
	}
	in.printf("Started service %s; logs: %s\n", name, logPath)
	return nil
}

// UninstallService stops and deletes the service, revokes the logon right if install
// granted it, and removes the service's config.
func (in *Installer) UninstallService(ctx context.Context, instance string) error {
	if !IsElevated() {
		return errors.New("removing a Windows service needs administrator rights: run this from an elevated terminal")
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service manager: %w", err)
	}
	defer func() { _ = m.Disconnect() }()
	name := ServiceName(instance)
	s, err := m.OpenService(name)
	if err != nil {
		in.printf("No service %s\n", name)
	} else {
		if err := stopService(ctx, s, 30*time.Second); err != nil {
			in.printf("Warning: stopping %s: %v\n", name, err)
		}
		if err := s.Delete(); err != nil {
			_ = s.Close()
			return fmt.Errorf("delete service %s: %w", name, err)
		}
		_ = s.Close()
		in.printf("Stopped and deleted service %s\n", name)
	}
	if marker, ok := readServiceMarker(instance); ok && marker.GrantedLogonRight {
		if sid, err := windows.StringToSid(marker.AccountSID); err == nil {
			if err := revokeServiceLogonRight(sid); err != nil {
				in.printf("Warning: could not revoke \"Log on as a service\" from %s: %v\n", marker.Account, err)
			} else {
				in.printf("Revoked \"Log on as a service\" from %s (granted by install)\n", marker.Account)
			}
		}
	}
	for _, p := range []string{serviceConfigPath(instance), serviceMarkerPath(instance)} {
		if removed, err := removeFile(p); err != nil {
			return fmt.Errorf("remove %s: %w", p, err)
		} else if removed {
			in.printf("Removed %s\n", p)
		}
	}
	in.printf("Logs were kept in %s\n", filepath.Dir(serviceLogPath(instance)))
	return nil
}

// ServiceStatus inspects the service.
func (in *Installer) ServiceStatus(_ context.Context, instance string) (Status, error) {
	name := ServiceName(instance)
	st := Status{
		Mechanism:      "Windows service",
		Name:           name,
		DefinitionPath: name,
		ConfigPath:     serviceConfigPath(instance),
		LogPath:        serviceLogPath(instance),
	}
	// SC_MANAGER_CONNECT and query-only service access work without elevation.
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return st, fmt.Errorf("connect to the service manager: %w", err)
	}
	defer func() { _ = windows.CloseServiceHandle(scm) }()
	h, err := windows.OpenService(scm, windows.StringToUTF16Ptr(name), windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return st, nil
		}
		return st, err
	}
	s := &mgr.Service{Name: name, Handle: h}
	defer func() { _ = s.Close() }()
	st.Installed = true
	q, err := s.Query()
	if err != nil {
		return st, err
	}
	st.Running = q.State == svc.Running
	st.PID = int(q.ProcessId)
	st.Detail = serviceStateName(q.State)
	if cfg, err := s.Config(); err == nil {
		st.Detail += ", runs as " + cfg.ServiceStartName
	}
	return st, nil
}

func serviceStateName(s svc.State) string {
	switch s {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "start pending"
	case svc.StopPending:
		return "stop pending"
	case svc.Running:
		return "running"
	case svc.ContinuePending:
		return "continue pending"
	case svc.PausePending:
		return "pause pending"
	case svc.Paused:
		return "paused"
	}
	return fmt.Sprintf("state %d", s)
}

func stopService(ctx context.Context, s *mgr.Service, timeout time.Duration) error {
	q, err := s.Query()
	if err != nil {
		return err
	}
	if q.State == svc.Stopped {
		return nil
	}
	if q.State != svc.StopPending {
		if _, err := s.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return err
		}
	}
	return waitServiceState(ctx, s, svc.Stopped, timeout)
}

func waitServiceState(ctx context.Context, s *mgr.Service, want svc.State, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		q, err := s.Query()
		if err != nil {
			return err
		}
		if q.State == want {
			return nil
		}
		if want == svc.Running && q.State == svc.Stopped {
			return fmt.Errorf("service stopped (exit code %d, service exit code %d)", q.Win32ExitCode, q.ServiceSpecificExitCode)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out in state %s", serviceStateName(q.State))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func readServiceMarker(instance string) (serviceMarker, bool) {
	var m serviceMarker
	data, err := os.ReadFile(serviceMarkerPath(instance))
	if err != nil || json.Unmarshal(data, &m) != nil {
		return m, false
	}
	return m, true
}

func writeServiceMarker(instance string, m serviceMarker) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(serviceMarkerPath(instance), data, 0o644)
}

// LSA (advapi32) account-rights calls; x/sys/windows does not wrap them.
var (
	modadvapi32                   = windows.NewLazySystemDLL("advapi32.dll")
	procLsaOpenPolicy             = modadvapi32.NewProc("LsaOpenPolicy")
	procLsaClose                  = modadvapi32.NewProc("LsaClose")
	procLsaAddAccountRights       = modadvapi32.NewProc("LsaAddAccountRights")
	procLsaRemoveAccountRights    = modadvapi32.NewProc("LsaRemoveAccountRights")
	procLsaEnumerateAccountRights = modadvapi32.NewProc("LsaEnumerateAccountRights")
	procLsaFreeMemory             = modadvapi32.NewProc("LsaFreeMemory")
	procLsaNtStatusToWinError     = modadvapi32.NewProc("LsaNtStatusToWinError")
)

const (
	policyCreateAccount = 0x00000010
	policyLookupNames   = 0x00000800
	seServiceLogonRight = "SeServiceLogonRight"
)

func ntErr(status uintptr) error {
	if status == 0 {
		return nil
	}
	code, _, _ := procLsaNtStatusToWinError.Call(status)
	return syscall.Errno(code)
}

func lsaOpen() (windows.Handle, error) {
	var attrs windows.OBJECT_ATTRIBUTES
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	var h windows.Handle
	r, _, _ := procLsaOpenPolicy.Call(0, uintptr(unsafe.Pointer(&attrs)), policyCreateAccount|policyLookupNames, uintptr(unsafe.Pointer(&h)))
	if err := ntErr(r); err != nil {
		return 0, fmt.Errorf("LsaOpenPolicy: %w", err)
	}
	return h, nil
}

func lsaClose(h windows.Handle) { _, _, _ = procLsaClose.Call(uintptr(h)) }

// hasRight reports whether sid already holds right.
func hasRight(h windows.Handle, sid *windows.SID, right string) bool {
	var rights *windows.NTUnicodeString
	var count uint32
	r, _, _ := procLsaEnumerateAccountRights.Call(uintptr(h), uintptr(unsafe.Pointer(sid)), uintptr(unsafe.Pointer(&rights)), uintptr(unsafe.Pointer(&count)))
	if r != 0 || rights == nil {
		return false
	}
	defer func() { _, _, _ = procLsaFreeMemory.Call(uintptr(unsafe.Pointer(rights))) }()
	for _, u := range unsafe.Slice(rights, count) {
		if strings.EqualFold(u.String(), right) {
			return true
		}
	}
	return false
}

// grantServiceLogonRight grants sid SeServiceLogonRight and reports whether it was newly
// granted (false if the account already had it).
func grantServiceLogonRight(sid *windows.SID) (bool, error) {
	h, err := lsaOpen()
	if err != nil {
		return false, err
	}
	defer lsaClose(h)
	if hasRight(h, sid, seServiceLogonRight) {
		return false, nil
	}
	u, err := windows.NewNTUnicodeString(seServiceLogonRight)
	if err != nil {
		return false, err
	}
	r, _, _ := procLsaAddAccountRights.Call(uintptr(h), uintptr(unsafe.Pointer(sid)), uintptr(unsafe.Pointer(u)), 1)
	if err := ntErr(r); err != nil {
		return false, fmt.Errorf("LsaAddAccountRights: %w", err)
	}
	return true, nil
}

func revokeServiceLogonRight(sid *windows.SID) error {
	h, err := lsaOpen()
	if err != nil {
		return err
	}
	defer lsaClose(h)
	u, err := windows.NewNTUnicodeString(seServiceLogonRight)
	if err != nil {
		return err
	}
	r, _, _ := procLsaRemoveAccountRights.Call(uintptr(h), uintptr(unsafe.Pointer(sid)), 0, uintptr(unsafe.Pointer(u)), 1)
	return ntErr(r)
}

// ReadPassword prompts on the console and reads a line without echoing it.
func ReadPassword(prompt string) (string, error) {
	h := windows.Handle(os.Stdin.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return "", errors.New("stdin is not a console, so the password cannot be prompted for; pipe it in with --password-stdin")
	}
	fmt.Fprint(os.Stderr, prompt)
	if err := windows.SetConsoleMode(h, (mode&^windows.ENABLE_ECHO_INPUT)|windows.ENABLE_LINE_INPUT|windows.ENABLE_PROCESSED_INPUT); err != nil {
		return "", err
	}
	defer func() {
		_ = windows.SetConsoleMode(h, mode)
		fmt.Fprintln(os.Stderr)
	}()
	var buf []byte
	b := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(b)
		if n == 1 {
			if b[0] == '\n' {
				break
			}
			buf = append(buf, b[0])
		}
		if err != nil {
			break
		}
	}
	return strings.TrimRight(string(buf), "\r"), nil
}
