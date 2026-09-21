package telemetry

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/golang/snappy"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/spacemonkeygo/monkit/v3"
	"github.com/zeebo/errs/v2"

	"gitlab.internal/tuan.quang.tran/aioz-common/cycle"
	"gitlab.internal/tuan.quang.tran/aioz-common/httpclient"
	"gitlab.internal/tuan.quang.tran/aioz-common/stats/monitor"
)

var (
	// Error is the telemetry package's error tag.
	Error = errs.Tag("telemetry")

	mon = monkit.Package()
)

// pushTransport is the default transport plus HTTP/2 health checks.
//
// Without them a connection broken with no TCP reset - which is what an
// expired NAT entry or a restarted tunnel looks like - stays pooled, and every
// push multiplexed onto it hangs for the full httpTimeout. Since the loop
// reuses the same connection, that is not one slow push but a blackout: the
// receiver stops hearing from this process until the path happens to recover,
// while a healthy connection completes a push in tens of milliseconds.
func pushTransport() *http.Transport {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil // never happens; nil means the default, which still works
	}

	transport = transport.Clone()
	httpclient.ConfigureH2HealthChecks(transport, 0, 0)

	return transport
}

// httpTimeout bounds every remote_write POST so a slow or hung receiver can
// never block the push loop past one interval.
const httpTimeout = 10 * time.Second

// batchSeries caps how many series are sent per remote_write request. Kept
// well under typical receiver-side limits (Prometheus/Cortex/Mimir/
// VictoriaMetrics commonly cap in the low thousands) so a service with a large
// registry still pushes successfully, just as multiple requests.
const batchSeries = 1500

// Client periodically walks a monkit registry and pushes the collected series
// to a Prometheus remote_write receiver over HTTP.
//
// Unlike the pull-based /metrics endpoint in stats/debug, Client never applies
// monkit's DeltaTransformer: every report reads the registry's raw cumulative
// values, because remote_write receivers expect monotonically non-decreasing
// counters and compute rate()/increase() themselves. Sending deltas instead
// would make those queries meaningless.
type Client struct {
	log      *slog.Logger
	http     *http.Client
	registry *monkit.Registry
	gatherer prometheus.Gatherer
	cfg      Config

	extraLabels []label

	loop *cycle.Cycle
}

// Option customizes a Client.
type Option func(*Client)

// WithGatherer pushes everything g reports alongside the monkit registry, for
// services instrumented with prometheus/client_golang as well as monkit.
// Passing this explicitly is equivalent to Config.IncludeDefaultGatherer
// except that the source is chosen by the caller rather than being the
// package-global default registry.
func WithGatherer(g prometheus.Gatherer) Option {
	return func(c *Client) { c.gatherer = g }
}

// WithHTTPClient replaces the HTTP client used for pushes. Mainly for tests
// and for callers that need a proxy or custom transport.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// NewClient builds a telemetry push Client for cfg and registry. A nil
// registry means monkit.Default.
//
// NewClient also attaches monkit's process and runtime statistics to the
// registry through stats/monitor.Register (idempotent per registry), so
// goroutine, memory, GC and fd counts ride along in every push without the
// caller wiring them up separately.
func NewClient(log *slog.Logger, cfg Config, registry *monkit.Registry, opts ...Option) (*Client, error) {
	if registry == nil {
		registry = monkit.Default
	}
	if log == nil {
		return nil, Error.Errorf("logger is required")
	}

	monitor.Register(registry)

	instance := cfg.Instance
	if instance == "" {
		instance = resolveInstanceID()
	}

	client := &Client{
		log:      log,
		http:     &http.Client{Timeout: httpTimeout, Transport: pushTransport()},
		registry: registry,
		cfg:      cfg,
		// Held pre-built and pre-sorted: these are appended to every series,
		// and re-sorting the full label set per sample is the hot path of a
		// push with thousands of series. An empty env is dropped here rather
		// than pushed as "", so a service that does not set it keeps the
		// label set it had before.
		extraLabels: nonEmptyLabels(
			label{Name: "app", Value: cfg.App},
			label{Name: "env", Value: cfg.Env},
			label{Name: "instance", Value: instance},
			label{Name: "role", Value: cfg.Role},
		),
		loop: cycle.New(cfg.Interval),
	}

	for _, opt := range opts {
		opt(client)
	}
	if client.gatherer == nil && cfg.IncludeDefaultGatherer {
		client.gatherer = prometheus.DefaultGatherer
	}

	return client, nil
}

