package httpclient

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

// The ranges the guard refuses, including the spellings that hide an internal
// IPv4 address inside an IPv6 one.
func TestWhichAddressesAreBlocked(t *testing.T) {
	t.Parallel()

	blocked := []string{
		"127.0.0.1",       // loopback
		"10.1.2.3",        // private
		"172.16.0.1",      // private
		"192.168.0.1",     // private
		"169.254.169.254", // link-local: the cloud metadata endpoint
		"100.64.0.1",      // carrier-grade NAT
		"0.0.0.0",         // unspecified, which Linux routes to loopback
		"0.1.2.3",         // "this network"
		"192.0.0.1",       // IETF protocol assignments
		"192.0.2.1",       // documentation
		"198.18.0.1",      // benchmarking
		"224.0.0.1",       // multicast
		"240.0.0.1",       // reserved
		"255.255.255.255", // broadcast
		"::1",             // loopback
		"::",              // unspecified
		"fe80::1",         // link-local
		"fc00::1",         // unique local
		"fd00:ec2::254",   // AWS's IPv6 metadata endpoint, inside fc00::/7
		"fec0::1",         // deprecated site-local
		"ff02::1",         // multicast
		"100::1",          // discard-only
		"2001:db8::1",     // documentation
		"2001::1",         // Teredo: the IPv4 address inside is obfuscated
		"::ffff:127.0.0.1",
		"::ffff:10.0.0.1",
		"64:ff9b::7f00:1",    // NAT64 of 127.0.0.1
		"64:ff9b::a9fe:a9fe", // NAT64 of 169.254.169.254
		"64:ff9b:1::1",       // local-use NAT64
		"2002:7f00:1::1",     // 6to4 of 127.0.0.1
		"2002:a9fe:a9fe::1",  // 6to4 of 169.254.169.254
	}
	allowed := []string{
		"8.8.8.8",
		"1.1.1.1",
		"172.32.0.1",  // just past 172.16.0.0/12
		"100.128.0.1", // just past 100.64.0.0/10
		"2606:4700:4700::1111",
		"::ffff:8.8.8.8",
		"64:ff9b::808:808", // NAT64 of a public address
		"2002:808:808::1",  // 6to4 of a public address
	}

	for _, s := range blocked {
		require.True(
			t,
			blockedAddr(netip.MustParseAddr(s)),
			"%s should be blocked",
			s,
		)
	}
	for _, s := range allowed {
		require.False(
			t,
			blockedAddr(netip.MustParseAddr(s)),
			"%s should be allowed",
			s,
		)
	}
}
