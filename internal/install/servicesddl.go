package install

import (
	"fmt"
	"strings"
)

// Removal never needs elevation (SYR-28HHD): the elevated install adds an ACE to the
// service's own DACL granting the account it runs as just enough to remove it, so
// `syred uninstall --service`, the daemon's self-removal on close code 4001 and the
// hub's `syred uninstall --worker` over SSH all work from that account's ordinary
// (non-elevated) token.
//
// Service access rights (winsvc.h) and the SDDL tokens the service DACL uses for them
// (sc sdshow prints them this way; the tokens are the generic SDDL two-letter codes
// whose numeric values coincide with the service rights):
//
//	SERVICE_QUERY_CONFIG   0x00001  CC
//	SERVICE_QUERY_STATUS   0x00004  LC
//	SERVICE_STOP           0x00020  WP
//	DELETE                 0x10000  SD
//
// The account gets CC|LC|WP|SD = 0x10025: query its config and status, stop it and
// delete it. (The default service DACL already lets interactive and service logons
// query and interrogate any service.) Not SERVICE_START (RP),
// SERVICE_CHANGE_CONFIG (DC), WRITE_DAC (WD) or WRITE_OWNER (WO): the account cannot
// repoint the service at another binary, change the account or password, or widen its
// own rights. SERVICE_START is left out too: starting is the SCM's job (automatic
// start and the recovery actions).

// Service access rights granted to the service account.
const (
	serviceQueryConfig = 0x00001
	serviceQueryStatus = 0x00004
	serviceStop        = 0x00020
	accessDelete       = 0x10000

	// ServiceAccountRights is the access mask of the account's ACE on its service.
	ServiceAccountRights = serviceQueryConfig | serviceQueryStatus | serviceStop | accessDelete
	// serviceAccountRightsSDDL spells ServiceAccountRights in SDDL tokens.
	serviceAccountRightsSDDL = "CCLCWPSD"
)

// serviceAccountACE is the allow ACE for sid on the service.
func serviceAccountACE(sid string) string {
	return "(A;;" + serviceAccountRightsSDDL + ";;;" + sid + ")"
}

// serviceDACLWithAccount returns the DACL SDDL (as GetSecurityInfo with
// DACL_SECURITY_INFORMATION prints it, "D:<flags>(ace)(ace)...") with every existing
// ACE for sid removed and serviceAccountACE(sid) appended. Replacing rather than
// adding keeps a reinstall idempotent. The new allow ACE goes last, which keeps the
// DACL canonical (explicit deny ACEs before allow ACEs).
func serviceDACLWithAccount(sddl, sid string) (string, error) {
	sddl = strings.TrimSpace(sddl)
	if sid == "" || strings.ContainsAny(sid, "();") {
		return "", fmt.Errorf("invalid SID %q", sid)
	}
	start := strings.Index(sddl, "D:")
	if start < 0 {
		return "", fmt.Errorf("no DACL in security descriptor %q", sddl)
	}
	rest := sddl[start+2:]
	// The DACL ends where the SACL ("S:") begins, outside any ACE.
	end := len(rest)
	depth := 0
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '(':
			depth++
		case ')':
			depth--
		case 'S', 'O', 'G':
			if depth == 0 && i+1 < len(rest) && rest[i+1] == ':' {
				end = i
			}
		}
		if end != len(rest) {
			break
		}
	}
	tail := rest[end:]
	rest = rest[:end]
	flagsEnd := strings.IndexByte(rest, '(')
	if flagsEnd < 0 {
		flagsEnd = len(rest)
	}
	var b strings.Builder
	b.WriteString(sddl[:start])
	b.WriteString("D:")
	b.WriteString(rest[:flagsEnd])
	aces := rest[flagsEnd:]
	for len(aces) > 0 {
		if aces[0] != '(' {
			return "", fmt.Errorf("malformed DACL %q", sddl)
		}
		closeAt := strings.IndexByte(aces, ')')
		if closeAt < 0 {
			return "", fmt.Errorf("malformed DACL %q", sddl)
		}
		ace := aces[:closeAt+1]
		aces = aces[closeAt+1:]
		fields := strings.Split(ace[1:len(ace)-1], ";")
		if len(fields) >= 6 && strings.EqualFold(fields[5], sid) {
			continue
		}
		b.WriteString(ace)
	}
	b.WriteString(serviceAccountACE(sid))
	b.WriteString(tail)
	return b.String(), nil
}

// serviceSecretFileSDDL is the DACL of the service's config and marker files in
// %ProgramData%\syre: SYSTEM and Administrators full control, the service account
// read (FILE_GENERIC_READ, 0x120089) and DELETE (0x10000) = 0x130089, so the account
// can remove them when it uninstalls the service without elevation. It cannot write
// them.
func serviceSecretFileSDDL(account string) string {
	return "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x130089;;;" + account + ")"
}
