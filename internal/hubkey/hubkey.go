// Package hubkey manages the hub's dedicated SSH key (SYR-K1B60) and its lines in a
// machine's authorized_keys.
//
// Machines added over SSH talk to the hub over SSH: the hub runs the system ssh with its
// own ed25519 key (never a user's personal key), and the machine's authorized_keys
// restricts that key to a forced command, so all it can do is attach to syred:
//
//	command="/home/me/.local/bin/syred attach --allow=<id>",no-pty,no-agent-forwarding,no-port-forwarding,no-X11-forwarding ssh-ed25519 AAAA... syre-hub:<hub-id>
//
// One line per hub and account: the comment marks it as this hub's, and --allow lists
// the syred identities (worker ids) on the account the hub may attach to, so two
// machines sharing a home directory (a container and its host) share the line, and
// removing one only drops its id. The line goes when the last id does.
package hubkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/BryanSant/syre/internal/install"
	"github.com/google/uuid"
)

// MarkerPrefix starts the comment of every authorized_keys line a hub manages.
const MarkerPrefix = "syre-hub:"

// Options are the restrictions on the hub key besides its forced command.
const Options = "no-pty,no-agent-forwarding,no-port-forwarding,no-X11-forwarding"

// Marker is the authorized_keys comment identifying hubID's line.
func Marker(hubID string) string { return MarkerPrefix + hubID }

// Entry is one hub key grant: the hub, its public key ("ssh-ed25519 AAAA..."), the
// syred binary the forced command runs, the remote OS (quoting differs) and the
// syred identity to allow.
type Entry struct {
	HubID     string
	PublicKey string
	Exe       string
	GOOS      string
	WorkerID  string
}

var hubIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidHubID reports whether id is usable as a hub id (it ends up in authorized_keys).
func ValidHubID(id string) bool { return hubIDRe.MatchString(id) }

// ValidWorkerID reports whether id is a canonical UUID (syred's identity).
func ValidWorkerID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u.String() == id
}

func (e Entry) validate() error {
	if !ValidHubID(e.HubID) {
		return fmt.Errorf("invalid hub id %q", e.HubID)
	}
	if !ValidWorkerID(e.WorkerID) {
		return fmt.Errorf("invalid worker id %q", e.WorkerID)
	}
	f := strings.Fields(e.PublicKey)
	if len(f) < 2 || f[0] != "ssh-ed25519" {
		return errors.New("the hub key must be an ssh-ed25519 public key")
	}
	if _, err := base64.StdEncoding.DecodeString(f[1]); err != nil {
		return errors.New("the hub public key is not valid base64")
	}
	if strings.TrimSpace(e.Exe) == "" || strings.ContainsAny(e.Exe, "\"\r\n") {
		return fmt.Errorf("invalid syred path %q", e.Exe)
	}
	return nil
}

// ForcedCommand is the command the hub key may run: `<exe> attach --allow=<ids>`, with
// exe quoted for the remote shell when it has spaces (sh on POSIX, cmd.exe on Windows).
func ForcedCommand(goos, exe string, workerIDs []string) string {
	q := exe
	if strings.ContainsAny(exe, " \t'&()^%!") {
		if goos == "windows" {
			q = `"` + exe + `"`
		} else {
			q = "'" + strings.ReplaceAll(exe, "'", `'\''`) + "'"
		}
	}
	return q + " attach --allow=" + strings.Join(workerIDs, ",")
}

// Line renders a complete authorized_keys line.
func Line(e Entry, workerIDs []string) string {
	f := strings.Fields(e.PublicKey)
	cmd := strings.ReplaceAll(ForcedCommand(e.GOOS, e.Exe, workerIDs), `"`, `\"`)
	return `command="` + cmd + `",` + Options + " " + f[0] + " " + f[1] + " " + Marker(e.HubID)
}

var allowRe = regexp.MustCompile(`--allow=([0-9A-Fa-f,-]*)`)

