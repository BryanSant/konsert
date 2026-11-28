package chat

// Antigravity plan usage (SYR-MJ9PB): `agy -p /usage --output-format json` runs the
// /usage slash command headlessly, with no model call and no conversation left behind
// (agy ≥ 1.5). Each model group has a weekly and a 5-hour bucket with the fraction left.

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

const agyUsageTimeout = 60 * time.Second

// agyUsage runs agy's /usage for the account, in a scratch dir, with auto-update off.
func agyUsage(ctx context.Context, acct Account) acpUsage {
	dir, err := ScratchDir("syre-agy-usage-")
	if err != nil {
		return acpUsage{Err: err}
	}
	defer os.RemoveAll(dir)
	env := map[string]string{}
	for k, v := range acct.Env {
		env[k] = v
	}
	env["AGY_CLI_DISABLE_AUTO_UPDATE"] = "true"
	bin := acct.Binary
	if bin == "" {
		bin = "agy"
	}
	args := append(append([]string{}, acct.ArgsPrefix...), "-p", "/usage", "--output-format", "json")
	out, code, err := RunQuickIn(ctx, dir, bin, args, env, agyUsageTimeout)
	if err != nil {
		return acpUsage{Err: errors.New("agy /usage did not finish")}
	}
	w, perr := parseAgyUsage([]byte(out))
	if perr != nil {
		if code != 0 {
			return acpUsage{Err: errors.New("agy /usage failed: update agy to see usage")}
		}
		return acpUsage{Err: perr}
	}
	return acpUsage{Windows: w}
}

type agyUsageOutput struct {
	Status  string `json:"status"`
	Command struct {
		Name string `json:"name"`
		Data struct {
			Groups []struct {
				Name    string `json:"name"`
				Buckets []struct {
					ID                string   `json:"id"`
					Name              string   `json:"name"`
					Window            string   `json:"window"`
					RemainingFraction *float64 `json:"remaining_fraction"`
					ResetTime         string   `json:"reset_time"`
				} `json:"buckets"`
			} `json:"groups"`
		} `json:"data"`
	} `json:"command"`
}

// parseAgyUsage turns /usage JSON into windows: <five_hour|seven_day>_<group>, labelled
// "<Group> · weekly"; used = 1 − remaining. A reset at the epoch means the bucket is
// untouched (no reset time); a bucket without remaining_fraction is unknown, not
// exhausted, and is left out.
func parseAgyUsage(raw []byte) ([]protocol.AgentRateLimit, error) {
	var o agyUsageOutput
	if err := unmarshal(raw, &o); err != nil {
		return nil, errors.New("agy /usage printed no JSON: update agy to see usage")
	}
	if o.Status != "SUCCESS" || o.Command.Name != "usage" {
		return nil, errors.New("agy /usage did not succeed")
	}
	var out []protocol.AgentRateLimit
	for _, g := range o.Command.Data.Groups {
		slug := agyGroupSlug(g.Name)
		for _, b := range g.Buckets {
			if b.RemainingFraction == nil {
				continue
			}
			base, word := "", ""
			switch strings.ToLower(b.Window) {
			case "weekly", "7d":
				base, word = "seven_day", "weekly"
			case "5h", "five_hour":
				base, word = "five_hour", "5-hour"
			case "daily", "1d":
				base, word = "daily", "daily"
			default:
				base, word = strings.ToLower(b.Window), b.Window
			}
			used := (1 - min(max(*b.RemainingFraction, 0), 1)) * 100
			rl := protocol.AgentRateLimit{Window: base + "_" + slug, Label: g.Name + " · " + word,
				UsedPercent: float64(int(used*10+0.5)) / 10, State: claudePercentState(used)}
			if used >= 100 {
				rl.State = "exhausted"
			}
			if t, err := time.Parse(time.RFC3339, b.ResetTime); err == nil && t.Year() > 1970 {
				t = t.UTC()
				rl.ResetsAt = &t
			}
			out = append(out, rl)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("agy reported no usage buckets")
	}
	return out, nil
}

// agyGroupSlug names a model group for a window id: "Gemini Models" → "gemini",
// "Claude and GPT models" → "claude_gpt".
func agyGroupSlug(name string) string {
	var parts []string
	for _, w := range strings.Fields(strings.ToLower(name)) {
		if w == "models" || w == "model" || w == "and" || w == "&" {
			continue
		}
		parts = append(parts, w)
	}
	return strings.Join(parts, "_")
}
