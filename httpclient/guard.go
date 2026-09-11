package httpclient

import (
	"errors"
	"net/netip"
	"syscall"

	"github.com/spacemonkeygo/monkit/v3"
)

// ErrBlockedAddress is wrapped by a dial the private-network guard refused, so
// a caller can tell "this URL points inside our network" from a network fault.
var ErrBlockedAddress = errors.New(
	"address blocked by the private-network guard",
)

// The private-network guard is what stops a URL someone else supplied - a
// webhook, a callback, an image to fetch - from making this process connect
// inside its own network: to localhost, to another service, or to the cloud
// metadata endpoint that hands out the instance's credentials. That attack is
// server-side request forgery.
//
// It checks the address being dialed, after DNS resolution, from the dialer's
// Control hook. A check on the URL's host before the request is not a check:
// any name can resolve inside, and with DNS rebinding the same name answers
// with a public address when it is checked and a private one when it is
// connected to. At dial time there is nothing left to resolve.
//
// Two things follow from checking there. Every redirect hop is a new dial, so
// redirects are covered without a redirect policy of their own. And a proxy
// defeats it, because the dial is then to the proxy and the proxy connects
// wherever the URL says - so a guarded transport does not use one.

// blockedPrefixes is everything that is not the public internet. IPv4 prefixes
// are matched after unmapping, so ::ffff:127.0.0.1 is 127.0.0.1.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix(
		"0.0.0.0/8",
	), // "this network"; Linux routes 0.0.0.0 to loopback
	netip.MustParsePrefix("10.0.0.0/8"),    // private
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("127.0.0.0/8"),   // loopback
	netip.MustParsePrefix(
		"169.254.0.0/16",
	), // link-local, and the metadata endpoint
	netip.MustParsePrefix("172.16.0.0/12"),   // private
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("192.168.0.0/16"),  // private
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("224.0.0.0/4"),     // multicast
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, and broadcast
	netip.MustParsePrefix(
		"::/96",
	), // unspecified, loopback, IPv4-compatible
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
	netip.MustParsePrefix("100::/64"),       // discard-only
	netip.MustParsePrefix(
		"2001::/32",
	), // Teredo, which obfuscates the IPv4 inside
	netip.MustParsePrefix("2001:db8::/32"), // documentation
	netip.MustParsePrefix(
		"fc00::/7",
	), // unique local, including AWS's fd00:ec2::254
	netip.MustParsePrefix("fe80::/10"), // link-local
	netip.MustParsePrefix("fec0::/10"), // deprecated site-local
	netip.MustParsePrefix("ff00::/8"),  // multicast
}

// Translation prefixes carry an IPv4 address inside an IPv6 one. The IPv4
// address is what the packet reaches, so it is what gets checked: NAT64 of a
// public address is fine, NAT64 of 127.0.0.1 is not.
var (
	nat64     = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour = netip.MustParsePrefix("2002::/16")
)

// blockedAddr reports whether addr is an internal address.
func blockedAddr(addr netip.Addr) bool {
	// Contains never matches an address with a zone, and a link-local IPv6
	// address from the resolver can carry one.
	addr = addr.WithZone("").Unmap()
	if inner, ok := embeddedIPv4(addr); ok {
		addr = inner
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func embeddedIPv4(addr netip.Addr) (netip.Addr, bool) {
	b := addr.As16()
	switch {
	case nat64.Contains(addr):
		return netip.AddrFrom4([4]byte(b[12:16])), true
	case sixToFour.Contains(addr):
		return netip.AddrFrom4([4]byte(b[2:6])), true
	}
	return netip.Addr{}, false
}

// guard refuses dials to internal addresses outside the allowed prefixes.
type guard struct {
	allowed []netip.Prefix
	nameTag monkit.SeriesTag
}

// guard builds the guard this config asks for, or nil when it allows
// everything. blockByDefault is the constructor's own choice for an empty
// PrivateNetworks: New allows, NewOneShot blocks.
func (config Config) guard(blockByDefault bool) (*guard, error) {
	var block bool
	switch config.PrivateNetworks {
	case "":
		block = blockByDefault
	case "allow":
	case "block":
		block = true
	default:
		return nil, Error.Errorf(
			"private-networks %q is not allow or block",
			config.PrivateNetworks,
		)
	}

	allowed := make([]netip.Prefix, 0, len(config.AllowedNetworks))
	for _, network := range config.AllowedNetworks {
		prefix, err := netip.ParsePrefix(network)
		if err != nil {
			return nil, Error.Errorf(
				"allowed-networks %q is not an address prefix such as 10.0.0.0/8",
				network,
			)
		}
		allowed = append(allowed, prefix.Masked())
	}

	// An allowlist is itself a request to block everything else, so it turns
	// the guard on rather than being silently ignored by a client that allows
	// by default.
	if !block && len(allowed) == 0 {
		return nil, nil
	}
	if config.PrivateNetworks == "allow" {
		return nil, nil
	}

	return &guard{
		allowed: allowed,
		nameTag: monkit.NewSeriesTag("name", config.clientName()),
	}, nil
}

// control is the dialer's Control hook. address is always the resolved
// literal the socket is about to connect to.
func (g *guard) control(_, address string, _ syscall.RawConn) error {
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil {
		// Never expected; refusing is the only safe reading of an address
		// that cannot be checked.
		return g.refuse("unparsable", address)
	}

	addr := addrPort.Addr().WithZone("").Unmap()
	if !blockedAddr(addr) {
		return nil
	}
	for _, prefix := range g.allowed {
		if prefix.Contains(addr) {
			return nil
		}
	}
	return g.refuse("private", addr.String())
}

func (g *guard) refuse(reason, address string) error {
	mon.Event(
		"http_dial_refused",
		g.nameTag,
		monkit.NewSeriesTag("reason", reason),
	)
	return Error.Errorf(
		"refusing to connect to %s: %w",
		address,
		ErrBlockedAddress,
	)
}
