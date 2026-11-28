package chat

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

// CLIStatus is the default StatusChecker for agents without a chat engine: it runs the
// CLI's own side-effect-free status command where one exists (output is parsed, never
// logged) and reads copilot's local plan cache. See docs/research/agent-cli-capabilities.md.
func CLIStatus(ctx context.Context, acct Account) protocol.AgentAccount {
	a := protocol.AgentAccount{ID: acct.ID, Agent: acct.Agent, Label: acct.Label, IsDefault: acct.IsDefault,
		CheckedAt: time.Now().UTC(), Auth: protocol.AgentAuthStatus{State: protocol.AuthUnknown}}
	info, _ := lookupAgent(acct.Agent)
	relogin := func() *protocol.AgentRelogin {
		if info.Relogin == "" {
			return nil
		}
		return &protocol.AgentRelogin{Command: info.Relogin, Env: ProfileEnv(acct)}
	}
	switch acct.Agent {
	case "claude":
		out, code, err := runStatus(ctx, acct, "auth", "status")
		var st struct {
			LoggedIn         bool    `json:"loggedIn"`
			AuthMethod       string  `json:"authMethod"`
			LoginExpiresAt   *string `json:"loginExpiresAt"`
			SubscriptionType string  `json:"subscriptionType"`
		}
		if jerr := unmarshal(out, &st); jerr == nil {
			a.Auth.Method = st.AuthMethod
			a.Plan = st.SubscriptionType
			if st.LoggedIn {
				a.Auth.State = protocol.AuthOK
				if st.LoginExpiresAt != nil {
					if t, perr := time.Parse(time.RFC3339, *st.LoginExpiresAt); perr == nil {
						a.Auth.ExpiresAt = &t
						if time.Until(t) < 48*time.Hour {
							a.Auth.State = protocol.AuthExpiring
						}
					}
				}
			} else {
				a.Auth.State = protocol.AuthLoggedOut
			}
		} else if err == nil && code == 1 {
			a.Auth.State = protocol.AuthLoggedOut
		}
	case "codex":
		_, code, err := runStatus(ctx, acct, "login", "status")
		switch {
		case err == nil && code == 0:
			a.Auth.State = protocol.AuthOK
		case err == nil:
			a.Auth.State = protocol.AuthLoggedOut
		}
	case "cursor":
		out, _, _ := runStatus(ctx, acct, "status", "--format", "json")
		var st struct {
			IsAuthenticated *bool `json:"isAuthenticated"`
		}
		if unmarshal(out, &st) == nil && st.IsAuthenticated != nil {
			a.Auth.State = protocol.AuthLoggedOut
			if *st.IsAuthenticated {
				a.Auth.State = protocol.AuthOK
			}
		}
	case "grok":
		// `grok models` exits 0 either way; the first line says whether we are logged in.
		out, _, _ := runStatus(ctx, acct, "models")
		low := strings.ToLower(string(out))
		switch {
		case strings.Contains(low, "not authenticated") || strings.Contains(low, "not logged in"):
			a.Auth.State = protocol.AuthLoggedOut
		case strings.Contains(low, "logged in"):
			a.Auth.State = protocol.AuthOK
		}
	}
	if a.Auth.State == protocol.AuthLoggedOut {
		a.Auth.Relogin = relogin()
	}
	return a
}

// runStatus runs `<binary> args...` with the account env, stdin closed, a 10 s cap and
// at most 256 KiB of captured output. A non-nil error means the command did not run to
// completion (code is then meaningless).
func runStatus(ctx context.Context, acct Account, args ...string) ([]byte, int, error) {
	if acct.Binary == "" {
		return nil, -1, errors.New("binary not found")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, acct.Binary, append(append([]string(nil), acct.ArgsPrefix...), args...)...)
	cmd.Env = AccountEnviron(os.Environ(), acct)
	cmd.WaitDelay = 2 * time.Second
	var buf limitedBuffer
	buf.max = 256 << 10
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return buf.Bytes(), exitErr.ExitCode(), nil
	}
	if err != nil {
		return buf.Bytes(), -1, err
	}
	return buf.Bytes(), 0, nil
}

type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room > 0 {
		if len(p) > room {
			b.Buffer.Write(p[:room])
		} else {
			b.Buffer.Write(p)
		}
	}
	return len(p), nil
}
