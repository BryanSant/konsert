package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// Machine credentials (KON-RK9YD). Each konsertd holds its own credential, a bearer
// string "kmc_<credential id>_<secret>": the id names the hub's machine_credentials row
// and the secret is 32 random bytes (base64url). The hub stores only the hex SHA-256 of
// the secret and compares hashes in constant time.

const machineCredPrefix = "kmc_"

// MachineSecret is a newly generated machine credential secret.
type MachineSecret struct {
	// Secret is the base64url secret to embed in the credential.
	Secret string
	// Hash is what the hub stores (MachineSecretHash of Secret).
	Hash string
}

// NewMachineSecret generates a 32-byte random secret and its hash.
func NewMachineSecret() (MachineSecret, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return MachineSecret{}, err
	}
	s := base64.RawURLEncoding.EncodeToString(b)
	return MachineSecret{Secret: s, Hash: MachineSecretHash(s)}, nil
}

// MachineSecretHash is the hex SHA-256 of a credential secret.
func MachineSecretHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// FormatMachineCredential builds the bearer string konsertd presents.
func FormatMachineCredential(credID, secret string) string {
	return machineCredPrefix + credID + "_" + secret
}

// ParseMachineCredential splits a bearer string into its credential id and secret.
func ParseMachineCredential(tok string) (credID, secret string, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(tok), machineCredPrefix)
	if !found {
		return "", "", false
	}
	// The id is a canonical UUID (36 chars, no '_'); the secret is base64url, which may
	// itself contain '_', so split after the id rather than at the last '_'.
	if len(rest) < 38 || rest[36] != '_' {
		return "", "", false
	}
	credID, secret = rest[:36], rest[37:]
	if u, err := uuid.Parse(credID); err != nil || u.String() != credID || len(secret) < 32 {
		return "", "", false
	}
	return credID, secret, true
}

// VerifyMachineSecret reports whether secret matches the stored hash, in constant time.
func VerifyMachineSecret(secret, storedHash string) bool {
	return storedHash != "" && constantTimeEquals(MachineSecretHash(secret), storedHash)
}

// BearerToken returns the request's Authorization: Bearer credential ("" if none).
func BearerToken(r *http.Request) string { return extractBearer(r) }

// Join codes: 8 characters of upper-case Crockford base32 (40 bits), shown as
// "ABCD-EFGH". They live 15 minutes, are single use, and the hub stores only their hash
// and rate-limits redemption.

const joinAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// JoinCodeLength is the number of significant characters in a join code.
const JoinCodeLength = 8

// NewJoinCode returns a random join code formatted as "ABCD-EFGH".
func NewJoinCode() (string, error) {
	b := make([]byte, JoinCodeLength)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	out := make([]byte, 0, JoinCodeLength+1)
	for i, v := range b {
		if i == JoinCodeLength/2 {
			out = append(out, '-')
		}
		out = append(out, joinAlphabet[v&31])
	}
	return string(out), nil
}

// NormalizeJoinCode canonicalizes user input: case, separators and the Crockford
// look-alikes (I and L read as 1, O as 0) do not matter. ok is false when the input
// cannot be a join code.
func NormalizeJoinCode(in string) (string, bool) {
	var b strings.Builder
	for _, r := range strings.ToUpper(in) {
		switch {
		case r == '-' || r == ' ' || r == '\t':
			continue
		case r == 'I' || r == 'L':
			r = '1'
		case r == 'O':
			r = '0'
		}
		if !strings.ContainsRune(joinAlphabet, r) {
			return "", false
		}
		b.WriteRune(r)
	}
	if b.Len() != JoinCodeLength {
		return "", false
	}
	return b.String(), true
}

// JoinCodeHash is the hex SHA-256 the hub stores for a normalized join code.
func JoinCodeHash(normalized string) string {
	sum := sha256.Sum256([]byte("konsert-join:" + normalized))
	return hex.EncodeToString(sum[:])
}
