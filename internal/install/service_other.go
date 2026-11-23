//go:build !windows

package install

import (
	"context"
	"errors"
)

// ServiceSupported reports whether --service is available on this OS.
const ServiceSupported = false

var errServiceWindowsOnly = errors.New("--service is only available on Windows; on Linux and macOS `konsertd install` already runs the daemon as you, at boot (systemd with lingering) or login (launchd)")

// ServiceOptions configures a native Windows service install.
type ServiceOptions struct {
	Account  string
	Password string
}

// IsElevated is only meaningful on Windows.
func IsElevated() bool { return false }

// InstallService is Windows-only.
func (in *Installer) InstallService(context.Context, Spec, ServiceOptions) error {
	return errServiceWindowsOnly
}

// UninstallService is Windows-only.
func (in *Installer) UninstallService(context.Context, string) error {
	return errServiceWindowsOnly
}

// ServiceStatus is Windows-only.
func (in *Installer) ServiceStatus(context.Context, string) (Status, error) {
	return Status{}, errServiceWindowsOnly
}

// ReadPassword is Windows-only.
func ReadPassword(string) (string, error) { return "", errServiceWindowsOnly }

// RunAsServiceIfNeeded never handles anything outside Windows.
func RunAsServiceIfNeeded(func(context.Context) error) (bool, error) { return false, nil }

// StopWithHeadlessHost only watches a conhost parent on Windows.
func StopWithHeadlessHost(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(ctx)
}
