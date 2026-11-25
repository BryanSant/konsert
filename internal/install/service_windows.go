//go:build windows

package install

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
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

	// Config (with the token): SYSTEM/Administrators full, the account read and
	// delete (so it can remove the service without elevation), nobody else.
	cfgPath := serviceConfigPath(spec.Instance)
	if err := writeRestricted(cfgPath, spec.Config, func(p string) error {
		return setProtectedDACL(p, serviceSecretFileSDDL(sidStr))
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

	if err := grantServiceRemoval(s.Handle, sidStr); err != nil {
		return fmt.Errorf("grant %s the right to stop and delete service %s: %w", scmAccount, name, err)
	}
	in.printf("Granted %s stop and delete on service %s, so removing it needs no elevation\n", scmAccount, name)

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

// ErrServiceAccessDenied means this account may not stop or delete the service: it was
// installed for another account (or before install granted the right).
var ErrServiceAccessDenied = errors.New("this account may not remove the service")

// serviceRemovalAccess is what removing a service needs; install grants exactly this
// to the account the service runs as (ServiceAccountRights).
const serviceRemovalAccess = windows.SERVICE_STOP | windows.DELETE | windows.SERVICE_QUERY_STATUS | windows.SERVICE_QUERY_CONFIG

// openServiceForRemoval opens the service with only the rights removal needs, through
// an SCM handle that needs no elevation. It returns nil when there is no such service.
func openServiceForRemoval(name string) (*mgr.Service, func(), error) {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to the service manager: %w", err)
	}
	closeSCM := func() { _ = windows.CloseServiceHandle(scm) }
	h, err := windows.OpenService(scm, windows.StringToUTF16Ptr(name), serviceRemovalAccess)
	if err != nil {
		closeSCM()
		switch {
		case errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST):
			return nil, nil, nil
		case errors.Is(err, windows.ERROR_ACCESS_DENIED):
			return nil, nil, fmt.Errorf("open service %s: %w", name, ErrServiceAccessDenied)
		}
		return nil, nil, fmt.Errorf("open service %s: %w", name, err)
	}
	s := &mgr.Service{Name: name, Handle: h}
	return s, func() { _ = s.Close(); closeSCM() }, nil
}

// UninstallService stops and deletes the service, revokes the logon right if install
// granted it, and removes the service's config. It needs no elevation when run as the
// account the service runs as (install granted it stop and delete); revoking the logon
// right does, so without elevation the right and the marker recording it are kept for
// a later elevated `konsertd uninstall --service`.
func (in *Installer) UninstallService(ctx context.Context, instance string) error {
	return in.removeService(ctx, instance, false)
}

// UninstallServiceSelf removes the service from inside the daemon it runs (close code
// 4001): the service is marked for deletion without stopping it (that would end this
// very process mid-removal); it is deleted when the daemon exits, and a service marked
// for deletion is never started again.
func (in *Installer) UninstallServiceSelf(ctx context.Context, instance string) error {
	return in.removeService(ctx, instance, true)
}

func (in *Installer) removeService(ctx context.Context, instance string, self bool) error {
	name := ServiceName(instance)
	s, closeService, err := openServiceForRemoval(name)
	if err != nil {
		return err
	}
	if s == nil {
		in.printf("No service %s\n", name)
	} else {
		if !self {
			if err := stopService(ctx, s, 30*time.Second); err != nil {
				in.printf("Warning: stopping %s: %v\n", name, err)
			}
		}
		err := s.Delete()
		closeService()
		switch {
		case err == nil:
		case errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE):
		case errors.Is(err, windows.ERROR_ACCESS_DENIED):
			return fmt.Errorf("delete service %s: %w", name, ErrServiceAccessDenied)
		default:
			return fmt.Errorf("delete service %s: %w", name, err)
		}
		if self {
			in.printf("Deleted service %s (it stops when this daemon exits)\n", name)
		} else {
			in.printf("Stopped and deleted service %s\n", name)
		}
	}
	keepMarker := false
	if marker, ok := readServiceMarker(instance); ok && marker.GrantedLogonRight {
		if sid, err := windows.StringToSid(marker.AccountSID); err == nil {
			if err := revokeServiceLogonRight(sid); err != nil {
				keepMarker = true
				in.printf("Kept the \"Log on as a service\" right of %s (granted by install): revoking it needs administrator rights; run `konsertd uninstall --service%s` from an elevated terminal to revoke it (%v)\n", marker.Account, nameFlagHint(instance), err)
			} else {
				in.printf("Revoked \"Log on as a service\" from %s (granted by install)\n", marker.Account)
			}
		}
	}
	paths := []string{serviceConfigPath(instance)}
	if !keepMarker {
		paths = append(paths, serviceMarkerPath(instance))
	}
	for _, p := range paths {
		if removed, err := removeFile(p); err != nil {
			return fmt.Errorf("remove %s: %w", p, err)
		} else if removed {
			in.printf("Removed %s\n", p)
		}
	}
	in.printf("Logs were kept in %s\n", filepath.Dir(serviceLogPath(instance)))
	return nil
}

// nameFlagHint is the --name argument that selects instance in a suggested command.
func nameFlagHint(instance string) string {
	if instance == "" {
		return ""
	}
	return " --name " + instance
}

// ServiceConfigs lists the native-service instances installed for the account running
// this process: the configs in %ProgramData%\konsert whose marker names this account.
// Services of other accounts on the machine are left out.
func (in *Installer) ServiceConfigs() ([]InstalledConfig, error) {
	me, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(serviceDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []InstalledConfig
	for _, e := range entries {
		instance, ok := instanceOfConfig(e.Name())
		if e.IsDir() || !ok {
			continue
		}
		if m, ok := readServiceMarker(instance); !ok || !strings.EqualFold(m.AccountSID, me) {
			continue
		}
		out = append(out, InstalledConfig{Instance: instance, Path: filepath.Join(serviceDir(), e.Name())})
	}
	return out, nil
}

// grantServiceRemoval adds the ACE granting sid ServiceAccountRights to the service's
// DACL, replacing any earlier ACE for sid (see servicesddl.go).
func grantServiceRemoval(h windows.Handle, sid string) error {
	sd, err := windows.GetSecurityInfo(h, windows.SE_SERVICE, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read the service DACL: %w", err)
	}
	sddl, err := serviceDACLWithAccount(sd.String(), sid)
	if err != nil {
		return err
	}
	nsd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return fmt.Errorf("parse %s: %w", sddl, err)
	}
	dacl, _, err := nsd.DACL()
	if err != nil {
		return err
	}
	return windows.SetSecurityInfo(h, windows.SE_SERVICE, windows.DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
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

// writeServiceMarker writes the marker readable and deletable by the account (so an
// unelevated uninstall can remove it) and writable only by administrators.
func writeServiceMarker(instance string, m serviceMarker) error {
	data, err := json.Marshal(m, jsontext.WithIndent("  "))
	if err != nil {
		return err
	}
	return writeRestricted(serviceMarkerPath(instance), data, func(p string) error {
		return setProtectedDACL(p, serviceSecretFileSDDL(m.AccountSID))
	})
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
