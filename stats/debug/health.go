package debug

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"
)

// DefaultHealthTimeout bounds one health check when --timeout is not given.
const DefaultHealthTimeout = 3 * time.Second

// HealthCommand returns a "healthcheck" subcommand that GETs the debug
// server's /health and exits non-zero unless it answers 200.
//
// It exists for the container's HEALTHCHECK. A distroless image has no shell
// and no curl, so the only program that can check the service is the service:
//
//	HEALTHCHECK CMD ["/usr/local/bin/service", "healthcheck"]
//
//	root.AddCommand(debug.HealthCommand("http://127.0.0.1:9002/health"))
//
// defaultURL must match the debug server's address. The debug address
// defaults to 127.0.0.1:0, a random port, which no health check can find:
// a service that uses this sets a fixed one.
func HealthCommand(defaultURL string) *cobra.Command {
	var (
		url     string
		timeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "Exit 0 when the debug server's /health answers 200",
		Long: "Exit 0 when the debug server's /health answers 200, and 1 otherwise.\n" +
			"For a container HEALTHCHECK, which has no curl in a distroless image.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return CheckHealth(cmd.Context(), url, timeout)
		},
	}
	cmd.Flags().StringVar(&url, "url", defaultURL, "health endpoint to check")
	cmd.Flags().
		DurationVar(&timeout, "timeout", DefaultHealthTimeout, "give up after this long")
	return cmd
}

// CheckHealth GETs url and returns nil only for a 200 within timeout.
//
// It ignores proxy settings, which in a container would route a loopback
// check somewhere else, and does not follow redirects: an endpoint that
// redirects is not the endpoint the check was pointed at.
func CheckHealth(ctx context.Context, url string, timeout time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Error.Wrap(err)
	}
	client := &http.Client{
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return Error.Wrap(err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))

	if resp.StatusCode != http.StatusOK {
		return Error.Errorf("%s answered %s", url, resp.Status)
	}
	return nil
}
