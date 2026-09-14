package clientip_test

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/clientip"
)

// request builds a request arriving from remote, carrying each value as its
// own X-Forwarded-For header line.
func request(remote string, forwardedFor ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remote
	for _, v := range forwardedFor {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

func resolver(t *testing.T, config clientip.Config) *clientip.Resolver {
	t.Helper()
	r, err := clientip.New(config)
	require.NoError(t, err)
	return r
}

// behindProxies trusts the private range the load balancers live in.
func behindProxies(t *testing.T) *clientip.Resolver {
	t.Helper()
	return resolver(
		t,
		clientip.Config{
			TrustedProxies: []string{"10.0.0.0/8"},
			Header:         "X-Forwarded-For",
		},
	)
}

func requireIP(t *testing.T, want string, got netip.Addr) {
	t.Helper()
	require.True(t, got.IsValid(), "no address resolved, want %s", want)
	require.Equal(t, want, got.String())
}

// With no trusted proxies every forwarding header is a client's claim, so
// none is read.
func TestWithNoTrustedProxiesHeadersAreIgnored(t *testing.T) {
	t.Parallel()

	r := resolver(t, clientip.Config{Header: "X-Forwarded-For"})
	requireIP(
		t,
		"203.0.113.9",
		r.ClientIP(request("203.0.113.9:4000", "1.2.3.4")),
	)
}

func TestHeadersFromAnUntrustedPeerAreIgnored(t *testing.T) {
	t.Parallel()

	requireIP(
		t,
		"203.0.113.9",
		behindProxies(t).ClientIP(request("203.0.113.9:4000", "1.2.3.4")),
	)
}

// Each proxy appends the address it received from; walking back from the
// right, the first address that is not one of ours is the one no client chose.
func TestTheRightmostUntrustedHopIsTheClient(t *testing.T) {
	t.Parallel()

	got := behindProxies(
		t,
	).ClientIP(request("10.0.0.5:80", "198.51.100.1, 203.0.113.9, 10.0.0.4"))
	requireIP(t, "203.0.113.9", got)
}

// The attack this package exists for: the client sends its own
// X-Forwarded-For, the proxy appends the real address, and reading the
// leftmost value would believe the client.
func TestAClientCannotPrependAnAddress(t *testing.T) {
	t.Parallel()

	got := behindProxies(
		t,
	).ClientIP(request("10.0.0.5:80", "1.2.3.4, 203.0.113.9"))
	requireIP(t, "203.0.113.9", got)
}

func TestRepeatedForwardedForHeadersAreReadInOrder(t *testing.T) {
	t.Parallel()

	got := behindProxies(
		t,
	).ClientIP(request("10.0.0.5:80", "198.51.100.1", "203.0.113.9, 10.0.0.4"))
	requireIP(t, "203.0.113.9", got)
}

// A request that only ever crossed our own network is an internal caller.
func TestEveryHopTrustedReturnsTheLeftmost(t *testing.T) {
	t.Parallel()

	requireIP(
		t,
		"10.1.1.1",
		behindProxies(t).ClientIP(request("10.0.0.5:80", "10.1.1.1, 10.2.2.2")),
	)
}

// A hop that does not parse was written by a trusted proxy, so everything to
// its left is unverifiable; the answer is the proxy, never the client's pick.
func TestAMalformedHopStopsAtTheProxy(t *testing.T) {
	t.Parallel()

	for _, hop := range []string{"unknown", "", "not an ip", "999.1.1.1"} {
		got := behindProxies(
			t,
		).ClientIP(request("10.0.0.5:80", "203.0.113.9, "+hop+", 10.0.0.4"))
		requireIP(t, "10.0.0.4", got)
	}
}

func TestHopsWithPortsAndBracketsParse(t *testing.T) {
	t.Parallel()

	r := behindProxies(t)
	requireIP(
		t,
		"203.0.113.9",
		r.ClientIP(request("10.0.0.5:80", "203.0.113.9:1234")),
	)
	requireIP(
		t,
		"2001:db8::1",
		r.ClientIP(request("10.0.0.5:80", "[2001:db8::1]:443")),
	)
	requireIP(
		t,
		"2001:db8::2",
		r.ClientIP(request("10.0.0.5:80", "2001:db8::2")),
	)
}

// An IPv4 address written as IPv4-mapped IPv6 is the same address, both when
// checking trust and in the answer.
func TestMappedIPv4IsUnmapped(t *testing.T) {
	t.Parallel()

	got := behindProxies(
		t,
	).ClientIP(request("[::ffff:10.0.0.5]:80", "::ffff:203.0.113.9"))
	requireIP(t, "203.0.113.9", got)
}

func TestAZoneOnThePeerIsStripped(t *testing.T) {
	t.Parallel()

	r := resolver(
		t,
		clientip.Config{
			TrustedProxies: []string{"fe80::/10"},
			Header:         "X-Forwarded-For",
		},
	)
	requireIP(
		t,
		"203.0.113.9",
		r.ClientIP(request("[fe80::1%eth0]:80", "203.0.113.9")),
	)
}

func TestXRealIPOnlyFromATrustedPeer(t *testing.T) {
	t.Parallel()

	r := resolver(
		t,
		clientip.Config{
			TrustedProxies: []string{"10.0.0.0/8"},
			Header:         "X-Real-IP",
		},
	)

	trusted := request("10.0.0.5:80")
	trusted.Header.Set("X-Real-IP", "203.0.113.9")
	requireIP(t, "203.0.113.9", r.ClientIP(trusted))

	untrusted := request("198.51.100.7:80")
	untrusted.Header.Set("X-Real-IP", "203.0.113.9")
	requireIP(t, "198.51.100.7", r.ClientIP(untrusted))
}

// Two X-Real-IP values mean a proxy appended rather than overwrote, so the
// client's own value may be among them.
func TestARepeatedXRealIPIsIgnored(t *testing.T) {
	t.Parallel()

	r := resolver(
		t,
		clientip.Config{
			TrustedProxies: []string{"10.0.0.0/8"},
			Header:         "X-Real-IP",
		},
	)

	req := request("10.0.0.5:80")
	req.Header.Add("X-Real-IP", "1.2.3.4")
	req.Header.Add("X-Real-IP", "203.0.113.9")
	requireIP(t, "10.0.0.5", r.ClientIP(req))
}

func TestAnUnparseableRemoteAddrIsInvalid(t *testing.T) {
	t.Parallel()

	r := behindProxies(t)
	for _, remote := range []string{"@", "/run/app.sock", ""} {
		require.False(
			t,
			r.ClientIP(request(remote, "203.0.113.9")).IsValid(),
			"%q",
			remote,
		)
	}
}

func TestNewRejects(t *testing.T) {
	t.Parallel()

	for name, config := range map[string]clientip.Config{
		"a malformed prefix":   {TrustedProxies: []string{"10.0.0.0/33"}, Header: "X-Forwarded-For"},
		"garbage":              {TrustedProxies: []string{"proxy.internal"}, Header: "X-Forwarded-For"},
		"every IPv4 address":   {TrustedProxies: []string{"0.0.0.0/0"}, Header: "X-Forwarded-For"},
		"every IPv6 address":   {TrustedProxies: []string{"::/0"}, Header: "X-Forwarded-For"},
		"the Forwarded header": {Header: "Forwarded"},
		"an unknown header":    {Header: "X-Client-IP"},
	} {
		_, err := clientip.New(config)
		require.Error(t, err, name)
	}
}

func TestNewAcceptsBareAddressesAndAnyHeaderCase(t *testing.T) {
	t.Parallel()

	r := resolver(
		t,
		clientip.Config{
			TrustedProxies: []string{"10.0.0.5"},
			Header:         "x-forwarded-for",
		},
	)
	requireIP(
		t,
		"203.0.113.9",
		r.ClientIP(request("10.0.0.5:80", "203.0.113.9")),
	)
	requireIP(t, "10.0.0.6", r.ClientIP(request("10.0.0.6:80", "203.0.113.9")))
}
