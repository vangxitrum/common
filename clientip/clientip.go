// Package clientip resolves the address a request really came from, for a
// service behind reverse proxies.
//
// X-Forwarded-For is a request header like any other, so any client can send
// one. Each proxy in front of the service appends the address it received the
// request from, which means only the entries our own proxies wrote can be
// believed. Walking the list from the right and stopping at the first address
// that is not one of our proxies gives the one address the client could not
// choose. Reading it any other way - the leftmost entry, or every entry when
// no proxy list is configured - lets a client pick the address it is rate
// limited and logged by, which is the bug this package exists for: gin trusts
// every proxy by default, and echo reads the header with no trust list at all.
//
// With no trusted proxies configured, no header is read and the answer is the
// connection's own address.
package clientip

import (
	"net/http"
	"net/netip"
	"strings"

	"github.com/zeebo/errs/v2"
)

// Error tags every failure this package produces.
var Error = errs.Tag("clientip")

const (
	headerForwardedFor = "X-Forwarded-For"
	headerRealIP       = "X-Real-Ip" // canonical form of X-Real-IP
)

// Config names the proxies in front of the service and the header they set.
type Config struct {
	TrustedProxies []string `help:"CIDRs or addresses of the reverse proxies whose forwarding header is believed; empty believes no header and uses the connection address" default:""`
	Header         string   `help:"forwarding header the trusted proxies set: X-Forwarded-For or X-Real-IP"                                                                 default:"X-Forwarded-For"`
}

// Resolver finds a request's client address. It is safe for concurrent use.
type Resolver struct {
	trusted []netip.Prefix
	header  string
}

// New validates config and returns a Resolver.
//
// It refuses a prefix that covers every address (0.0.0.0/0, ::/0): trusting
// every peer makes the header entirely client-controlled, which is the mistake
// this package is here to remove. It refuses the RFC 7239 Forwarded header,
// which no proxy in front of our services emits and whose quoted, obfuscated
// "for=" grammar would be attack surface with no caller.
func New(config Config) (*Resolver, error) {
	header := http.CanonicalHeaderKey(strings.TrimSpace(config.Header))
	switch header {
	case "":
		header = headerForwardedFor
	case headerForwardedFor, headerRealIP:
	case "Forwarded":
		return nil, Error.Errorf(
			"the RFC 7239 Forwarded header is not supported; configure the proxy to send X-Forwarded-For",
		)
	default:
		return nil, Error.Errorf(
			"header %q is not X-Forwarded-For or X-Real-IP",
			config.Header,
		)
	}

	trusted := make([]netip.Prefix, 0, len(config.TrustedProxies))
	for _, entry := range config.TrustedProxies {
		prefix, err := parseTrusted(strings.TrimSpace(entry))
		if err != nil {
			return nil, err
		}
		trusted = append(trusted, prefix)
	}

	return &Resolver{trusted: trusted, header: header}, nil
}

// parseTrusted reads a CIDR prefix, or a bare address as a single-address
// prefix.
func parseTrusted(entry string) (netip.Prefix, error) {
	var prefix netip.Prefix
	if strings.Contains(entry, "/") {
		parsed, err := netip.ParsePrefix(entry)
		if err != nil {
			return netip.Prefix{}, Error.Errorf(
				"trusted proxy %q is not a CIDR prefix",
				entry,
			)
		}
		prefix = parsed.Masked()
	} else {
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			return netip.Prefix{}, Error.Errorf(
				"trusted proxy %q is not an address or a CIDR prefix",
				entry,
			)
		}
		addr = addr.Unmap().WithZone("")
		prefix = netip.PrefixFrom(addr, addr.BitLen())
	}

	if prefix.Bits() == 0 {
		return netip.Prefix{}, Error.Errorf(
			"trusted proxy %q trusts every address, which lets any client choose its own",
			entry,
		)
	}
	return prefix, nil
}

// ClientIP returns the address the request came from.
//
// The result is invalid (!IsValid) only when the connection's own address is
// not an IP address, as on a unix socket; a caller keying anything on it
// should treat that as an error rather than as one shared key.
func (r *Resolver) ClientIP(req *http.Request) netip.Addr {
	peer, ok := parseRemote(req.RemoteAddr)
	if !ok {
		return netip.Addr{}
	}
	if !r.isTrusted(peer) {
		// Only a proxy of ours may speak for someone else.
		return peer
	}

	if r.header == headerRealIP {
		values := req.Header.Values(headerRealIP)
		if len(values) != 1 {
			// More than one means a proxy appended instead of overwriting, so
			// the client's own value may be among them.
			return peer
		}
		if addr, ok := parseHop(values[0]); ok {
			return addr
		}
		return peer
	}

	return r.walkForwardedFor(req.Header.Values(headerForwardedFor), peer)
}

// walkForwardedFor reads the hops from the right: every header line in
// order, and each line from its last comma back. Splitting from the right
// means an attacker-sized left side is never split or allocated.
func (r *Resolver) walkForwardedFor(
	lines []string,
	peer netip.Addr,
) netip.Addr {
	last := peer // the last trusted address on the path
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		for {
			comma := strings.LastIndexByte(line, ',')
			addr, ok := parseHop(line[comma+1:])
			if !ok {
				// A trusted proxy wrote this hop, so everything to its left
				// is unverifiable. The proxy is the answer that the client
				// did not get to choose.
				return last
			}
			if !r.isTrusted(addr) {
				return addr
			}
			last = addr
			if comma < 0 {
				break
			}
			line = line[:comma]
		}
	}
	// Every hop was one of ours: an internal caller, the leftmost of them.
	return last
}

func (r *Resolver) isTrusted(addr netip.Addr) bool {
	for _, prefix := range r.trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// parseRemote reads http.Request.RemoteAddr, which is "ip:port" for a TCP
// connection.
func parseRemote(remote string) (netip.Addr, bool) {
	if addrPort, err := netip.ParseAddrPort(remote); err == nil {
		return normalize(addrPort.Addr()), true
	}
	if addr, err := netip.ParseAddr(remote); err == nil {
		return normalize(addr), true
	}
	return netip.Addr{}, false
}

// parseHop reads one forwarding hop: an address, possibly with a port, and
// for IPv6 possibly in brackets.
func parseHop(hop string) (netip.Addr, bool) {
	hop = strings.TrimSpace(hop)
	if addr, err := netip.ParseAddr(hop); err == nil {
		return normalize(addr), true
	}
	if addrPort, err := netip.ParseAddrPort(hop); err == nil {
		return normalize(addrPort.Addr()), true
	}
	if strings.HasPrefix(hop, "[") && strings.HasSuffix(hop, "]") {
		if addr, err := netip.ParseAddr(hop[1 : len(hop)-1]); err == nil {
			return normalize(addr), true
		}
	}
	return netip.Addr{}, false
}

// normalize makes one address one value: an IPv4-mapped IPv6 address is its
// IPv4 address, and a zone - which netip.Prefix.Contains never matches - is
// dropped.
func normalize(addr netip.Addr) netip.Addr {
	return addr.Unmap().WithZone("")
}
