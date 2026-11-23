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
	for _, f := range []string{"zsh/.zshenv", "zsh/.zprofile", "zsh/.zshrc", "zsh/.zlogin", "zsh/konsert.zsh", "konsert.bash", "konsert.ps1"} {
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
	if !ok || !slices.Equal(args, []string{"--init-file", filepath.Join("/d", "konsert.bash"), "-i"}) || !hasEnv(env, loginEnv+"=1") {
		t.Fatalf("bash: %v %v %v", args, env, ok)
	}
	args, _, _ = in.Command("/usr/bin/bash", "codex login")
	if len(args) != 3 || !strings.HasPrefix(args[2], "codex login\nexec '/usr/bin/bash' --init-file ") {
		t.Fatalf("bash with prompt: %q", args)
	}

	args, _, ok = in.Command(`C:\Program Files\PowerShell\7\pwsh.exe`, "codex login")
	if !ok || args[0] != "-NoLogo" || args[1] != "-NoExit" || args[2] != "-Command" ||
		!strings.Contains(args[3], "ReadAllText(") || !strings.HasSuffix(args[3], "\ncodex login") {
		t.Fatalf("pwsh: %q", args)
	}
	if _, _, ok := in.Command(`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, ""); !ok {
		t.Fatal("powershell.exe should be supported")
	}
	for _, unsupported := range []string{"/bin/sh", "/usr/bin/fish", `C:\Windows\System32\cmd.exe`, "/bin/dash"} {
		if _, _, ok := in.Command(unsupported, ""); ok {
			t.Errorf("%s should not be integrated", unsupported)
		}
	}
	if _, _, ok := (Integration{}).Command("/bin/zsh", ""); ok {
		t.Error("an empty integration must not apply")
	}
}