// Run pushes metrics every cfg.Interval until ctx is canceled or Close is
// called. Run never exits because of a push failure: report errors are logged
// and swallowed, so one bad interval (receiver down, timeout) never tears down
// the lifecycle group around it. Run returns immediately, without pushing, if
// the client is disabled (cfg.URL is empty).
func (client *Client) Run(ctx context.Context) (err error) {
	defer mon.Task()(&ctx)(&err)

	if !client.cfg.Enabled() {
		return nil
	}

	return client.loop.Run(ctx, func(ctx context.Context) error {
		if err := client.report(ctx); err != nil {
			client.log.WarnContext(ctx, "telemetry push failed", slog.Any("error", err))
		}
		return nil
	})
}

// Close stops Run and waits for it to return.
func (client *Client) Close() error {
	client.loop.Close()
	return nil
}

// report walks the sources once, applies the cardinality filter, and pushes
// the result in one or more batched requests. Every batch is attempted even if
// an earlier one failed; the returned error describes all that did.
func (client *Client) report(ctx context.Context) (err error) {
	defer mon.Task()(&ctx)(&err)

	series := client.collect()
	if len(series) == 0 {
		return nil
	}

	var errGroup errs.Group
	for start := 0; start < len(series); start += batchSeries {
		end := min(start+batchSeries, len(series))
		if pushErr := client.push(ctx, series[start:end]); pushErr != nil {
			errGroup.Add(pushErr)
		}
	}
	return errGroup.Err()
}

// collect builds one series per sample from every configured source, stamped
// with a single collection timestamp so all of a push's samples line up.
func (client *Client) collect() []timeSeries {
	now := time.Now().UnixMilli()

	series := client.collectMonkit(now)
	if client.gatherer != nil {
		series = append(series, client.collectGatherer(now)...)
	}
	return series
}

// collectMonkit walks the monkit registry, producing one series per
// (measurement, field, tags) sample. This mirrors the stat walk in
// stats/debug/prometheus.go's PrometheusMetrics - same registry.Stats
// callback, same sanitize for names - but builds remote_write series instead
// of text exposition lines, and reads cumulative values straight from the
// registry with no DeltaTransformer applied.
func (client *Client) collectMonkit(now int64) []timeSeries {
	var series []timeSeries
	client.registry.Stats(func(key monkit.SeriesKey, field string, val float64) {
		measurement := sanitize(key.Measurement)
		if !client.cfg.includeMeasurement(measurement) {
			return
		}
		if client.cfg.dropField(field) {
			return
		}

		tags := key.Tags.All()
		labels := make([]label, 0, len(tags)+2+len(client.extraLabels))
		labels = append(labels,
			label{Name: "__name__", Value: measurement},
			label{Name: "field", Value: field},
		)
		labels = append(labels, client.extraLabels...)
		for tag, tagVal := range tags {
			labels = append(labels, label{Name: sanitize(tag), Value: tagVal})
		}

		series = append(series, timeSeries{
			Labels:  sortLabels(labels),
			Samples: []sample{{Value: val, Timestamp: now}},
		})
	})
	return series
}

// collectGatherer converts prometheus/client_golang metric families into
// series. A histogram or summary is flattened the way its text exposition is:
// one _sum series, one _count series, and one series per bucket or quantile,
// so the result is what a scrape of the same registry would have produced.
func (client *Client) collectGatherer(now int64) []timeSeries {
	families, err := client.gatherer.Gather()
	if err != nil {
		// Gather returns partial results alongside an error, so the families
		// it did produce are still pushed.
		client.log.Warn("telemetry gatherer reported an error", slog.Any("error", err))
	}

	var series []timeSeries
	for _, mf := range families {
		name := sanitize(mf.GetName())
		if !client.cfg.includeMeasurement(name) {
			continue
		}
		for _, m := range mf.GetMetric() {
			base := make([]label, 0, len(m.GetLabel())+1+len(client.extraLabels))
			base = append(base, client.extraLabels...)
			for _, lp := range m.GetLabel() {
				base = append(base, label{Name: sanitize(lp.GetName()), Value: lp.GetValue()})
			}
			series = append(series, client.familySeries(name, base, m, now)...)
		}
	}
	return series
}

