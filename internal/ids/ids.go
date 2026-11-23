// Package ids mints Konsert's identifiers.
//
// Two kinds exist:
//
//   - User-facing records get short, typed, ka-style IDs: a one- or two-letter kind prefix
//     plus random lower-case Crockford base32, so the kind is clear wherever an ID shows up
//     alone (logs, URLs): t-7qk3m task, c-yvp2f chat, m-… machine, p-… project, r-… repo,
//     s-… space, ws-… workspace, wt-… worktree. The hub checks each candidate against the
//     database, so a short suffix is safe; it grows by one character when collisions get
//     frequent. Machines propose their own m- ID; the hub re-mints it on a clash.
//   - Everything that must be a UUID (the hidden machine identity key, protocol request and
//     turn IDs, agent session IDs that CLIs require to be UUIDs) uses UUIDv7: time-ordered,
//     so they sort by creation and index well.
package ids

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// Kind is the typed prefix of a user-facing ID.
type Kind string

const (
	Task      Kind = "t"
	Chat      Kind = "c"
	Machine   Kind = "m"
	Project   Kind = "p"
	Repo      Kind = "r"
	Space     Kind = "s"
	Workspace Kind = "ws"
	Worktree  Kind = "wt"
)

// Kinds lists every user-facing kind.
var Kinds = []Kind{Task, Chat, Machine, Project, Repo, Space, Workspace, Worktree}

// Alphabet is lower-case Crockford base32: digits and letters without i, l, o, u.
// IDs are lower case because they appear in URLs (https://…/chats/c-yvp2f).
const Alphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// SuffixLength is the starting length of the random part.
const SuffixLength = 5

// maxTriesPerLength is how many collisions are tolerated before the suffix grows.
const maxTriesPerLength = 8

var idRe = regexp.MustCompile(`^([a-z]{1,2})-([0-9a-hjkmnp-tv-z]{5,12})$`)

// Default IDs of the built-in workspace and project. All-zero suffixes are never minted
// (see lowEntropy), so they cannot collide.
const (
	DefaultWorkspace = string(Workspace) + "-00000"
	DefaultProject   = string(Project) + "-00000"
)

// UUID returns a new UUIDv7 string.
func UUID() string { return uuid.Must(uuid.NewV7()).String() }

// IsUUIDv7 reports whether s is a canonical UUIDv7 string.
func IsUUIDv7(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u.Version() == 7 && u.String() == s
}

// Taken reports whether an ID is already in use.
type Taken func(id string) (bool, error)

// New mints a unique ID of kind k. taken is consulted for every candidate; nil means no check
// (only for tests and throwaway IDs).
func New(k Kind, taken Taken) (string, error) {
	for n := SuffixLength; n <= 12; n++ {
		for range maxTriesPerLength {
			sfx := random(n)
			if lowEntropy(sfx) {
				continue
			}
			id := string(k) + "-" + sfx
			if taken == nil {
				return id, nil
			}
			used, err := taken(id)
			if err != nil {
				return "", err
			}
			if !used {
				return id, nil
			}
		}
	}
	return "", fmt.Errorf("ids: could not mint a unique %s id", k)
}

// Parse splits a user-facing ID into its kind and suffix.
func Parse(id string) (Kind, string, bool) {
	m := idRe.FindStringSubmatch(id)
	if m == nil {
		return "", "", false
	}
	for _, k := range Kinds {
		if string(k) == m[1] {
			return k, m[2], true
		}
	}
	return "", "", false
}

// Valid reports whether id is a well-formed ID of kind k.
func Valid(k Kind, id string) bool {
	got, _, ok := Parse(id)
	return ok && got == k
}

// Hash derives a stable ID of kind k from seed (for identities that must not change but
// cannot be persisted, e.g. a worker on a read-only filesystem).
func Hash(k Kind, seed string) string {
	sum := sha256.Sum256([]byte(seed))
	out := make([]byte, SuffixLength)
	for i := range out {
		out[i] = Alphabet[int(sum[i])%32]
	}
	return string(k) + "-" + string(out)
}

func random(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	for i, c := range buf {
		buf[i] = Alphabet[int(c)%32] // 256 is a multiple of 32: no modulo bias
	}
	return string(buf)
}

// lowEntropy rejects suffixes that look hand-made: fewer than three distinct characters
// (this also reserves the all-zero defaults) or a straight run through the alphabet.
func lowEntropy(s string) bool {
	distinct := map[byte]bool{}
	for i := 0; i < len(s); i++ {
		distinct[s[i]] = true
	}
	if len(distinct) < 3 {
		return true
	}
	up, down := true, true
	for i := 1; i < len(s); i++ {
		a, b := strings.IndexByte(Alphabet, s[i-1]), strings.IndexByte(Alphabet, s[i])
		up = up && b == a+1
		down = down && b == a-1
	}
	return up || down
}
