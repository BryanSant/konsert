package sshutil

import (
	"encoding/base64"
	"fmt"
	"strings"
	"unicode/utf16"
)

// remoteShell builds the remote commands bootstrap runs on one kind of host. POSIX hosts
// get sh command lines; Windows hosts (whose OpenSSH lands in PowerShell or cmd.exe) get
// `powershell -EncodedCommand`, which both shells run verbatim, with no quoting to get wrong.
type remoteShell interface {
	// findKonsertd prints "found:<path>" or "missing:<path to install to>".
	findKonsertd() string
	// prepareInstall creates the directory the binary is copied to.
	prepareInstall(exePath string) string
	// scpPath is the scp destination path for exePath.
	scpPath(exePath string) string
	// finishCopy runs after scp (chmod on POSIX).
	finishCopy(exePath string) string
	// konsertd runs exe with args. With credOnStdin, the first stdin line reaches
	// konsertd's stdin (for --credential-stdin; never argv). A failure's output goes to
	// stderr, so it ends up in the returned error.
	konsertd(exe string, args []string, credOnStdin bool) string
}

// shQuote quotes s for POSIX sh.
func shQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+:@%=,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// psQuote quotes s as a PowerShell single-quoted (verbatim) string.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// posixShell targets Linux and macOS. konsertd is taken from PATH, else ~/.local/bin.
type posixShell struct{}

const posixUserBin = "$HOME/.local/bin/konsertd"

func (posixShell) findKonsertd() string {
	return `if P=$(command -v konsertd); then echo "found:$P"; elif [ -x "` + posixUserBin + `" ]; then echo "found:` + posixUserBin + `"; else echo "missing:` + posixUserBin + `"; fi`
}

func (posixShell) prepareInstall(string) string { return `mkdir -p "$HOME/.local/bin"` }

func (posixShell) scpPath(string) string { return "~/.local/bin/konsertd" }

func (posixShell) finishCopy(string) string { return `chmod +x "` + posixUserBin + `"` }

func (posixShell) konsertd(exe string, args []string, _ bool) string {
	// exe is the absolute path findKonsertd printed (the shell expanded $HOME there).
	words := append(make([]string, 0, len(args)+1), shQuote(exe))
	for _, a := range args {
		words = append(words, shQuote(a))
	}
	return strings.Join(words, " ")
}

// windowsShell targets Windows with OpenSSH. konsertd.exe lives in
// %LOCALAPPDATA%\Programs\konsertd unless one is already on PATH.
type windowsShell struct{}

// encodePS wraps a PowerShell script as a command line both cmd.exe and PowerShell run.
func encodePS(script string) string {
	// Without this, Windows PowerShell writes progress records ("Preparing modules for
	// first use") to a redirected stderr as CLIXML, burying real error messages.
	u := utf16.Encode([]rune(psPrelude + script))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		b[2*i] = byte(c)
		b[2*i+1] = byte(c >> 8)
	}
	return "powershell -NoProfile -NonInteractive -EncodedCommand " + base64.StdEncoding.EncodeToString(b)
}

const psPrelude = "$ProgressPreference = 'SilentlyContinue'\n"

// decodePS returns the script inside an encodePS command line, if it is one (without
// the prelude encodePS adds).
func decodePS(cmd string) (string, bool) {
	const prefix = "powershell -NoProfile -NonInteractive -EncodedCommand "
	if !strings.HasPrefix(cmd, prefix) {
		return "", false
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(cmd, prefix))
	if err != nil || len(b)%2 != 0 {
		return "", false
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return strings.TrimPrefix(string(utf16.Decode(u)), psPrelude), true
}

// windowsProbeScript prints the OS platform and processor architecture, one per line.
const windowsProbeScript = `[Environment]::OSVersion.Platform; $env:PROCESSOR_ARCHITECTURE`

// parseWindowsProbe parses windowsProbeScript's output ("Win32NT", "AMD64").
func parseWindowsProbe(out string) (arch string, ok bool) {
	var lines []string
	for _, l := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) < 2 || !strings.EqualFold(lines[0], "Win32NT") {
		return "", false
	}
	switch strings.ToUpper(lines[1]) {
	case "AMD64", "X64":
		return "amd64", true
	case "ARM64":
		return "arm64", true
	case "X86":
		return "386", true
	}
	return strings.ToLower(lines[1]), true
}

func (windowsShell) findKonsertd() string {
	return encodePS(`$p = Join-Path $env:LOCALAPPDATA 'Programs\konsertd\konsertd.exe'
if (Test-Path -LiteralPath $p) { "found:$p" }
elseif ($c = Get-Command konsertd.exe -ErrorAction SilentlyContinue) { "found:" + $c.Source }
else { "missing:$p" }`)
}

func (windowsShell) prepareInstall(exePath string) string {
	return encodePS(`New-Item -ItemType Directory -Force -Path (Split-Path -Parent ` + psQuote(exePath) + `) | Out-Null`)
}

// scpPath turns C:\Users\me\x.exe into /C:/Users/me/x.exe, the form Windows OpenSSH's
// SFTP server accepts for an absolute path.
func (windowsShell) scpPath(exePath string) string {
	p := strings.ReplaceAll(exePath, `\`, "/")
	if len(p) >= 2 && p[1] == ':' {
		p = "/" + p
	}
	return p
}

func (windowsShell) finishCopy(string) string { return "" }

func (windowsShell) konsertd(exe string, args []string, credOnStdin bool) string {
	var b strings.Builder
	pipe := ""
	if credOnStdin {
		// The credential is piped to konsertd's stdin, never put in argv.
		b.WriteString("$cred = [Console]::In.ReadLine()\n")
		pipe = "$cred | "
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = psQuote(a)
	}
	fmt.Fprintf(&b, "$out = %s& %s %s 2>&1 | Out-String\n", pipe, psQuote(exe), strings.Join(quoted, " "))
	b.WriteString("if ($LASTEXITCODE -ne 0) { [Console]::Error.WriteLine($out.Trim()); exit $LASTEXITCODE }\n")
	b.WriteString("$out")
	return encodePS(b.String())
}