// isHubLine reports whether line is hubID's managed line (its last field is the marker).
func isHubLine(line, hubID string) bool {
	f := strings.Fields(line)
	return len(f) > 0 && f[len(f)-1] == Marker(hubID)
}

// allowedIDs parses the --allow list of a managed line.
func allowedIDs(line string) []string {
	m := allowRe.FindStringSubmatch(line)
	if m == nil {
		return nil
	}
	var out []string
	for _, id := range strings.Split(m[1], ",") {
		if id = strings.ToLower(strings.TrimSpace(id)); ValidWorkerID(id) {
			out = append(out, id)
		}
	}
	return out
}

// splitLines splits an authorized_keys file into lines that keep their own terminator
// ("\n", "\r\n", or none on an unterminated last line), so lines the hub does not
// manage are written back byte for byte, whatever mix of endings the file has.
func splitLines(content string) []string {
	lines := strings.SplitAfter(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// body is a line without its terminator.
func body(l string) string { return strings.TrimRight(l, "\r\n") }

// terminator is l's line ending ("" if none).
func terminator(l string) string { return l[len(body(l)):] }

// fileEOL is the ending a new line gets: the last terminated line's, else "\n".
func fileEOL(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if t := terminator(lines[i]); t != "" {
			return t
		}
	}
	return "\n"
}

// Add returns content with e's worker allowed on hubID's line: the line is created, or
// rewritten with the new key and binary and the union of allowed ids. Every other line
// is kept byte for byte. Adding an id that is already allowed with the same key and
// binary returns content unchanged.
func Add(content string, e Entry) (string, error) {
	if err := e.validate(); err != nil {
		return "", err
	}
	lines := splitLines(content)
	eol := fileEOL(lines)
	ids := []string{strings.ToLower(e.WorkerID)}
	kept := make([]string, 0, len(lines)+1)
	insertAt, insertEOL := -1, eol
	for _, l := range lines {
		if isHubLine(body(l), e.HubID) {
			ids = append(ids, allowedIDs(l)...)
			if insertAt < 0 {
				insertAt = len(kept)
				if t := terminator(l); t != "" {
					insertEOL = t
				}
			}
			continue
		}
		kept = append(kept, l)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	line := Line(e, ids) + insertEOL
	if insertAt < 0 {
		if n := len(kept); n > 0 && terminator(kept[n-1]) == "" {
			kept[n-1] += eol
		}
		kept = append(kept, line)
	} else {
		kept = slices.Insert(kept, insertAt, line)
	}
	return strings.Join(kept, ""), nil
}

// Remove drops workerID from hubID's line, and the line itself once no id is left (an
// empty workerID drops the line outright). It reports whether anything changed; other
// lines are never touched.
func Remove(content, hubID, workerID string) (string, bool) {
	if !ValidHubID(hubID) {
		return content, false
	}
	workerID = strings.ToLower(strings.TrimSpace(workerID))
	lines := splitLines(content)
	out := make([]string, 0, len(lines))
	changed := false
	for _, l := range lines {
		if !isHubLine(body(l), hubID) {
			out = append(out, l)
			continue
		}
		ids := allowedIDs(l)
		rest := slices.DeleteFunc(slices.Clone(ids), func(id string) bool { return workerID == "" || id == workerID })
		if len(rest) == len(ids) && workerID != "" {
			out = append(out, l)
			continue
		}
		changed = true
		if len(rest) == 0 {
			continue
		}
		out = append(out, allowRe.ReplaceAllLiteralString(l, "--allow="+strings.Join(rest, ",")))
	}
	if !changed {
		return content, false
	}
	return strings.Join(out, ""), true
}

// Allowed lists the worker ids hubID's line in content allows.
func Allowed(content, hubID string) []string {
	var ids []string
	for _, l := range splitLines(content) {
		if isHubLine(body(l), hubID) {
			ids = append(ids, allowedIDs(l)...)
		}
	}
	return ids
}

// AdminsSID is the well-known SID of the local Administrators group.
const AdminsSID = "S-1-5-32-544"

// AuthorizedKeysPath is the file sshd reads the user's keys from: ~/.ssh/authorized_keys,
// except for members of Administrators on Windows, whose keys the default
// sshd_config (Match Group administrators) reads from
// %ProgramData%\ssh\administrators_authorized_keys instead.
func AuthorizedKeysPath(goos string, isAdmin bool, home, programData string) string {
	if goos == "windows" {
		if isAdmin {
			if programData == "" {
				programData = `C:\ProgramData`
			}
			return strings.TrimRight(programData, `\/`) + `\ssh\administrators_authorized_keys`
		}
		return strings.TrimRight(home, `\/`) + `\.ssh\authorized_keys`
	}
	return strings.TrimRight(home, "/") + "/.ssh/authorized_keys"
}

// AdminKeysSDDL is the DACL sshd requires on administrators_authorized_keys: only
// SYSTEM and Administrators, not inherited.
const AdminKeysSDDL = "D:P(A;;FA;;;SY)(A;;FA;;;BA)"

// --- the hub's key pair ---------------------------------------------------------

// Generate creates an ed25519 key pair: the OpenSSH private key (PEM) and the public
// key in authorized_keys form with comment.
func Generate(comment string) (privPEM []byte, pub string, err error) {
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	return marshalPrivate(pk, sk, comment), publicLine(pk, comment), nil
}

func sshString(b []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(b)))
	return append(out, b...)
}

