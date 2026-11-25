package drivers

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/shellint"
)

func fakeLookup(goos string, env map[string]string, bins map[string]string, files map[string]bool, gitExec, gitReg string) shellLookup {
	return shellLookup{
		goos: goos,
		getenv: func(k string) string {
			return env[k]
		},
		lookPath: func(name string) (string, error) {
			if p, ok := bins[name]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
		statFile: func(p string) bool {
			return files[p]
		},
		gitExecPath: func() string { return gitExec },
		gitRegDir:   func() string { return gitReg },
	}
}

func TestDetectGitBash(t *testing.T) {
	t.Run("via git --exec-path", func(t *testing.T) {
		l := fakeLookup("windows", nil, nil, map[string]bool{
			`D:\Tools\Git\bin\bash.exe`: true,
		}, `D:/Tools/Git/mingw64/libexec/git-core`, "")
		p, ok := detectGitBash(l)
		if !ok || p != `D:\Tools\Git\bin\bash.exe` {
			t.Fatalf("got %q, %v; want D:\\Tools\\Git\\bin\\bash.exe, true", p, ok)
		}
	})

	t.Run("via git --exec-path falls back to usr/bin/bash.exe", func(t *testing.T) {
		l := fakeLookup("windows", nil, nil, map[string]bool{
			`C:\PortableGit\usr\bin\bash.exe`: true,
		}, `C:\PortableGit\mingw64\libexec\git-core`, "")
		p, ok := detectGitBash(l)
		if !ok || p != `C:\PortableGit\usr\bin\bash.exe` {
			t.Fatalf("got %q, %v", p, ok)
		}
	})

	t.Run("via registry", func(t *testing.T) {
		l := fakeLookup("windows", nil, nil, map[string]bool{
			`E:\Apps\Git\bin\bash.exe`: true,
		}, "", `E:\Apps\Git`)
		p, ok := detectGitBash(l)
		if !ok || p != `E:\Apps\Git\bin\bash.exe` {
			t.Fatalf("got %q, %v", p, ok)
		}
	})

	t.Run("via ProgramFiles", func(t *testing.T) {
		l := fakeLookup("windows", map[string]string{
			"ProgramFiles": `C:\Program Files`,
		}, nil, map[string]bool{
			`C:\Program Files\Git\bin\bash.exe`: true,
		}, "", "")
		p, ok := detectGitBash(l)
		if !ok || p != `C:\Program Files\Git\bin\bash.exe` {
			t.Fatalf("got %q, %v", p, ok)
		}
	})

	t.Run("via LocalAppData", func(t *testing.T) {
		l := fakeLookup("windows", map[string]string{
			"ProgramFiles": `C:\Program Files`,
			"LocalAppData": `C:\Users\alice\AppData\Local`,
		}, nil, map[string]bool{
			`C:\Users\alice\AppData\Local\Programs\Git\bin\bash.exe`: true,
		}, "", "")
		p, ok := detectGitBash(l)
		if !ok || p != `C:\Users\alice\AppData\Local\Programs\Git\bin\bash.exe` {
			t.Fatalf("got %q, %v", p, ok)
		}
	})

	t.Run("ignores WSL System32 bash.exe", func(t *testing.T) {
		l := fakeLookup("windows", map[string]string{
			"ProgramFiles": `C:\Program Files`,
		}, map[string]string{
			"bash.exe": `C:\Windows\System32\bash.exe`,
		}, nil, "", "")
		if p, ok := detectGitBash(l); ok {
			t.Fatalf("must not pick WSL bash.exe, got %q", p)
		}
	})

	t.Run("non-windows returns false", func(t *testing.T) {
		l := fakeLookup("linux", nil, nil, map[string]bool{
			`C:\Program Files\Git\bin\bash.exe`: true,
		}, "", "")
		if p, ok := detectGitBash(l); ok {
			t.Fatalf("expected false on linux, got %q", p)
		}
	})
}

func TestDetectShellsMatrix(t *testing.T) {
	t.Run("windows", func(t *testing.T) {
		l := fakeLookup("windows",
			map[string]string{"ProgramFiles": `C:\Program Files`},
			map[string]string{
				"pwsh.exe":       `C:\Program Files\PowerShell\7\pwsh.exe`,
				"powershell.exe": `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
			},
			map[string]bool{`C:\Program Files\Git\bin\bash.exe`: true},
			"", "")
		got := detectShells(l)
		wantIDs := []string{protocol.ShellGitBash, protocol.ShellPwsh, protocol.ShellPowerShell}
		var ids []string
		for _, s := range got {
			ids = append(ids, s.ID)
		}
		if !slices.Equal(ids, wantIDs) {
			t.Fatalf("windows shells = %v, want %v", ids, wantIDs)
		}
	})

	t.Run("darwin", func(t *testing.T) {
		l := fakeLookup("darwin", nil, map[string]string{
			"zsh":  "/bin/zsh",
			"bash": "/bin/bash",
			"fish": "/opt/homebrew/bin/fish",
		}, nil, "", "")
		got := detectShells(l)
		var ids []string
		for _, s := range got {
			ids = append(ids, s.ID)
		}
		if !slices.Equal(ids, []string{protocol.ShellZsh, protocol.ShellBash}) {
			t.Fatalf("darwin shells = %v", ids)
		}
	})

	t.Run("linux", func(t *testing.T) {
		l := fakeLookup("linux", nil, map[string]string{
			"bash": "/usr/bin/bash",
			"zsh":  "/usr/bin/zsh",
			"fish": "/usr/bin/fish",
		}, nil, "", "")
		got := detectShells(l)
		var ids []string
		for _, s := range got {
			ids = append(ids, s.ID)
		}
		if !slices.Equal(ids, []string{protocol.ShellBash, protocol.ShellZsh, protocol.ShellFish}) {
			t.Fatalf("linux shells = %v", ids)
		}
	})
}

func TestShellSelectionAndBuildCommand(t *testing.T) {
	winLookup := fakeLookup("windows",
		map[string]string{"ProgramFiles": `C:\Program Files`},
		map[string]string{
			"pwsh.exe":       `C:\Program Files\PowerShell\7\pwsh.exe`,
			"powershell.exe": `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
		},
		map[string]bool{`C:\Program Files\Git\bin\bash.exe`: true},
		"", "")

	integ := &shellint.Integration{Dir: `C:\Users\alice\AppData\Local\syre\shell-integration\abc`, Nonce: "n1"}

	// Default on Windows stays pwsh.
	spec := buildShellCommand(winLookup, LaunchOptions{Mode: protocol.TaskModeInteractive, ShellIntegration: integ})
	if spec.Binary != `C:\Program Files\PowerShell\7\pwsh.exe` || !spec.ShellIntegration {
		t.Fatalf("default windows spec = %+v", spec)
	}

	// Explicit Git Bash with shell integration.
	spec = buildShellCommand(winLookup, LaunchOptions{
		Mode:             protocol.TaskModeInteractive,
		Shell:            protocol.ShellGitBash,
		ShellIntegration: integ,
	})
	if spec.Binary != `C:\Program Files\Git\bin\bash.exe` || !spec.ShellIntegration {
		t.Fatalf("git-bash spec = %+v", spec)
	}
	if len(spec.Args) != 3 || spec.Args[0] != "--init-file" || !strings.HasSuffix(spec.Args[1], "/syre.bash") || spec.Args[2] != "-i" {
		t.Fatalf("git-bash args = %q", spec.Args)
	}
	if !slices.Contains(spec.Env, "CHERE_INVOKING=1") || !slices.Contains(spec.Env, "SYRE_SHELL_LOGIN=1") || !slices.Contains(spec.Env, shellint.NonceEnv+"=n1") {
		t.Fatalf("git-bash env = %q", spec.Env)
	}

	// Explicit Git Bash without integration uses POSIX flags, not PowerShell -NoLogo.
	plain := buildShellCommand(winLookup, LaunchOptions{
		Mode:  protocol.TaskModeInteractive,
		Shell: protocol.ShellGitBash,
	})
	if !slices.Equal(plain.Args, []string{"-l"}) || !slices.Contains(plain.Env, "CHERE_INVOKING=1") {
		t.Fatalf("git-bash plain = %+v", plain)
	}

	oneshot := buildShellCommand(winLookup, LaunchOptions{
		Mode:   protocol.TaskModeOneshot,
		Shell:  protocol.ShellGitBash,
		Prompt: "echo hi",
	})
	if !slices.Equal(oneshot.Args, []string{"-c", "--", "echo hi"}) {
		t.Fatalf("git-bash oneshot = %+v", oneshot)
	}

	// Explicit PowerShell 5.1.
	ps5 := buildShellCommand(winLookup, LaunchOptions{
		Mode:             protocol.TaskModeInteractive,
		Shell:            protocol.ShellPowerShell,
		ShellIntegration: integ,
	})
	if ps5.Binary != `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe` || !ps5.ShellIntegration {
		t.Fatalf("powershell 5.1 spec = %+v", ps5)
	}

	// Unavailable requested shell falls back to default.
	fallback := buildShellCommand(winLookup, LaunchOptions{
		Mode:  protocol.TaskModeInteractive,
		Shell: protocol.ShellZsh,
	})
	if fallback.Binary != `C:\Program Files\PowerShell\7\pwsh.exe` {
		t.Fatalf("fallback binary = %q, want pwsh.exe", fallback.Binary)
	}
}
