package httpclient_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/config"
	"gitlab.internal/tuan.quang.tran/aioz-common/httpclient"
	"gitlab.internal/tuan.quang.tran/aioz-common/stats/testcontext"
)

// Every server in these tests listens on 127.0.0.1, which is exactly the kind
// of address the guard exists to refuse - so each test either expects it
// refused or exempts it on purpose.

func closeBody(resp *http.Response) {
	if resp != nil {
		_ = resp.Body.Close()
	}
}

// NewOneShot is the client a service reaches for when the address came from
// somewhere else - a webhook, a callback URL - so it refuses internal addresses
// without being asked.
func TestOneShotRefusesAPrivateAddressByDefault(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	handler := newScript(status(http.StatusOK))
	server := httptest.NewServer(handler)
	defer server.Close()

	client, err := httpclient.NewOneShot(testLogger(t), testConfig(t))
	require.NoError(t, err)

	resp, err := get(t, ctx, client, server.URL)
	closeBody(resp)

	require.ErrorIs(t, err, httpclient.ErrBlockedAddress)
	require.Zero(t, handler.calls(), "the request must not reach the server")
}

// New is for a dependency named in configuration, which is usually internal,
// so it keeps connecting anywhere unless told otherwise.
func TestNewAllowsAPrivateAddressByDefault(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	server := httptest.NewServer(newScript(status(http.StatusOK)))
	defer server.Close()

	client, err := httpclient.New(testLogger(t), testConfig(t))
	require.NoError(t, err)

	resp, err := get(t, ctx, client, server.URL)
	require.NoError(t, err)
	defer closeBody(resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestNewRefusesAPrivateAddressWhenAsked(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	handler := newScript(status(http.StatusOK))
	server := httptest.NewServer(handler)
	defer server.Close()

	config := testConfig(t)
	config.PrivateNetworks = "block"

	client, err := httpclient.New(testLogger(t), config)
	require.NoError(t, err)

	resp, err := get(t, ctx, client, server.URL)
	closeBody(resp)

	require.ErrorIs(t, err, httpclient.ErrBlockedAddress)
	require.Zero(t, handler.calls())
}

// The check is on the address the name resolved to, at the moment of dialing,
// not on the text of the URL. A hostname check would be defeated by any name
// that points inside - or by DNS rebinding, where the same name answers public
// for the check and private for the connection.
func TestTheResolvedAddressIsWhatIsChecked(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	handler := newScript(status(http.StatusOK))
	server := httptest.NewServer(handler)
	defer server.Close()

	client, err := httpclient.NewOneShot(testLogger(t), testConfig(t))
	require.NoError(t, err)

	byName := strings.Replace(server.URL, "127.0.0.1", "localhost", 1)
	resp, err := get(t, ctx, client, byName)
	closeBody(resp)

	require.ErrorIs(t, err, httpclient.ErrBlockedAddress)
	require.Zero(t, handler.calls())
}

func TestAnAllowedNetworkIsExempt(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	server := httptest.NewServer(newScript(status(http.StatusOK)))
	defer server.Close()

	config := testConfig(t)
	config.AllowedNetworks = []string{"127.0.0.0/8"}

	client, err := httpclient.NewOneShot(testLogger(t), config)
	require.NoError(t, err)

	resp, err := get(t, ctx, client, server.URL)
	require.NoError(t, err)
	defer closeBody(resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestAllowOverridesTheOneShotDefault(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	server := httptest.NewServer(newScript(status(http.StatusOK)))
	defer server.Close()

	config := testConfig(t)
	config.PrivateNetworks = "allow"

	client, err := httpclient.NewOneShot(testLogger(t), config)
	require.NoError(t, err)

	resp, err := get(t, ctx, client, server.URL)
	require.NoError(t, err)
	defer closeBody(resp)
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// The classic SSRF: a public address the guard lets through answers with a
// redirect to the cloud metadata endpoint. Every hop dials again, so every hop
// is checked. The refusal happens before the connect, so this test never
// touches the network.
func TestARedirectToAPrivateAddressIsRefused(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	server := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(
				w,
				r,
				"http://169.254.169.254/latest/meta-data/",
				http.StatusFound,
			)
		}),
	)
	defer server.Close()

	config := testConfig(t)
	// Stands in for the public address the first hop would really be.
	config.AllowedNetworks = []string{"127.0.0.1/32"}

	client, err := httpclient.NewOneShot(testLogger(t), config)
	require.NoError(t, err)

	resp, err := get(t, ctx, client, server.URL)
	closeBody(resp)

	require.ErrorIs(t, err, httpclient.ErrBlockedAddress)
	require.Contains(t, err.Error(), "169.254.169.254")
}

// A refused address will be refused again, so it is not a fault to retry.
func TestABlockedDialIsNotRetried(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	server := httptest.NewServer(newScript(status(http.StatusOK)))
	defer server.Close()

	config := testConfig(t)

	client, err := httpclient.NewOneShot(testLogger(t), config)
	require.NoError(t, err)

	resp, err := doGet(t, ctx, fastRetry(t), client, server.URL)
	closeBody(resp)

	require.ErrorIs(t, err, httpclient.ErrBlockedAddress)
	require.Equal(t, 1.0,
		seriesValue(t, "http_dial_refused", config.Name, "total",
			map[string]string{"reason": "private"}),
		"one refusal, not one per retry")
}

func TestPrivateNetworkConfigIsValidated(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		mutate func(*httpclient.Config)
		want   string
	}{
		{
			name:   "an unknown mode",
			mutate: func(c *httpclient.Config) { c.PrivateNetworks = "deny" },
			want:   "private-networks",
		},
		{
			name:   "a network that is not a prefix",
			mutate: func(c *httpclient.Config) { c.AllowedNetworks = []string{"10.0.0.1"} },
			want:   "allowed-networks",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			config := testConfig(t)
			tc.mutate(&config)

			err := config.Validate()
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// Every other test builds Config by hand. A service gets it from config.Bind,
// so this checks the bound defaults: an empty allowed-networks must bind to no
// prefixes rather than to one empty string, which would fail Validate and stop
// every service that binds this struct from starting.
func TestTheBoundDefaultsBuildBothClients(t *testing.T) {
	t.Parallel()

	var cfg httpclient.Config
	config.BindFlags(
		pflag.NewFlagSet(t.Name(), pflag.ContinueOnError),
		&cfg,
		config.UseReleaseDefaults(),
	)

	require.Empty(t, cfg.PrivateNetworks)
	require.Empty(t, cfg.AllowedNetworks)

	_, err := httpclient.New(testLogger(t), cfg)
	require.NoError(t, err)

	_, err = httpclient.NewOneShot(testLogger(t), cfg)
	require.NoError(t, err)
}
