package httpclient_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/httpclient"
)

func TestConfigureH2HealthChecks_Defaults(t *testing.T) {
	transport := &http.Transport{}
	httpclient.ConfigureH2HealthChecks(transport, 0, 0)

	require.NotNil(t, transport.HTTP2)
	require.Equal(
		t,
		httpclient.DefaultH2HealthCheckInterval,
		transport.HTTP2.SendPingTimeout,
	)
	require.Equal(t,
		httpclient.DefaultH2PingTimeout, transport.HTTP2.PingTimeout)
}

func TestConfigureH2HealthChecks_ExplicitValues(t *testing.T) {
	transport := &http.Transport{}
	httpclient.ConfigureH2HealthChecks(transport, 3*time.Second, time.Second)

	require.NotNil(t, transport.HTTP2)
	require.Equal(t, 3*time.Second, transport.HTTP2.SendPingTimeout)
	require.Equal(t, time.Second, transport.HTTP2.PingTimeout)
}

func TestConfigureH2HealthChecks_NegativeFallsBackToDefaults(t *testing.T) {
	transport := &http.Transport{}
	httpclient.ConfigureH2HealthChecks(transport, -time.Second, -time.Second)

	require.NotNil(t, transport.HTTP2)
	require.Equal(
		t,
		httpclient.DefaultH2HealthCheckInterval,
		transport.HTTP2.SendPingTimeout,
	)
	require.Equal(t,
		httpclient.DefaultH2PingTimeout, transport.HTTP2.PingTimeout)
}

func TestConfigureH2HealthChecks_KeepsOtherHTTP2Settings(t *testing.T) {
	// A caller that already set an HTTP2Config keeps it: only the two ping
	// fields are ours.
	transport := &http.Transport{
		HTTP2: &http.HTTP2Config{MaxConcurrentStreams: 42},
	}
	httpclient.ConfigureH2HealthChecks(transport, time.Second, time.Second)

	require.Equal(t, 42, transport.HTTP2.MaxConcurrentStreams)
	require.Equal(t, time.Second, transport.HTTP2.SendPingTimeout)
}

func TestTransport_HasHealthChecksOn(t *testing.T) {
	transport, err := httpclient.Transport(httpclient.Config{
		H2HealthCheckInterval: 7 * time.Second,
		H2PingTimeout:         2 * time.Second,
	})
	require.NoError(t, err)

	require.NotNil(t, transport.HTTP2,
		"Transport should configure h2 health checks")
	require.Equal(t, 7*time.Second, transport.HTTP2.SendPingTimeout)
	require.Equal(t, 2*time.Second, transport.HTTP2.PingTimeout)
}

func TestTransport_ZeroConfigTakesTheDefaults(t *testing.T) {
	transport, err := httpclient.Transport(httpclient.Config{})
	require.NoError(t, err)

	require.NotNil(t, transport.HTTP2)
	require.Equal(
		t,
		httpclient.DefaultH2HealthCheckInterval,
		transport.HTTP2.SendPingTimeout,
	)
	require.Equal(t,
		httpclient.DefaultH2PingTimeout, transport.HTTP2.PingTimeout)
}

func TestOneShotTransport_LeavesHealthChecksOff(t *testing.T) {
	// It disables keep-alives, so there is no pooled connection to go stale
	// and nothing for a ping to protect. Leaving HTTP2 untouched is what
	// proves the pooled profile is the only one configured.
	transport, err := httpclient.OneShotTransport(httpclient.Config{})
	require.NoError(t, err)

	require.True(t, transport.DisableKeepAlives)
	require.Nil(t, transport.HTTP2,
		"OneShotTransport should not configure h2 health checks")
}
