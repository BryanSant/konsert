package shells

import (
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// maxCommand caps a cleaned command line (runes).
const maxCommand = 300

// CleanCommand turns a shell's argv into the command a human would recognise: the script
// of `sh -c …` / `bash -lc …` / `pwsh -Command …` / `cmd /c …`, with the wrappers agents
// add around it removed (claude sources a shell snapshot, sets options, runs the command
// through eval '…' and records the working directory afterwards). Anything else is the
// argv joined. Capped at maxCommand runes.
func CleanCommand(argv []string) string {
	for len(argv) > 0 && argv[len(argv)-1] == "" {
		argv = argv[:len(argv)-1]
	}
	if len(argv) == 0 {
		return ""
	}
	cmd := strings.Join(argv, " ")
	if IsShell(argv[0]) {
		if script, ok := shellScript(argv); ok {
			cmd = cleanScript(script)
		}
	} else {
		args := append([]string{filepath.Base(argv[0])}, argv[1:]...)
		cmd = strings.Join(args, " ")
	}
	cmd = strings.Join(strings.Fields(cmd), " ")
	if utf8.RuneCountInString(cmd) > maxCommand {
		r := []rune(cmd)
		cmd = string(r[:maxCommand-1]) + "…"
	}
	return cmd
}

// shellScript returns the script a shell runs with -c (or the PowerShell / cmd.exe
// equivalents).
func shellScript(argv []string) (string, bool) {
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		la := strings.ToLower(a)
		switch {
		case la == "-c" || la == "/c" || la == "/k" || la == "-command" || la == "-commandwithargs":
			rest := argv[i+1:]
			// sh -c 'script' name args…: only the script matters; cmd /c and pwsh
			// -Command join every remaining word.
			if strings.HasPrefix(la, "/") || strings.HasPrefix(la, "-command") {
				return strings.Join(rest, " "), len(rest) > 0
			}
			// Options may follow -c (claude runs `zsh -c -l 'script'`).
			for len(rest) > 0 && (strings.HasPrefix(rest[0], "-") || strings.HasPrefix(rest[0], "+")) {
				done := rest[0] == "--"
				rest = rest[1:]
				if done {
					break
				}
			}
			if len(rest) > 0 {
				return rest[0], true
			}
			return "", false
		case strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "c") && len(a) <= 5:
			// Combined flags: -lc, -ic, -lic.
			if i+1 < len(argv) {
				return argv[i+1], true
			}
		}
	}
	return "", false
}

// cleanScript removes agent wrapper steps from a shell script. claude runs every Bash call
// as
//
//	source …/shell-snapshots/snapshot-zsh-….sh 2>/dev/null || true && \builtin setopt … ||
//	true && { \builtin unalias -- 'which'; … } >/dev/null 2>&1 || true && eval 'CMD' < /dev/null
//	&& pwd -P >| /tmp/claude-…-cwd
//
// and writes its shell snapshot with a script starting SNAPSHOT_FILE=….
func cleanScript(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "SNAPSHOT_FILE=") {
		return "claude: shell snapshot"
	}
	parts := splitAnd(s)
	var keep []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		bare := strings.TrimPrefix(strings.TrimPrefix(p, `\`), "builtin ")
		switch {
		case p == "":
			continue
		case (strings.HasPrefix(p, "source ") || strings.HasPrefix(p, ". ")) && strings.Contains(p, "snapshot"):
			continue // claude's shell snapshot
		case strings.HasPrefix(bare, "setopt ") || strings.HasPrefix(bare, "shopt ") || strings.HasPrefix(bare, "unsetopt "):
			continue
		case strings.HasPrefix(p, "{") && strings.Contains(p, "unalias"):
			continue // claude drops aliases that would shadow builtins
		case strings.HasPrefix(p, "pwd -P >") || strings.HasPrefix(p, "pwd >"):
			continue // the agent records the working directory
		case strings.HasPrefix(p, "eval "):
			p = unquoteEval(strings.TrimSpace(p[len("eval "):]))
		}
		keep = append(keep, p)
	}
	if len(keep) == 0 {
		return s
	}
	return strings.Join(keep, " && ")
}

// splitAnd splits a script at top-level " && " (outside quotes).
func splitAnd(s string) []string {
	var parts []string
	var quote rune
	start := 0
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote != 0:
			if r == '\\' && quote == '"' {
				i++
			} else if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '\\':
			i++
		case r == '&' && i+1 < len(rs) && rs[i+1] == '&':
			parts = append(parts, string(rs[start:i]))
			i++
			start = i + 1
		}
	}
	return append(parts, string(rs[start:]))
}

// unquoteEval turns eval's argument ('cmd' with '\” escapes, then redirections claude
// adds such as \< /dev/null) back into the command.
func unquoteEval(s string) string {
	if !strings.HasPrefix(s, "'") && !strings.HasPrefix(s, "\"") {
		return s
	}
	var b strings.Builder
	rs := []rune(s)
	i := 0
	for i < len(rs) {
		switch rs[i] {
		case '\'':
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				j++
			}
			b.WriteString(string(rs[i+1 : min(j, len(rs))]))
			i = j + 1
		case '"':
			j := i + 1
			for j < len(rs) && rs[j] != '"' {
				if rs[j] == '\\' && j+1 < len(rs) {
					j++
				}
				j++
			}
			b.WriteString(strings.ReplaceAll(string(rs[i+1:min(j, len(rs))]), `\"`, `"`))
			i = j + 1
		case '\\':
			if i+1 < len(rs) && rs[i+1] == '\'' {
				b.WriteRune('\'')
				i += 2
				continue
			}
			i++
		default:
			// Unquoted text after the quoted command: claude's "\< /dev/null".
			rest := strings.TrimSpace(string(rs[i:]))
			rest = strings.TrimPrefix(rest, `\`)
			if rest != "< /dev/null" && rest != "</dev/null" {
				b.WriteString(" " + rest)
			}
			i = len(rs)
		}
	}
	return strings.TrimSpace(b.String())
}
