// Package ids mints Konsert's identifiers.
//
// Two kinds exist:
//
//   - User-facing records (tasks, chats, workspaces, projects, repos, worktrees, spaces) get
//     short, typed, ka-style IDs: a kind prefix plus random Crockford base32, e.g. TASK-7QK3M.
//     The hub is the only minter and checks each candidate against the database, so a short
//     suffix is safe; it grows by one character when collisions get frequent.
//   - Everything that must be a UUID (worker/machine identity minted offline, protocol request
//     and turn IDs, agent session IDs that CLIs require to be UUIDs) uses UUIDv7: time-ordered,
//     so they sort by creation and index well.
package ids

import (
	"crypto/rand"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// Kind is the typed prefix of a user-facing ID.
type Kind string

const (
	Task      Kind = "TASK"
	Chat      Kind = "CHAT"
	Workspace Kind = "WS"
	Project   Kind = "PRJ"
	Repo      Kind = "REPO"
	Worktree  Kind = "WT"
	Space     Kind = "SPC"
)

// Kinds lists every user-facing kind.
var Kinds = []Kind{Task, Chat, Workspace, Project, Repo, Worktree, Space}

// Alphabet is Crockford base32: digits and upper-case letters without I, L, O, U.
const Alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// SuffixLength is the starting length of the random part.
const SuffixLength = 5

// maxTriesPerLength is how many collisions are tolerated before the suffix grows.
const maxTriesPerLength = 8

var idRe = regexp.MustCompile(`^([A-Z]{2,4})-([0-9A-HJKMNP-TV-Z]{5,12})$`)

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
