package chat

// Grok plan usage (SYR-T6XCS) from the billing API grok's TUI uses
// (cli-chat-proxy.grok.com), with the account's own login from $GROK_HOME/auth.json.
// Syre never refreshes the token: the grok CLI owns it.

import (
	"context"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BryanSant/syre/internal/protocol"
)

var grokHTTP = newUsageClient()

const grokDefaultProxy = "https://cli-chat-proxy.grok.com"

type grokAuth struct {
	Key       string    `json:"key"`
	UserID    string    `json:"user_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// grokLogin picks the account's auth.x.ai login that is good for more than 5 minutes.
func grokLogin(acct Account, now time.Time) (grokAuth, string) {
	raw, err := os.ReadFile(filepath.Join(accountDir(acct, "GROK_HOME", ".grok"), "auth.json"))
	if err != nil {
		return grokAuth{}, "Not signed in to Grok"
	}
	var all map[string]grokAuth
	if unmarshal(raw, &all) != nil {
		return grokAuth{}, "Grok's auth.json is unreadable"
	}
	expired := false
	for k, a := range all {
		if !strings.HasPrefix(k, "https://auth.x.ai") || a.Key == "" {
			continue
		}
		if a.ExpiresAt.IsZero() || a.ExpiresAt.After(now.Add(5*time.Minute)) {
			return a, ""
		}
		expired = true
	}
	if expired {
		return grokAuth{}, "Grok sign-in expired: run grok once to refresh it"
	}
	return grokAuth{}, "Not signed in to Grok"
}

func grokUsage(ctx context.Context, acct Account) acpUsage {
	login, note := grokLogin(acct, time.Now())
	if login.Key == "" {
		return acpUsage{Note: note}
	}
	base := strings.TrimRight(cmpOrString(acct.Env["GROK_CLI_CHAT_PROXY_BASE_URL"], cmpOrString(os.Getenv("GROK_CLI_CHAT_PROXY_BASE_URL"), grokDefaultProxy)), "/")
	h := http.Header{}
	h.Set("Authorization", "Bearer "+login.Key)
	h.Set("X-XAI-Token-Auth", "xai-grok-cli")
	h.Set("Accept", "application/json")
	if login.UserID != "" {
		h.Set("x-userid", login.UserID)
	}
	key := "grok:" + acct.ID
	body, err := grokHTTP.Get(ctx, key, base+"/v1/billing?format=credits", h)
	if err == nil {
		if u, ok := parseGrokBilling(body); ok {
			return u
		}
	}
	monthly, merr := grokHTTP.Get(ctx, key, base+"/v1/billing", h)
	if merr != nil {
		return acpUsage{Err: cmpErr(err, merr)}
	}
	if u, ok := parseGrokBilling(monthly); ok {
		return u
	}
	return acpUsage{Err: errors.New("grok billing reported no usage")}
}

func cmpErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// grokVal reads a {val} that may be a number or a string.
type grokVal struct {
	Val any `json:"val"`
}

func (v *grokVal) num() (float64, bool) {
	if v == nil {
		return 0, false
	}
	switch x := v.Val.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

type grokBilling struct {
	CurrentPeriod *struct {
		Type string `json:"type"`
		End  string `json:"end"`
	} `json:"currentPeriod"`
	CreditUsagePercent *float64 `json:"creditUsagePercent"`
	SubscriptionTier   string   `json:"subscriptionTier"`
	BillingPeriodEnd   string   `json:"billingPeriodEnd"`
	MonthlyLimit       *grokVal `json:"monthlyLimit"`
	Used               *grokVal `json:"used"`
}

// parseGrokBilling reads either billing form (sometimes wrapped in "config"): the
// credits form gives the period's credit use (weekly on SuperGrok), the monthly form
// used / monthlyLimit. 100% is exhausted.
func parseGrokBilling(raw []byte) (acpUsage, bool) {
	var wrap struct {
		Config *grokBilling `json:"config"`
	}
	var b grokBilling
	if unmarshal(raw, &wrap) == nil && wrap.Config != nil {
		b = *wrap.Config
	} else if unmarshal(raw, &b) != nil {
		return acpUsage{}, false
	}
	u := acpUsage{Plan: b.SubscriptionTier}
	resets := func(s string) *time.Time {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			t = t.UTC()
			return &t
		}
		return nil
	}
	if b.CreditUsagePercent != nil {
		window := "seven_day"
		end := b.BillingPeriodEnd
		if b.CurrentPeriod != nil {
			switch {
			case strings.Contains(b.CurrentPeriod.Type, "MONTH"):
				window = "monthly"
			case strings.Contains(b.CurrentPeriod.Type, "DAY") && !strings.Contains(b.CurrentPeriod.Type, "WEEK"):
				window = "daily"
			}
			end = cmpOrString(b.CurrentPeriod.End, end)
		}
		u.Windows = []protocol.AgentRateLimit{grokWindow(window, "Credits", *b.CreditUsagePercent, resets(end))}
		return u, true
	}
	if limit, ok := b.MonthlyLimit.num(); ok && limit > 0 {
		if used, ok := b.Used.num(); ok {
			u.Windows = []protocol.AgentRateLimit{grokWindow("monthly", "", used/limit*100, resets(b.BillingPeriodEnd))}
			return u, true
		}
	}
	return u, false
}

func grokWindow(window, label string, pct float64, reset *time.Time) protocol.AgentRateLimit {
	pct = math.Round(min(max(pct, 0), 100)*10) / 10
	rl := protocol.AgentRateLimit{Window: window, Label: label, UsedPercent: pct, ResetsAt: reset, State: claudePercentState(pct)}
	if pct >= 100 {
		rl.State = "exhausted"
	}
	return rl
}
