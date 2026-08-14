// Package hostpolicy answers one question in one place: may this wallet open a
// connection to that host?
//
// The answer used to be written twice — once in the dialer that makes the
// connection, once in the settings validator that decides whether a URL can be
// saved — and the two drifted apart. The validator's copy had only the net.IP
// predicates, so every IPv6 spelling of a local address listed below saved
// cleanly through the settings form and was then refused at connect time. A
// rule stated in two places is two rules, and users meet the weaker one first.
package hostpolicy

import (
	"net"
	"strings"
)

// PublicIP reports whether an address is one the wallet may connect to.
//
// The net.IP predicates are necessary and not sufficient, which is what
// unsafeSpecialNetworks is for.
func PublicIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() ||
		ip.IsUnspecified() {
		return false
	}
	for _, special := range unsafeSpecialNetworks {
		if special.Contains(ip) {
			return false
		}
	}
	return true
}

// LocalName reports whether a hostname reaches this machine, or something on
// the same LAN, without ever looking like an address.
//
// These two spellings have to be refused by name because that is the only form
// they take: neither parses as an IP literal, so no address predicate ever sees
// them. Every other name is left to whoever resolves it.
func LocalName(host string) bool {
	lowered := strings.ToLower(host)
	return lowered == "localhost" || strings.HasSuffix(lowered, ".local")
}

// PublicHost judges a host as written — a name or an IP literal — without
// resolving anything.
//
// This is the cheap half of the policy, meant for the moment a URL is being
// saved: it catches what can be read straight off the string, so a bad value
// fails while the person who typed it is still looking at the form rather than
// an hour later in a log line nobody reads. A name that is not a literal is
// reported public here and settled by the dialer, which resolves it and applies
// PublicIP to the address it is about to connect to — no validation function
// does DNS.
func PublicHost(host string) bool {
	if LocalName(host) {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return true
	}
	return PublicIP(ip)
}

// unsafeSpecialNetworks covers what the net.IP predicates do not.
//
// The IPv6 entries matter more than they look. Go's To4 converts only the
// IPv4-mapped form, so ::127.0.0.1 — the deprecated IPv4-compatible spelling of
// loopback — is not seen as loopback by IsLoopback and passes IsGlobalUnicast.
// NAT64 and 6to4 are the same trick with an extra step: both embed an arbitrary
// IPv4 address, including a private one, in something that looks like an
// ordinary global address.
var unsafeSpecialNetworks = mustCIDRs(
	"100.64.0.0/10",   // shared address space, often routes into carrier infrastructure
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // documentation
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // documentation
	"203.0.113.0/24",  // documentation
	"::/96",           // IPv4-compatible IPv6, deprecated and not decoded by To4
	"64:ff9b::/96",    // NAT64
	"64:ff9b:1::/48",  // local-use NAT64
	"100::/64",        // discard-only
	"2001::/23",       // IETF protocol assignments, including Teredo
	"2001:db8::/32",   // documentation
	"2002::/16",       // 6to4
)

func mustCIDRs(values ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, parsed, err := net.ParseCIDR(value)
		if err != nil {
			// The list above is a constant, so a typo in it is a build-time
			// mistake. Skipping the bad entry instead would narrow the policy at
			// run time without saying so.
			panic(err)
		}
		out = append(out, parsed)
	}
	return out
}
