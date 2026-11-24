package linkwatch

import (
	"context"
	"net"
	"slices"
	"strings"
	"time"
)

// Default watcher timing.
const (
	// DefaultWatchEvery is how often the watcher looks at the clock and the network.
	DefaultWatchEvery = 5 * time.Second
	// DefaultWakeSlack is how late a tick may be before it counts as a wake from sleep.
	DefaultWakeSlack = 10 * time.Second
)

// Change reasons reported by Watcher.
const (
	ReasonWake    = "wake from sleep"
	ReasonNetwork = "network change"
)

// Watcher notices a wake from sleep (a tick far past its interval, by the wall clock or
// the monotonic one: on Linux and macOS the monotonic clock stops while suspended, the
// wall clock does not) and changes in the machine's network addresses.
type Watcher struct {
	// Every is the tick interval (DefaultWatchEvery when zero) and Slack how late a tick
	// may be (DefaultWakeSlack when zero).
	Every time.Duration
	Slack time.Duration
	// Addrs fingerprints the network addresses (AddrFingerprint when nil); tests inject one.
	Addrs func() (string, error)

	prev     time.Time
	prevAddr string
	started  bool
}

func (w *Watcher) every() time.Duration {
	if w.Every > 0 {
		return w.Every
	}
	return DefaultWatchEvery
}

func (w *Watcher) slack() time.Duration {
	if w.Slack > 0 {
		return w.Slack
	}
	return DefaultWakeSlack
}

func (w *Watcher) addrs() (string, error) {
	if w.Addrs != nil {
		return w.Addrs()
	}
	return AddrFingerprint()
}

// Step looks at the clock (now, which may carry a monotonic reading) and the network,
// and returns a change reason or "". The first call only takes the baseline. A failed
// address read keeps the previous fingerprint.
func (w *Watcher) Step(now time.Time) string {
	addr, err := w.addrs()
	if !w.started {
		w.started, w.prev = true, now
		if err == nil {
			w.prevAddr = addr
		}
		return ""
	}
	// Sub uses the monotonic readings when both times have one; Round(0) strips them.
	gap := max(now.Sub(w.prev), now.Round(0).Sub(w.prev.Round(0)))
	w.prev = now
	reason := ""
	if gap > w.every()+w.slack() {
		reason = ReasonWake
	}
	if err == nil {
		if addr != w.prevAddr && reason == "" {
			reason = ReasonNetwork
		}
		w.prevAddr = addr
	}
	return reason
}

// Run calls onChange with the reason for every change seen on ticks (at the tick's time;
// a real ticker: Watch) until ctx ends; start is the baseline. onChange must not block
// for long.
func (w *Watcher) Run(ctx context.Context, start time.Time, ticks <-chan time.Time, onChange func(reason string)) {
	w.Step(start)
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticks:
			if r := w.Step(t); r != "" {
				onChange(r)
			}
		}
	}
}

// Watch runs the watcher on a real ticker until ctx ends.
func (w *Watcher) Watch(ctx context.Context, onChange func(reason string)) {
	t := time.NewTicker(w.every())
	defer t.Stop()
	w.Run(ctx, time.Now(), t.C, onChange)
}

// AddrFingerprint lists the machine's routable addresses: every IPv4 address and the /64
// prefix of every global IPv6 address (IPv6 privacy addresses rotate within the prefix),
// sorted. Loopback and link-local addresses are left out. One cheap system call
// (getifaddrs or a netlink dump); no subprocess.
func AddrFingerprint() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipn.IP
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			continue
		}
		if v4 := ip.To4(); v4 != nil {
			out = append(out, v4.String())
			continue
		}
		out = append(out, ip.Mask(net.CIDRMask(64, 128)).String()+"/64")
	}
	slices.Sort(out)
	return strings.Join(slices.Compact(out), ","), nil
}
