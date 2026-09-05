package tools

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// This file is REQ-SEC-05: the SSRF guard.
//
// The requirement says the guard validates "at DNS resolution time AND TCP
// connection time", and the conjunction is the whole thing. A guard that
// checks only at resolution has a TOCTOU window a DNS rebind walks straight
// through: the attacker's name resolves to a public address for the check and
// to 169.254.169.254 for the connect, and the SDK fetches the cloud instance
// metadata credentials on their behalf.
//
// The connect-time check is therefore not belt-and-braces. It is the one that
// actually holds, because it runs on the concrete address the kernel is about
// to connect to. The resolution-time check exists to fail fast and to say
// something useful about why.

// ErrBlockedAddress is returned for an address the guard refuses.
var ErrBlockedAddress = errors.New("tools: address is not permitted by the SSRF guard")

// ErrSchemeNotAllowed is REQ-SEC-09: HTTPS only unless HTTP is opted into.
var ErrSchemeNotAllowed = errors.New("tools: only https:// is allowed; set AllowHTTP to permit http://")

// SSRFGuard builds a transport whose every connection is validated.
type SSRFGuard struct {
	// AllowHTTP is REQ-SEC-09's opt-in. Off by default.
	AllowHTTP bool
	// Resolve is injectable so a test can decide what a name resolves to
	// without owning DNS. Nil means net.DefaultResolver.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// DialAddr opens the socket to an ALREADY-VALIDATED address. Nil means a
	// real dialer carrying the connect-time re-check. It is injectable so a
	// test can land a validated public address on a local test server; the
	// validation itself is never injectable.
	DialAddr func(ctx context.Context, network, addr string) (net.Conn, error)
	// TLSClientConfig is passed through to the transport, for a deployment
	// behind a private CA. It does not weaken the guard: the address check
	// happens at dial time, before any handshake.
	TLSClientConfig *tls.Config
	Timeout         time.Duration
}

// Transport returns an http.Transport that dials only through the guard.
//
// Proxies are deliberately NOT honoured: an http_proxy in the environment
// would route every request through a host the guard never validated, which
// silently disables it. A deployment that needs a proxy needs a guard that
// validates the proxy, and that is a different thing than this.
func (g *SSRFGuard) Transport() *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           g.DialContext,
		TLSClientConfig:       g.TLSClientConfig,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// DialContext validates and connects.
func (g *SSRFGuard) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	addrs, err := g.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("tools: %q resolved to no addresses", host)
	}

	// EVERY resolved address must be permitted, not merely the one we would
	// have picked. A name resolving to both a public address and a private one
	// is the rebinding pattern itself, and "connect to the first allowed one"
	// hands the attacker a retry loop.
	for _, a := range addrs {
		if blocked, why := BlockedAddress(a); blocked {
			return nil, fmt.Errorf("%w: %s resolved to %s (%s)", ErrBlockedAddress, host, a, why)
		}
	}

	target := net.JoinHostPort(addrs[0].String(), port)
	if g.DialAddr != nil {
		return g.DialAddr(ctx, network, target)
	}

	d := &net.Dialer{
		Timeout: g.dialTimeout(),
		// Control runs with the concrete address immediately before connect.
		// This is the check that closes the rebinding window; everything above
		// only makes the failure legible.
		Control: func(_, address string, _ syscall.RawConn) error {
			h, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(h)
			if err != nil {
				return err
			}
			if blocked, why := BlockedAddress(ip); blocked {
				return fmt.Errorf("%w: connect to %s (%s)", ErrBlockedAddress, ip, why)
			}
			return nil
		},
	}
	return d.DialContext(ctx, network, target)
}

func (g *SSRFGuard) dialTimeout() time.Duration {
	if g.Timeout > 0 {
		return g.Timeout
	}
	return 10 * time.Second
}

func (g *SSRFGuard) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{ip}, nil
	}
	if g.Resolve != nil {
		return g.Resolve(ctx, host)
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// BlockedAddress classifies one address and says why.
//
// The IPv4-MAPPED IPv6 case is the reason this unwraps before classifying.
// `::ffff:169.254.169.254` is a perfectly ordinary IPv6 address to any check
// that only asks netip whether it is loopback or private — those predicates
// answer for the IPv6 form, which is neither — and it routes to the IPv4 cloud
// metadata endpoint. Unmapping first is what makes the classification mean
// what it says.
func BlockedAddress(a netip.Addr) (bool, string) {
	if !a.IsValid() {
		return true, "invalid address"
	}
	if a.Is4In6() {
		a = a.Unmap()
	}

	switch {
	case a.IsUnspecified():
		return true, "unspecified"
	case a.IsLoopback():
		return true, "loopback"
	case a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast():
		// 169.254.0.0/16 and fe80::/10. This is where the cloud instance
		// metadata endpoint lives, and it is the single highest-value target
		// an SSRF has.
		return true, "link-local"
	case a.IsInterfaceLocalMulticast(), a.IsMulticast():
		return true, "multicast"
	case a.IsPrivate():
		return true, "private"
	}

	for _, r := range reservedRanges {
		if r.prefix.Contains(a) {
			return true, r.why
		}
	}
	return false, ""
}

type reserved struct {
	prefix netip.Prefix
	why    string
}

// reservedRanges covers what netip's own predicates do not.
//
// Each entry is a range that is routable-looking and must not be reachable:
// netip.Addr.IsPrivate covers only RFC1918 and fc00::/7, so carrier-grade NAT,
// the benchmarking range and the documentation ranges all pass it.
var reservedRanges = []reserved{
	{netip.MustParsePrefix("100.64.0.0/10"), "carrier-grade NAT"},
	{netip.MustParsePrefix("192.0.0.0/24"), "IETF protocol assignments"},
	{netip.MustParsePrefix("192.0.2.0/24"), "documentation (TEST-NET-1)"},
	{netip.MustParsePrefix("198.18.0.0/15"), "benchmarking"},
	{netip.MustParsePrefix("198.51.100.0/24"), "documentation (TEST-NET-2)"},
	{netip.MustParsePrefix("203.0.113.0/24"), "documentation (TEST-NET-3)"},
	{netip.MustParsePrefix("240.0.0.0/4"), "reserved for future use"},
	{netip.MustParsePrefix("255.255.255.255/32"), "broadcast"},
	{netip.MustParsePrefix("64:ff9b::/96"), "NAT64"},
	{netip.MustParsePrefix("100::/64"), "discard-only"},
	{netip.MustParsePrefix("2001:db8::/32"), "documentation"},
	{netip.MustParsePrefix("2002::/16"), "6to4 relay"},
}
