package worker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BryanSant/konsert/internal/install"
	"github.com/google/uuid"
)

// A machine's hub credential (KON-RK9YD) lives in konsertd's state directory, next to
// the identity it belongs to: <stateDir>/credentials/<worker id>, mode 0600. Keying it
// by the worker id means a reset state dir (new identity) never reuses a credential the
// hub bound to the old one, and `konsertd install`/`uninstall` never touch it.

// ErrNoCredential is returned by LoadCredential when this machine has none saved.
var ErrNoCredential = errors.New("no machine credential")

// CredentialPath is where the credential for workerID is kept ("" if there is no state
// directory or the id is not a UUID).
func CredentialPath(stateDir, workerID string) string {
	if stateDir == "" {
		stateDir = defaultStateDir()
	}
	if stateDir == "" {
		return ""
	}
	if u, err := uuid.Parse(workerID); err != nil || u.String() != workerID {
		// Test daemons use free-form ids; keep them out of path joins all the same.
		if workerID == "" || strings.ContainsAny(workerID, `/\.:`) {
			return ""
		}
	}
	return filepath.Join(stateDir, "credentials", workerID)
}

// LoadCredential reads the saved credential for workerID.
func LoadCredential(stateDir, workerID string) (string, error) {
	path := CredentialPath(stateDir, workerID)
	if path == "" {
		return "", ErrNoCredential
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNoCredential
	}
	if err != nil {
		return "", err
	}
	cred := strings.TrimSpace(string(b))
	if cred == "" {
		return "", ErrNoCredential
	}
	return cred, nil
}

// SaveCredential atomically writes cred as workerID's credential, readable only by the
// current user (0600 in a 0700 directory; a user-only ACL on Windows).
func SaveCredential(stateDir, workerID, cred string) error {
	cred = strings.TrimSpace(cred)
	if cred == "" || strings.ContainsAny(cred, " \t\r\n") {
		return errors.New("invalid machine credential")
	}
	path := CredentialPath(stateDir, workerID)
	if path == "" {
		return fmt.Errorf("no state directory to keep the credential for %q in", workerID)
	}
	return install.WriteSecretFile(path, []byte(cred+"\n"))
}