// familySeries expands one client_golang metric into its series.
func (client *Client) familySeries(name string, base []label, m *dto.Metric, now int64) []timeSeries {
	emit := func(metricName string, extra []label, value float64) timeSeries {
		labels := make([]label, 0, len(base)+len(extra)+1)
		labels = append(labels, label{Name: "__name__", Value: metricName})
		labels = append(labels, base...)
		labels = append(labels, extra...)
		return timeSeries{
			Labels:  sortLabels(labels),
			Samples: []sample{{Value: value, Timestamp: now}},
		}
	}

	switch {
	case m.GetCounter() != nil:
		return []timeSeries{emit(name, nil, m.GetCounter().GetValue())}
	case m.GetGauge() != nil:
		return []timeSeries{emit(name, nil, m.GetGauge().GetValue())}
	case m.GetUntyped() != nil:
		return []timeSeries{emit(name, nil, m.GetUntyped().GetValue())}
	case m.GetSummary() != nil:
		s := m.GetSummary()
		out := []timeSeries{
			emit(name+"_sum", nil, s.GetSampleSum()),
			emit(name+"_count", nil, float64(s.GetSampleCount())),
		}
		for _, q := range s.GetQuantile() {
			out = append(out, emit(name, []label{{
				Name:  "quantile",
				Value: formatFloat(q.GetQuantile()),
			}}, q.GetValue()))
		}
		return out
	case m.GetHistogram() != nil:
		h := m.GetHistogram()
		out := []timeSeries{
			emit(name+"_sum", nil, h.GetSampleSum()),
			emit(name+"_count", nil, float64(h.GetSampleCount())),
		}
		for _, b := range h.GetBucket() {
			out = append(out, emit(name+"_bucket", []label{{
				Name:  "le",
				Value: formatFloat(b.GetUpperBound()),
			}}, float64(b.GetCumulativeCount())))
		}
		// The +Inf bucket is implicit in the protobuf form but required in
		// the exposition, and without it histogram_quantile has no upper
		// bound to interpolate against.
		out = append(out, emit(name+"_bucket", []label{{Name: "le", Value: "+Inf"}},
			float64(h.GetSampleCount())))
		return out
	}
	return nil
}

