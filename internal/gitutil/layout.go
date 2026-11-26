package gitutil

import "path/filepath"

const (
	// MetaDir is the dot directory under a workdir root that holds Syre-managed
	// ticket worktrees and clones. Names starting with "." are skipped by checkout
	// discovery, so this tree is not reported as a checkout.
	MetaDir = ".syre"
	// TicketsDir is MetaDir's subdirectory of per-ticket workdirs.
	TicketsDir = "tickets"
	// ClonesDir is MetaDir's subdirectory of bare clones used when a binding has
	// no local checkout.
	ClonesDir = "clones"
)

// TicketWorkdir is <root>/.syre/tickets/<ticketSID>, the parent of that ticket's
// per-repo worktrees.
func TicketWorkdir(root, ticketSID string) string {
	return filepath.Join(root, MetaDir, TicketsDir, ticketSID)
}

// CloneDir is <root>/.syre/clones/<repoName>.
func CloneDir(root, repoName string) string {
	return filepath.Join(root, MetaDir, ClonesDir, repoName)
}
