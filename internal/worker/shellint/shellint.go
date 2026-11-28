// Package shellint is shell integration for Syre terminals: scripts that make zsh,
// bash and PowerShell report each command line, its working directory and exit status
// through escape sequences in the terminal stream, a streaming parser that extracts
// them in syred, and the tracker that turns them into operator-command events.
//
// # Marks
//
// The shells emit the standard OSC 133 prompt marks (FinalTerm / VS Code / Windows
// Terminal semantics), terminated by BEL:
//
//	ESC ] 133 ; A BEL          prompt start
//	ESC ] 133 ; B BEL          prompt end, input start
//	ESC ] 133 ; C BEL          command line accepted, about to run
//	ESC ] 133 ; D ; <exit> BEL command finished with exit status <exit>
//
// and two Syre-private marks in the VS Code OSC 633 format, each ending with the
// per-terminal nonce syred passes in SYRE_SHELL_NONCE (the shell removes it from
// its environment, so programs running in the terminal cannot forge commands):
//
//	ESC ] 633 ; E ; <escaped command line> ; <nonce> BEL   (sent just before C)
//	ESC ] 633 ; P ; Cwd=<escaped path> ; <nonce> BEL       (sent at every prompt)
//
// Values are escaped as in VS Code: "\" is "\\", ";" is "\x3b", and control characters
// are "\xNN". syred strips every OSC 633 sequence from the stream before it reaches
// the ring buffer or the browser; OSC 133 passes through (xterm.js ignores it).
//
// # Shells
//
//   - zsh: ZDOTDIR points at a directory whose .zshenv/.zprofile/.zshrc/.zlogin source
//     the user's own files, then add precmd/preexec hooks and restore ZDOTDIR.
//   - bash: --init-file runs the usual login (or .bashrc) startup files, then installs a
//     DEBUG trap (pre-exec) and PROMPT_COMMAND entries, chaining existing ones.
//   - PowerShell (pwsh, powershell.exe): -NoExit -Command dot-sources the script after
//     the profile; it wraps the prompt function and PSReadLine's AddToHistoryHandler.
//     Windows PowerShell 5.1 decodes typed input through the console's legacy code page,
//     so characters outside it (e.g. U+2713 on code page 437) never reach the shell; the
//     reported command matches what the shell received. pwsh 7 has no such limit.
//   - fish (3.x, 4.x): --init-command sources syre.fish after the user's config.fish; it
//     hooks the fish_prompt, fish_preexec and fish_postexec events. fish 4 starts with
//     --features no-mark-prompt, because its own OSC 133 marks come before the event
//     handlers run (C would precede the trusted command line). fish 4 also queries the
//     terminal at startup and waits up to 10 s for answers: a browser tab answers at
//     once; a terminal nobody watches starts after that wait.
//   - cmd.exe, sh, dash and others: not integrated (no command events).
package shellint

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed all:scripts
var scripts embed.FS

// Env variables understood by the integration scripts.
const (
	NonceEnv       = "SYRE_SHELL_NONCE"
	loginEnv       = "SYRE_SHELL_LOGIN"
	userZdotdirEnv = "SYRE_USER_ZDOTDIR"
)

// Integration enables shell integration for one terminal.
type Integration struct {
	Dir   string // directory written by Prepare
	Nonce string // per-terminal secret the shell echoes in Syre-private marks
}

// NewNonce returns a fresh random nonce.
func NewNonce() string { return rand.Text() }