// push marshals series into a WriteRequest, snappy-compresses it, and POSTs it
// to cfg.URL. On a request error, timeout or non-2xx response it logs at warn
// and returns an error; the caller drops that batch rather than buffering or
// retrying it, because the next interval resends the current cumulative value
// anyway.
func (client *Client) push(ctx context.Context, series []timeSeries) (err error) {
	defer mon.Task()(&ctx)(&err)

	compressed := snappy.Encode(nil, marshalWriteRequest(series))

	httpReq, err := http.NewRequestWithContext(
		ctx, http.MethodPost, client.cfg.URL, bytes.NewReader(compressed),
	)
	if err != nil {
		return Error.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Encoding", "snappy")
	httpReq.Header.Set("Content-Type", "application/x-protobuf")
	httpReq.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	client.setAuth(httpReq)

	resp, err := client.http.Do(httpReq)
	if err != nil {
		client.log.Warn("telemetry remote_write request failed",
			slog.Any("error", err), slog.Int("series", len(series)))
		return Error.Errorf("do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		client.log.Warn("telemetry remote_write push rejected",
			slog.Int("status", resp.StatusCode),
			slog.Int("series", len(series)),
			slog.String("body", string(body)))
		return Error.Errorf("remote_write push rejected: status=%d", resp.StatusCode)
	}
	return nil
}

// setAuth attaches receiver auth, if configured. BearerToken wins over
// Username/Password when both are set.
func (client *Client) setAuth(req *http.Request) {
	switch {
	case client.cfg.BearerToken != "":
		req.Header.Set("Authorization", "Bearer "+client.cfg.BearerToken)
	case client.cfg.Username != "" || client.cfg.Password != "":
		req.SetBasicAuth(client.cfg.Username, client.cfg.Password)
	}
}

// sortLabels sorts a label set by name in place and returns it.
//
// Remote-write receivers validate that labels arrive sorted, and an unsorted
// order also makes the "same" series unstable across ticks, because iteration
// over a monkit tag map is randomized - which breaks the series identity that
// rate() and increase() depend on.
func sortLabels(labels []label) []label {
	sort.Slice(labels, func(i, j int) bool { return labels[i].Name < labels[j].Name })
	return labels
}

// nonEmptyLabels drops labels with an empty value. Prometheus treats an empty
// label value as the label not being there, so sending it wastes bytes on
// every series and makes the label set look inconsistent between a service
// that sets, say, role and one that does not.
func nonEmptyLabels(labels ...label) []label {
	out := labels[:0]
	for _, l := range labels {
		if l.Value != "" {
			out = append(out, l)
		}
	}
	return out
}

// formatFloat renders a bucket bound or quantile the way the text exposition
// format does, so +Inf and integral bounds match a scrape byte for byte.
func formatFloat(f float64) string {
	return strings.TrimSuffix(strings.TrimRight(strconv.FormatFloat(f, 'g', -1, 64), "0"), ".")
}

// resolveInstanceID resolves this process's instance identity: the hostname,
// or a container id read from /proc/self/cgroup, or "unknown". In containers
// the hostname is normally already the container's short id (Docker and
// Kubernetes both default to that), so the cgroup fallback only matters when
// the hostname has been overridden.
func resolveInstanceID() string {
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	if id := containerID(); id != "" {
		return id
	}
	return "unknown"
}

// containerID reads /proc/self/cgroup and extracts a short (12-char) container
// id from a cgroup path segment, if one is present.
func containerID() string {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	return parseContainerID(string(data))
}

// parseContainerID extracts a short (12-char) container id from cgroup file
// contents, if one of its path segments ends in a 64-char hex container id.
// Handles both a bare id (cgroup v1, ".../docker/<64hex>") and a
// systemd-scope-wrapped one (cgroup v2, ".../docker-<64hex>.scope" or
// "cri-containerd-<64hex>.scope"). Split out from containerID so it is
// testable without depending on /proc.
func parseContainerID(cgroupData string) string {
	for _, line := range strings.Split(strings.TrimSpace(cgroupData), "\n") {
		idx := strings.LastIndex(line, "/")
		if idx < 0 || idx+1 >= len(line) {
			continue
		}
		segment := strings.TrimSuffix(line[idx+1:], ".scope")
		if len(segment) < 64 {
			continue
		}
		id := segment[len(segment)-64:]
		if isHex(id) {
			return id[:12]
		}
	}
	return ""
}

func isHex(s string) bool {
	for _, r := range s {
		digit := '0' <= r && r <= '9'
		hexLetter := 'a' <= r && r <= 'f'
		if !digit && !hexLetter {
			return false
		}
	}
	return true
}

// sanitize formats val to be usable as a Prometheus metric or label name,
// matching stats/debug/server.go's sanitize byte for byte (copied rather than
// imported: that one is unexported and those files are left unchanged).
//
// The data model requires names to match [a-zA-Z_:][a-zA-Z0-9_:]*; colons are
// reserved for recording rules, so they are not produced here.
func sanitize(val string) string {
	if val == "" {
		return ""
	}
	if '0' <= val[0] && val[0] <= '9' {
		val = "_" + val
	}
	return strings.Map(func(r rune) rune {
		switch {
		case 'a' <= r && r <= 'z':
			return r
		case 'A' <= r && r <= 'Z':
			return r
		case '0' <= r && r <= '9':
			return r
		default:
			return '_'
		}
	}, val)
}
