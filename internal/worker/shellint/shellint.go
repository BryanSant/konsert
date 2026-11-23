// Package shellint is shell integration for Konsert terminals: scripts that make zsh,
// bash and PowerShell report each command line, its working directory and exit status
// through escape sequences in the terminal stream, a streaming parser that extracts
// them in konsertd, and the tracker that turns them into operator-command events.
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
// and two Konsert-private marks in the VS Code OSC 633 format, each ending with the
// per-terminal nonce konsertd passes in KONSERT_SHELL_NONCE (the shell removes it from
// its environment, so programs running in the terminal cannot forge commands):
//
//	ESC ] 633 ; E ; <escaped command line> ; <nonce> BEL   (sent just before C)
//	ESC ] 633 ; P ; Cwd=<escaped path> ; <nonce> BEL       (sent at every prompt)
//
// Values are escaped as in VS Code: "\" is "\\", ";" is "\x3b", and control characters
// are "\xNN". konsertd strips every OSC 633 sequence from the stream before it reaches
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
//   - cmd.exe, sh, dash, fish and others: not integrated (no command events).
package shellint

import (
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed all:scripts
var scripts embed.FS

// Env variables understood by the integration scripts.
const (
	NonceEnv       = "KONSERT_SHELL_NONCE"
	loginEnv       = "KONSERT_SHELL_LOGIN"
	userZdotdirEnv = "KONSERT_USER_ZDOTDIR"
)

// Integration enables shell integration for one terminal.
type Integration struct {
	Dir   string // directory written by Prepare
	Nonce string // per-terminal secret the shell echoes in Konsert-private marks
}

// NewNonce returns a fresh random nonce.
func NewNonce() string { return rand.Text() }

// Kind classifies a shell binary: "zsh", "bash", "pwsh" or "" (not supported).
func Kind(bin string) string {
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(bin, `\`, "/")))
	base = strings.TrimSuffix(base, ".exe")
	switch base {
	case "zsh", "bash":
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
		rc := filepath.Join(in.Dir, "konsert.bash")
		env = append(env, loginEnv+"=1")
		if hasPrompt {
			return []string{"-l", "-c", prompt + "\nexec " + shQuote(bin) + " --init-file " + shQuote(rc) + " -i"}, env, true
		}
		return []string{"--init-file", rc, "-i"}, env, true
	case "pwsh":
		ps1 := filepath.Join(in.Dir, "konsert.ps1")
		// A script block is not subject to the execution policy, unlike a .ps1 file.
		cmd := ". ([scriptblock]::Create([IO.File]::ReadAllText(" + psQuote(ps1) + ")))"
		if hasPrompt {
			cmd += "\n" + prompt
		}
		return []string{"-NoLogo", "-NoExit", "-Command", cmd}, env, true
	}
	return nil, nil, false
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func psQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