// Kind classifies a shell binary: "zsh", "bash", "pwsh" or "" (not supported).
func Kind(bin string) string {
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(bin, `\`, "/")))
	base = strings.TrimSuffix(base, ".exe")
	switch base {
	case "zsh", "bash", "fish":
		return base
	case "pwsh", "powershell":
		return "pwsh"
	}
	return ""
}

// Prepare writes the integration scripts under base (in a directory named after their
// content hash, so daemons of different versions never share files) and returns it.
// Existing identical files are reused.
func Prepare(base string) (string, error) {
	h := sha256.New()
	var files []string
	err := fs.WalkDir(scripts, "scripts", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == ".gitattributes" {
			return err
		}
		b, err := scripts.ReadFile(p)
		if err != nil {
			return err
		}
		files = append(files, p)
		fmt.Fprintf(h, "%s\x00%d\x00", p, len(b))
		h.Write(b)
		return nil
	})
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, hex.EncodeToString(h.Sum(nil))[:16])
	for _, p := range files {
		b, _ := scripts.ReadFile(p)
		dst := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(p, "scripts/")))
		if cur, err := os.ReadFile(dst); err == nil && string(cur) == string(b) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return "", err
		}
		tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
		if err != nil {
			return "", err
		}
		_, werr := tmp.Write(b)
		cerr := tmp.Close()
		if werr == nil {
			werr = cerr
		}
		if werr == nil {
			werr = os.Rename(tmp.Name(), dst)
		}
		if werr != nil {
			_ = os.Remove(tmp.Name())
			return "", werr
		}
	}
	return dir, nil
}

// Command returns the arguments and extra environment that start bin as an interactive
// shell with integration. A non-empty prompt runs first (as a login shell command, or
// as part of PowerShell's -Command), then the integrated interactive shell takes over.
// ok is false when bin is not a supported shell; the caller then launches it as usual.
func (in Integration) Command(bin, prompt string) (args, env []string, ok bool) {
	if in.Dir == "" || in.Nonce == "" {
		return nil, nil, false
	}
	env = []string{NonceEnv + "=" + in.Nonce}
	hasPrompt := strings.TrimSpace(prompt) != ""
	switch Kind(bin) {
	case "zsh":
		zdot := filepath.Join(in.Dir, "zsh")
		env = append(env, "ZDOTDIR="+zdot)
		if cur, set := os.LookupEnv("ZDOTDIR"); set && cur != zdot {
			env = append(env, userZdotdirEnv+"="+cur)
		}
		if hasPrompt {
			// The exec'd shell inherits ZDOTDIR (the non-interactive login shell keeps it).
			return []string{"-l", "-c", prompt + "\nexec " + shQuote(bin) + " -l"}, env, true
		}
		return []string{"-l"}, env, true
	case "bash":
		rc := filepath.Join(in.Dir, "syre.bash")
		env = append(env, loginEnv+"=1")
		// On Windows (Git Bash / MSYS2), /etc/profile cd's to $HOME unless
		// CHERE_INVOKING is set, and forward slashes avoid backslash quoting issues.
		if isWindowsBinary(bin) {
			env = append(env, "CHERE_INVOKING=1")
			rc = strings.ReplaceAll(rc, `\`, "/")
			bin = strings.ReplaceAll(bin, `\`, "/")
		}
		if hasPrompt {
			return []string{"-l", "-c", prompt + "\nexec " + shQuote(bin) + " --init-file " + shQuote(rc) + " -i"}, env, true
		}
		return []string{"--init-file", rc, "-i"}, env, true
	case "fish":
		rc := filepath.Join(in.Dir, "syre.fish")
		var features []string
		if fishMajor(bin) >= 4 {
			features = []string{"--features", "no-mark-prompt"}
		}
		shell := append(append([]string{}, features...), "-l", "-i", "--init-command", "source "+fishQuote(rc))
		if hasPrompt {
			then := "exec " + fishQuote(bin)
			for _, a := range shell {
				then += " " + fishQuote(a)
			}
			return append(append([]string{}, features...), "-l", "-c", prompt+"\n"+then), env, true
		}
		return shell, env, true
	case "pwsh":
		ps1 := filepath.Join(in.Dir, "syre.ps1")
		// A script block is not subject to the execution policy, unlike a .ps1 file.
		cmd := ". ([scriptblock]::Create([IO.File]::ReadAllText(" + psQuote(ps1) + ")))"
		if hasPrompt {
			cmd += "\n" + prompt
		}
		return []string{"-NoLogo", "-NoExit", "-Command", cmd}, env, true
	}
	return nil, nil, false
}

func isWindowsBinary(bin string) bool {
	return strings.HasSuffix(strings.ToLower(bin), ".exe") ||
		strings.Contains(bin, `\`) ||
		(len(bin) >= 2 && bin[1] == ':')
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// fishQuote quotes s for fish: inside single quotes only \ and ' are special.
func fishQuote(s string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", `\'`) + "'"
}

var fishVersions sync.Map // binary path -> major version

// fishMajor is the major version of a fish binary (0 when unknown), read once per path.
func fishMajor(bin string) int {
	if v, ok := fishVersions.Load(bin); ok {
		return v.(int)
	}
	major := 0
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, bin, "--version").Output(); err == nil {
		major = parseFishMajor(string(out))
	}
	fishVersions.Store(bin, major)
	return major
}

// parseFishMajor reads "fish, version 4.9.3" (0 when it can't).
func parseFishMajor(s string) int {
	_, v, ok := strings.Cut(s, "version ")
	if !ok {
		return 0
	}
	n := 0
	for _, r := range strings.TrimSpace(v) {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}
