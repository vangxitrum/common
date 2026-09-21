package httpclient_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"

	"gitlab.internal/tuan.quang.tran/aioz-common/httpclient"
)

func TestConfigureH2HealthChecks_Defaults(t *testing.T) {
	h2, err := httpclient.ConfigureH2HealthChecks(&http.Transport{}, 0, 0)
	require.NoError(t, err)

	require.Equal(t, httpclient.DefaultH2HealthCheckInterval, h2.ReadIdleTimeout)
	require.Equal(t, httpclient.DefaultH2PingTimeout, h2.PingTimeout)
}

func TestConfigureH2HealthChecks_ExplicitValues(t *testing.T) {
	h2, err := httpclient.ConfigureH2HealthChecks(&http.Transport{}, 3*time.Second, time.Second)
	require.NoError(t, err)

	require.Equal(t, 3*time.Second, h2.ReadIdleTimeout)
	require.Equal(t, time.Second, h2.PingTimeout)
}

func TestConfigureH2HealthChecks_NegativeFallsBackToDefaults(t *testing.T) {
	h2, err := httpclient.ConfigureH2HealthChecks(&http.Transport{}, -time.Second, -time.Second)
	require.NoError(t, err)

	require.Equal(t, httpclient.DefaultH2HealthCheckInterval, h2.ReadIdleTimeout)
	require.Equal(t, httpclient.DefaultH2PingTimeout, h2.PingTimeout)
}

func TestTransport_HasHealthChecksOn(t *testing.T) {
	transport, err := httpclient.Transport(httpclient.Config{})
	require.NoError(t, err)

	// The settings themselves are unreachable once attached, but attaching is
	// observable: ConfigureTransports refuses to run twice on one transport.
	// An error here means Transport already configured h2 - which is the
	// wiring under test. The values it set are covered by the tests above.
	_, err = http2.ConfigureTransports(transport)
	require.Error(t, err,
		"Transport should have attached an h2 transport with health checks")
}

func TestOneShotTransport_LeavesHealthChecksOff(t *testing.T) {
	// It disables keep-alives, so there is no pooled connection to go stale
	// and nothing for a ping to protect. Configuring h2 here must therefore
	// still be possible: that is what proves the pooled profile is the only
	// one touched.
	transport, err := httpclient.OneShotTransport(httpclient.Config{})
	require.NoError(t, err)
	require.True(t, transport.DisableKeepAlives)

	h2, err := http2.ConfigureTransports(transport)
	require.NoError(t, err, "OneShotTransport should not have attached h2 itself")
	require.Zero(t, h2.ReadIdleTimeout, "and so should not have set any ping interval")
}
