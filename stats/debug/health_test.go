package debug

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/spacemonkeygo/monkit/v3"
	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/stats/testcontext"
)

func runHealthCommand(t *testing.T, defaultURL string, args ...string) error {
	t.Helper()
	cmd := HealthCommand(defaultURL)
	cmd.SetArgs(args)
	cmd.SetOut(&testWriter{t})
	cmd.SetErr(&testWriter{t})
	return cmd.ExecuteContext(t.Context())
}

// The container's HEALTHCHECK runs this against the real debug server:
// distroless has no shell or curl to do it any other way.
func TestHealthCommandPassesAgainstTheDebugServer(t *testing.T) {
	ctx := testcontext.New(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := NewServer(testLogger(t), ln, monkit.NewRegistry(), Config{})
	ctx.Go(func() error { return srv.Run(ctx) })

	require.NoError(
		t,
		runHealthCommand(t, "http://"+ln.Addr().String()+"/health"),
	)
}

func TestHealthCommandFailsOnAnErrorStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "down", http.StatusServiceUnavailable)
		}),
	)
	t.Cleanup(srv.Close)

	require.Error(t, runHealthCommand(t, srv.URL+"/health"))
}

func TestHealthCommandFailsWhenNothingListens(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := ln.Addr().String()
	require.NoError(t, ln.Close())

	require.Error(t, runHealthCommand(t, "http://"+address+"/health"))
}

// A redirect is not followed: a health endpoint that redirects is not the
// health endpoint the check was pointed at.
func TestHealthCommandDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})
	mux.HandleFunc("/elsewhere", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("OK"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	require.Error(t, runHealthCommand(t, srv.URL+"/health"))
}

// A server that accepts and never answers must fail the check within the
// timeout, or the container runtime's own timeout decides instead.
func TestHealthCommandTimesOut(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})

	start := time.Now()
	err = runHealthCommand(
		t,
		"http://"+ln.Addr().String()+"/health",
		"--timeout",
		"200ms",
	)
	require.Error(t, err)
	require.Less(t, time.Since(start), 2*time.Second)
}

func TestHealthCommandURLFlagOverridesTheDefault(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("OK"))
		}),
	)
	t.Cleanup(srv.Close)

	require.NoError(
		t,
		runHealthCommand(
			t,
			"http://127.0.0.1:1/health",
			"--url",
			srv.URL+"/health",
		),
	)
	require.Error(t, runHealthCommand(t, "http://127.0.0.1:1/health"))
}
