package shellint

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPrepareWritesScriptsIdempotently(t *testing.T) {
	base := t.TempDir()
	dir, err := Prepare(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"zsh/.zshenv", "zsh/.zprofile", "zsh/.zshrc", "zsh/.zlogin", "zsh/syre.zsh", "syre.bash", "syre.ps1"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	again, err := Prepare(base)
	if err != nil || again != dir {
		t.Fatalf("second Prepare = %q, %v; want %q", again, err, dir)
	}
}

func TestIntegrationCommand(t *testing.T) {
	in := Integration{Dir: "/d", Nonce: "n"}
	hasEnv := func(env []string, kv string) bool { return slices.Contains(env, kv) }

	args, env, ok := in.Command("/bin/zsh", "")
	if !ok || !slices.Equal(args, []string{"-l"}) || !hasEnv(env, "ZDOTDIR="+filepath.Join("/d", "zsh")) || !hasEnv(env, NonceEnv+"=n") {
		t.Fatalf("zsh: %v %v %v", args, env, ok)
	}
	args, _, _ = in.Command("/bin/zsh", "codex login")
	if len(args) != 3 || args[0] != "-l" || args[1] != "-c" || args[2] != "codex login\nexec '/bin/zsh' -l" {
		t.Fatalf("zsh with prompt: %q", args)
	}

	args, env, ok = in.Command("/usr/bin/bash", "")
	if !ok || !slices.Equal(args, []string{"--init-file", filepath.Join("/d", "syre.bash"), "-i"}) || !hasEnv(env, loginEnv+"=1") {
		t.Fatalf("bash: %v %v %v", args, env, ok)
	}
	args, _, _ = in.Command("/usr/bin/bash", "codex login")
	if len(args) != 3 || !strings.HasPrefix(args[2], "codex login\nexec '/usr/bin/bash' --init-file ") {
		t.Fatalf("bash with prompt: %q", args)
	}

	// Windows Git Bash: CHERE_INVOKING=1 preserves workdir and forward slashes avoid quoting bugs.
	winIn := Integration{Dir: `C:\Users\alice\shell-integration\abc`, Nonce: "n"}
	args, env, ok = winIn.Command(`C:\Program Files\Git\bin\bash.exe`, "")
	if !ok || len(args) != 3 || args[0] != "--init-file" || !strings.HasSuffix(args[1], "/syre.bash") || strings.Contains(args[1], `\`) || args[2] != "-i" ||
		!hasEnv(env, "CHERE_INVOKING=1") || !hasEnv(env, loginEnv+"=1") {
		t.Fatalf("git-bash: %v %v %v", args, env, ok)
	}

	args, _, ok = in.Command(`C:\Program Files\PowerShell\7\pwsh.exe`, "codex login")
	if !ok || args[0] != "-NoLogo" || args[1] != "-NoExit" || args[2] != "-Command" ||
		!strings.Contains(args[3], "ReadAllText(") || !strings.HasSuffix(args[3], "\ncodex login") {
		t.Fatalf("pwsh: %q", args)
	}
	if _, _, ok := in.Command(`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, ""); !ok {
		t.Fatal("powershell.exe should be supported")
	}
	for _, unsupported := range []string{"/bin/sh", `C:\Windows\System32\cmd.exe`, "/bin/dash"} {
		if _, _, ok := in.Command(unsupported, ""); ok {
			t.Errorf("%s should not be integrated", unsupported)
		}
	}
	if _, _, ok := (Integration{}).Command("/bin/zsh", ""); ok {
		t.Error("an empty integration must not apply")
	}
}

func TestFishCommand(t *testing.T) {
	in := Integration{Dir: "/d", Nonce: "n"}
	src := "source " + fishQuote(filepath.Join("/d", "syre.fish"))
	// fish 3 (or a version we can't read): no feature flags.
	fishVersions.Store("/usr/bin/fish", 3)
	args, env, ok := in.Command("/usr/bin/fish", "")
	if !ok || !slices.Equal(args, []string{"-l", "-i", "--init-command", src}) || !slices.Contains(env, NonceEnv+"=n") {
		t.Fatalf("fish 3: %q %v %v", args, env, ok)
	}
	// fish 4 marks prompts itself, before our hooks run: that is turned off.
	fishVersions.Store("/opt/fish4/fish", 4)
	args, _, _ = in.Command("/opt/fish4/fish", "")
	if !slices.Equal(args, []string{"--features", "no-mark-prompt", "-l", "-i", "--init-command", src}) {
		t.Fatalf("fish 4: %q", args)
	}
	// A prompt runs first, then the integrated shell.
	args, _, _ = in.Command("/opt/fish4/fish", "codex login")
	want := "codex login\nexec '/opt/fish4/fish' '--features' 'no-mark-prompt' '-l' '-i' '--init-command' " + fishQuote(src)
	if len(args) != 5 || args[2] != "-l" || args[3] != "-c" || args[4] != want {
		t.Fatalf("fish with prompt: %q", args)
	}
}

func TestParseFishMajorAndQuote(t *testing.T) {
	for in, want := range map[string]int{"fish, version 4.9.3\n": 4, "fish, version 3.7.1": 3, "fish, version 10.0b1": 10, "nonsense": 0} {
		if got := parseFishMajor(in); got != want {
			t.Errorf("%q: %d, want %d", in, got, want)
		}
	}
	if got := fishQuote(`it's a \ path`); got != `'it\'s a \\ path'` {
		t.Errorf("fishQuote = %s", got)
	}
}
