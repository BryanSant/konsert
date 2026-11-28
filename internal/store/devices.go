package store

import "time"

// Device kinds (SYR-W0P8B).
const (
	DeviceKindBrowser = "browser"
	DeviceKindPhone   = "phone"
	DeviceKindCLI     = "cli"
	DeviceKindMCP     = "mcp"
)

// ValidDeviceKind reports whether k is a known device kind.
func ValidDeviceKind(k string) bool {
	switch k {
	case DeviceKindBrowser, DeviceKindPhone, DeviceKindCLI, DeviceKindMCP:
		return true
	}
	return false
}

// Device is one signed-in client with its own revocable credential. Only the SHA-256
// of the credential is stored (SecretHash), never the credential itself.
type Device struct {
	ID            string     `json:"id"`
	SID           string     `json:"sid"`
	Name          string     `json:"name"`
	Kind          string     `json:"kind"`
	SecretHash    string     `json:"-"`
	CreatedAt     time.Time  `json:"created_at"`
	CreatedBy     string     `json:"created_by,omitempty"`
	LastUsedAt    time.Time  `json:"last_used_at"`
	LastIP        string     `json:"last_ip,omitempty"`
	LastUserAgent string     `json:"last_user_agent,omitempty"`
	ExpiresAt     time.Time  `json:"expires_at"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
}

// Pairing code purposes.
const (
	// PairingPurposePair signs in the browser that redeems the code (/pair/{code}).
	PairingPurposePair = "pair"
	// PairingPurposeLogin is the CLI device-code flow: a signed-in browser approves the
	// code and the polling CLI receives the credential.
	PairingPurposeLogin = "login"
	// PairingPurposeLaunch signs in a browser on the hub's own machine for `syre open`:
	// short-lived, and only redeemable over loopback.
	PairingPurposeLaunch = "launch"
)

// PairingCode is a single-use, short-lived sign-in code. Only hashes are stored.
type PairingCode struct {
	ID         string
	SID        string
	Purpose    string
	CodeHash   string
	PollHash   string
	Kind       string
	DeviceName string
	CreatedBy  string
	CreatedIP  string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	ApprovedAt *time.Time
	ApprovedBy string
	DeniedAt   *time.Time
	RedeemedAt *time.Time
	DeviceID   string
}
