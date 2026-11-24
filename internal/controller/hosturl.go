package controller

import (
	"net"
	"net/netip"
	"strings"
)

// BestBaseURL returns the URL other devices should use to reach a hub listening on
// listenAddr: publicURL when set; otherwise the listen address itself when it names
// a specific host, or (for a wildcard listener) this machine's best LAN address,
// preferring private LAN ranges, then Tailscale (100.64.0.0/10), then anything else.
// loopbackOnly reports a listener only this machine can reach.
func BestBaseURL(listenAddr, publicURL string) (base string, loopbackOnly bool) {
	if u := strings.TrimRight(strings.TrimSpace(publicURL), "/"); u != "" {
		return u, false
	}
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		host, port = listenAddr, "8420"
	}
	if host == "localhost" {
		return "http://localhost:" + port, true
	}
	if ip := net.ParseIP(host); ip != nil && !ip.IsUnspecified() {
		return "http://" + net.JoinHostPort(host, port), ip.IsLoopback()
	}
	if host != "" && net.ParseIP(host) == nil {
		return "http://" + net.JoinHostPort(host, port), false
	}
	if lan := bestLANAddr(interfaceAddrs()); lan.IsValid() {
		return "http://" + net.JoinHostPort(lan.String(), port), false
	}
	return "http://127.0.0.1:" + port, true
}

// interfaceAddrs lists the addresses of this machine's up, non-loopback interfaces.
func interfaceAddrs() []netip.Addr {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if pfx, err := netip.ParsePrefix(a.String()); err == nil {
				out = append(out, pfx.Addr().Unmap())
			}
		}
	}
	return out
}

var tailscaleRange = netip.MustParsePrefix("100.64.0.0/10")

// bestLANAddr picks the address a phone on the same network is most likely to reach:
// IPv4 private LAN, then Tailscale, then other global IPv4, then IPv6 (no link-local).
func bestLANAddr(addrs []netip.Addr) netip.Addr {
	rank := func(a netip.Addr) int {
		switch {
		case a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified() || a.IsMulticast():
			return 0
		case a.Is4() && a.IsPrivate():
			return 4
		case a.Is4() && tailscaleRange.Contains(a):
			return 3
		case a.Is4():
			return 2
		default:
			return 1
		}
	}
	var best netip.Addr
	for _, a := range addrs {
		if r := rank(a); r > 0 && (!best.IsValid() || r > rank(best)) {
			best = a
		}
	}
	return best
}
