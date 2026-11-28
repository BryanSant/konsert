package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/config"
	"github.com/BryanSant/syre/internal/protocol"
	"github.com/BryanSant/syre/internal/worker/drivers"
)

// declaredProbeTimeout bounds one declared probe's command.
const declaredProbeTimeout = 3 * time.Second

// runDeclaredProbes adds the config's probes to f.Tools: a path probe reports
// "installed" when the path exists (~ and globs allowed); a command probe reports the
// version its regex finds in the output (the first group, else the whole match; with no
// regex, the first version-like number), or "installed" when the command succeeds
// without one. A probe that finds nothing adds nothing.
func runDeclaredProbes(ctx context.Context, probes []config.ProbeConfig, f *protocol.MachineFacts) {
	if len(probes) == 0 {
		return
	}
	if f.Tools == nil {
		f.Tools = map[string]string{}
	}
	home, _ := os.UserHomeDir()
	for _, p := range probes {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			continue
		}
		if path := strings.TrimSpace(p.Path); path != "" {
			if strings.HasPrefix(path, "~/") && home != "" {
				path = filepath.Join(home, path[2:])
			}
			if m, _ := filepath.Glob(path); len(m) > 0 {
				f.Tools[name] = "installed"
			}
			continue
		}
		if len(p.Command) == 0 {
			continue
		}
		bin, ok := drivers.FindBinary(p.Command[0])
		if !ok {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, declaredProbeTimeout)
		out, err := exec.CommandContext(pctx, bin, p.Command[1:]...).CombinedOutput()
		cancel()
		if ver := probeVersion(string(out), p.VersionRegex); ver != "" {
			f.Tools[name] = ver
		} else if err == nil {
			f.Tools[name] = "installed"
		}
	}
}

// probeVersion finds a version in out with re (first group, else the match), or the
// first version-like number when re is empty.
func probeVersion(out, re string) string {
	if strings.TrimSpace(re) == "" {
		return semverRegex.FindString(out)
	}
	rx, err := regexp.Compile(re)
	if err != nil {
		return ""
	}
	m := rx.FindStringSubmatch(out)
	switch {
	case len(m) > 1:
		return m[1]
	case len(m) == 1:
		return m[0]
	}
	return ""
}