func publicBlob(pk ed25519.PublicKey) []byte {
	return append(sshString([]byte("ssh-ed25519")), sshString(pk)...)
}

func publicLine(pk ed25519.PublicKey, comment string) string {
	s := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(publicBlob(pk))
	if comment != "" {
		s += " " + comment
	}
	return s
}

// marshalPrivate encodes an unencrypted openssh-key-v1 private key.
func marshalPrivate(pk ed25519.PublicKey, sk ed25519.PrivateKey, comment string) []byte {
	var check [4]byte
	_, _ = rand.Read(check[:])
	var priv []byte
	priv = append(priv, check[:]...)
	priv = append(priv, check[:]...)
	priv = append(priv, sshString([]byte("ssh-ed25519"))...)
	priv = append(priv, sshString(pk)...)
	priv = append(priv, sshString(sk)...)
	priv = append(priv, sshString([]byte(comment))...)
	for i := byte(1); len(priv)%8 != 0; i++ {
		priv = append(priv, i)
	}
	var b []byte
	b = append(b, "openssh-key-v1\x00"...)
	b = append(b, sshString([]byte("none"))...)
	b = append(b, sshString([]byte("none"))...)
	b = append(b, sshString(nil)...)
	b = binary.BigEndian.AppendUint32(b, 1)
	b = append(b, sshString(publicBlob(pk))...)
	b = append(b, sshString(priv)...)
	return pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: b})
}

// LoadOrCreate returns the public key of the hub key at path, generating the pair
// (private key readable only by this user, public key at path+".pub") the first time.
func LoadOrCreate(path, comment string) (string, error) {
	if pub, err := os.ReadFile(path + ".pub"); err == nil {
		if _, err := os.Stat(path); err == nil {
			if s := strings.TrimSpace(string(pub)); strings.HasPrefix(s, "ssh-ed25519 ") {
				return s, nil
			}
		}
	}
	priv, pub, err := Generate(comment)
	if err != nil {
		return "", err
	}
	if err := install.WriteSecretFile(path, priv); err != nil {
		return "", fmt.Errorf("write the hub key: %w", err)
	}
	if err := os.WriteFile(path+".pub", []byte(pub+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("write the hub public key: %w", err)
	}
	return pub, nil
}
