package logger

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gitlab.internal/tuan.quang.tran/aioz-common/httpclient"
)

// bulkRecorder is a fake Elasticsearch: it parses the ndjson bulk body so a
// test can assert on the documents rather than on the raw request.
type bulkRecorder struct {
	*httptest.Server

	mu       sync.Mutex
	status   int
	requests int
	headers  []http.Header
	actions  []map[string]any
	docs     []map[string]any
}

func newBulkRecorder(t *testing.T) *bulkRecorder {
	t.Helper()

	r := &bulkRecorder{status: http.StatusOK}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)

		r.mu.Lock()
		defer r.mu.Unlock()

		r.requests++
		r.headers = append(r.headers, req.Header.Clone())

		// The bulk body alternates action line, document line.
		lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
		for i := 0; i+1 < len(lines); i += 2 {
			var action, doc map[string]any
			require.NoError(t, json.Unmarshal([]byte(lines[i]), &action))
			require.NoError(t, json.Unmarshal([]byte(lines[i+1]), &doc))
			r.actions = append(r.actions, action)
			r.docs = append(r.docs, doc)
		}
		w.WriteHeader(r.status)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *bulkRecorder) snapshot() (int, []map[string]any, []map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests, append([]map[string]any(nil), r.actions...), append([]map[string]any(nil), r.docs...)
}

func testESConfig(url string) ElasticsearchConfig {
	return ElasticsearchConfig{
		URL:        url,
		DataStream: "logs-aioz-test",
		// Long enough that the periodic flush never fires during a test:
		// every flush a test sees is one it caused.
		FlushInterval: time.Hour,
		BatchSize:     2,
		Service:       "testsvc",
		Host:          "testhost",
	}
}

func TestElasticsearchSink_BatchesUntilFull(t *testing.T) {
	recv := newBulkRecorder(t)
	sink := newElasticsearchSink(testESConfig(recv.URL))
	defer func() { _ = sink.Close() }()

	now := time.Now()
	require.NoError(t, sink.Handle(slog.LevelInfo, now, "first", nil))

	requests, _, _ := recv.snapshot()
	require.Zero(t, requests, "a partial batch must not be sent")

	require.NoError(t, sink.Handle(slog.LevelInfo, now, "second", nil))

	requests, actions, docs := recv.snapshot()
	require.Equal(t, 1, requests, "the full batch goes in one request")
	require.Len(t, docs, 2)
	require.Equal(t, "first", docs[0]["message"])
	require.Equal(t, "second", docs[1]["message"])

	// A data stream only accepts "create"; "index" would be rejected.
	create, ok := actions[0]["create"].(map[string]any)
	require.True(t, ok, "action must be create, got %v", actions[0])
	require.Equal(t, "logs-aioz-test", create["_index"])
}

func TestElasticsearchSink_CloseFlushesPartialBatch(t *testing.T) {
	recv := newBulkRecorder(t)
	sink := newElasticsearchSink(testESConfig(recv.URL))

	require.NoError(t, sink.Handle(slog.LevelInfo, time.Now(), "only one", nil))
	require.NoError(t, sink.Close())

	requests, _, docs := recv.snapshot()
	require.Equal(t, 1, requests)
	require.Len(t, docs, 1)
	require.Equal(t, "only one", docs[0]["message"])
}

func TestElasticsearchSink_PeriodicFlush(t *testing.T) {
	recv := newBulkRecorder(t)
	cfg := testESConfig(recv.URL)
	cfg.BatchSize = 1000 // never reached
	cfg.FlushInterval = 20 * time.Millisecond
	sink := newElasticsearchSink(cfg)
	defer func() { _ = sink.Close() }()

	require.NoError(t, sink.Handle(slog.LevelInfo, time.Now(), "quiet service", nil))

	require.Eventually(t, func() bool {
		_, _, docs := recv.snapshot()
		return len(docs) == 1
	}, 2*time.Second, 10*time.Millisecond, "a partial batch should flush on the interval")
}

func TestElasticsearchSink_DocumentShape(t *testing.T) {
	recv := newBulkRecorder(t)
	cfg := testESConfig(recv.URL)
	cfg.BatchSize = 1
	sink := newElasticsearchSink(cfg)
	defer func() { _ = sink.Close() }()

	loggedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	require.NoError(t, sink.Handle(LevelCritical, time.Now(), "\x1b[31mred message\x1b[0m", map[string]any{
		"logged_at": loggedAt,
		"source":    "/app/main.go:42",
		"error":     "boom",
		"trace_id":  "trace-1",
		"user_id":   "user-1",
		"attempts":  3,
	}))

	_, _, docs := recv.snapshot()
	require.Len(t, docs, 1)
	doc := docs[0]

	require.Equal(t, "2026-01-02T03:04:05Z", doc["@timestamp"], "logged_at wins over the handler's time")
	require.Equal(t, "red message", doc["message"], "ANSI coloring is for terminals, not for a log store")
	require.Equal(t, "CRITICAL", doc["log"].(map[string]any)["level"])
	require.Equal(t, "testsvc", doc["service"].(map[string]any)["name"])
	require.Equal(t, "testhost", doc["host"].(map[string]any)["name"],
		"host.name is the machine, service.name is the service - they are not the same field")
	require.Equal(t, "trace-1", doc["trace"].(map[string]any)["id"])
	require.Equal(t, "boom", doc["error"].(map[string]any)["message"])
	require.NotContains(t, doc["error"], "stack_trace")

	origin := doc["log"].(map[string]any)["origin"].(map[string]any)
	require.Equal(t, "/app/main.go:42", origin["file"].(map[string]any)["name"])

	// user_id and attempts were not declared as index fields, so they ship
	// under log.extra: readable, but adding nothing to the mapping.
	extra := doc["log"].(map[string]any)["extra"].(map[string]any)
	require.Equal(t, "user-1", extra["user_id"])
	require.Equal(t, float64(3), extra["attempts"])
	require.NotContains(t, extra, "logged_at", "the timestamp is not also an attribute")
	require.NotContains(t, extra, "trace_id", "a field with an ECS home is not duplicated")
	require.NotContains(t, doc, "labels", "nothing here was declared as a label")
}

func TestElasticsearchSink_ErrorIsFiledAsErrorMessage(t *testing.T) {
	recv := newBulkRecorder(t)
	cfg := testESConfig(recv.URL)
	cfg.BatchSize = 1
	sink := newElasticsearchSink(cfg)
	defer func() { _ = sink.Close() }()

	require.NoError(t, sink.Handle(LevelCritical, time.Now(), "it broke", map[string]any{
		"error": "boom",
	}))

	_, _, docs := recv.snapshot()
	errObj := docs[0]["error"].(map[string]any)
	require.Equal(t, map[string]any{"message": "boom"}, errObj,
		"the message and nothing else: an error's own stack is not logged")
	require.NotContains(t, docs[0], "labels", "nothing should be left over in labels")
}

func TestElasticsearchSink_ParsesApiRequestLine(t *testing.T) {
	recv := newBulkRecorder(t)
	cfg := testESConfig(recv.URL)
	cfg.BatchSize = 1
	sink := newElasticsearchSink(cfg)
	defer func() { _ = sink.Close() }()

	require.NoError(t, sink.Handle(slog.LevelInfo, time.Now(),
		"GET status:200 latency:2ms url:/readyz",
		map[string]any{"group": "api", "request_id": "req-1"}))

	_, _, docs := recv.snapshot()
	doc := docs[0]

	require.Equal(t, "GET", doc["http"].(map[string]any)["request"].(map[string]any)["method"])
	require.Equal(t, float64(200), doc["http"].(map[string]any)["response"].(map[string]any)["status_code"])
	require.Equal(t, "/readyz", doc["url"].(map[string]any)["path"])
	require.Equal(t, float64(2_000_000), doc["event"].(map[string]any)["duration"], "ECS measures duration in nanoseconds")

	// The message survives: it is what a human reads in Discover.
	require.Equal(t, "GET status:200 latency:2ms url:/readyz", doc["message"])
	// request_id is not a declared dimension, so it ships under log.extra:
	// one request id is not something a dashboard splits by, and mapping one
	// field per request is how an index reaches its field limit.
	require.Equal(t, "req-1",
		doc["log"].(map[string]any)["extra"].(map[string]any)["request_id"])
}

func TestElasticsearchSink_NonApiMessageIsNotParsed(t *testing.T) {
	recv := newBulkRecorder(t)
	cfg := testESConfig(recv.URL)
	cfg.BatchSize = 1
	sink := newElasticsearchSink(cfg)
	defer func() { _ = sink.Close() }()

	// Same shape of message, but not an api-group record: nothing is invented.
	require.NoError(t, sink.Handle(slog.LevelInfo, time.Now(),
		"GET status:200 url:/readyz", map[string]any{"group": "worker"}))

	_, _, docs := recv.snapshot()
	require.NotContains(t, docs[0], "http")
	require.NotContains(t, docs[0], "url")
}

func TestElasticsearchSink_MalformedApiLineKeepsItsMessage(t *testing.T) {
	recv := newBulkRecorder(t)
	cfg := testESConfig(recv.URL)
	cfg.BatchSize = 1
	sink := newElasticsearchSink(cfg)
	defer func() { _ = sink.Close() }()

	require.NoError(t, sink.Handle(slog.LevelInfo, time.Now(),
		"something went sideways status:notanumber latency:forever",
		map[string]any{"group": "api"}))

	_, _, docs := recv.snapshot()
	doc := docs[0]
	require.Equal(t, "something went sideways status:notanumber latency:forever", doc["message"])
	require.NotContains(t, doc, "http", "an unparseable status must not become a field")
	require.NotContains(t, doc, "event")
}

func TestElasticsearchSink_IndexFieldsSplitIndexedFromShipped(t *testing.T) {
	recv := newBulkRecorder(t)
	cfg := testESConfig(recv.URL)
	cfg.BatchSize = 1
	cfg.IndexFields = []string{"tenant"}
	sink := newElasticsearchSink(cfg)
	defer func() { _ = sink.Close() }()

	require.NoError(t, sink.Handle(slog.LevelInfo, time.Now(), "m", map[string]any{
		"tenant":      "acme",
		"part_number": 7,
	}))

	_, _, docs := recv.snapshot()
	doc := docs[0]

	require.Equal(t, "acme", doc["labels"].(map[string]any)["tenant"],
		"a declared attribute is mapped, so it can be filtered and aggregated")

	extra := doc["log"].(map[string]any)["extra"].(map[string]any)
	require.Equal(t, float64(7), extra["part_number"],
		"an undeclared attribute is still shipped: it is kept out of the "+
			"mapping, not out of the document")
	require.NotContains(t, doc["labels"], "part_number")
}

func TestElasticsearchSink_DefaultIndexFieldsAreServiceAndRole(t *testing.T) {
	recv := newBulkRecorder(t)
	cfg := testESConfig(recv.URL)
	cfg.BatchSize = 1
	cfg.IndexFields = nil
	sink := newElasticsearchSink(cfg)
	defer func() { _ = sink.Close() }()

	require.NoError(t, sink.Handle(slog.LevelInfo, time.Now(), "m", map[string]any{
		"service": "from-record",
		"role":    "worker",
		"other":   "x",
	}))

	_, _, docs := recv.snapshot()
	doc := docs[0]

	service := doc["service"].(map[string]any)
	require.Equal(t, "from-record", service["name"],
		"a record attribute overrides what the config stamped")
	require.Equal(t, "worker", service["role"])
	require.Equal(t, "x", doc["log"].(map[string]any)["extra"].(map[string]any)["other"])
}

func TestElasticsearchSink_RoleIsStampedOnEveryDocument(t *testing.T) {
	recv := newBulkRecorder(t)
	cfg := testESConfig(recv.URL)
	cfg.BatchSize = 1
	cfg.Role = "job-runner"
	sink := newElasticsearchSink(cfg)
	defer func() { _ = sink.Close() }()

	require.NoError(t, sink.Handle(slog.LevelInfo, time.Now(), "m", nil))

	_, _, docs := recv.snapshot()
	service := docs[0]["service"].(map[string]any)
	require.Equal(t, "testsvc", service["name"])
	require.Equal(t, "job-runner", service["role"],
		"a process deployed as several roles is only tellable apart by this")
}

func TestElasticsearchSink_StructuralFieldsAreIndexedUndeclared(t *testing.T) {
	recv := newBulkRecorder(t)
	cfg := testESConfig(recv.URL)
	cfg.BatchSize = 1
	// A declaration that mentions none of them.
	cfg.IndexFields = []string{"tenant"}
	sink := newElasticsearchSink(cfg)
	defer func() { _ = sink.Close() }()

	require.NoError(t, sink.Handle(slog.LevelError, time.Now(), "m", map[string]any{
		"trace_id": "trace-1",
		"error":    "boom",
		"source":   "/app/main.go:42",
	}))

	_, _, docs := recv.snapshot()
	doc := docs[0]

	// These describe the event, not a dimension of it: filing them under an
	// unmapped object would make a failure nobody can search for.
	require.Equal(t, "trace-1", doc["trace"].(map[string]any)["id"])
	require.Equal(t, "boom", doc["error"].(map[string]any)["message"])
	require.NotNil(t, doc["log"].(map[string]any)["origin"])
	require.NotContains(t, doc["log"], "extra")
}

func TestElasticsearchSink_Auth(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*ElasticsearchConfig)
		want   string
	}{
		"bearer": {func(c *ElasticsearchConfig) { c.BearerToken = "tok"; c.APIKey = "key" }, "Bearer tok"},
		"apikey": {func(c *ElasticsearchConfig) { c.APIKey = "key"; c.Username = "u" }, "ApiKey key"},
		"basic":  {func(c *ElasticsearchConfig) { c.Username, c.Password = "u", "p" }, "Basic dTpw"},
		"none":   {func(c *ElasticsearchConfig) {}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			recv := newBulkRecorder(t)
			cfg := testESConfig(recv.URL)
			cfg.BatchSize = 1
			tc.mutate(&cfg)

			sink := newElasticsearchSink(cfg)
			defer func() { _ = sink.Close() }()

			require.NoError(t, sink.Handle(slog.LevelInfo, time.Now(), "m", nil))

			recv.mu.Lock()
			defer recv.mu.Unlock()
			require.Equal(t, tc.want, recv.headers[0].Get("Authorization"))
			require.Equal(t, "application/x-ndjson", recv.headers[0].Get("Content-Type"))
		})
	}
}

func TestElasticsearchSink_RejectedBatchIsReportedAndDropped(t *testing.T) {
	recv := newBulkRecorder(t)
	recv.status = http.StatusBadRequest

	cfg := testESConfig(recv.URL)
	cfg.BatchSize = 1
	sink := newElasticsearchSink(cfg)
	defer func() { _ = sink.Close() }()

	err := sink.Handle(slog.LevelInfo, time.Now(), "m", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "status 400")

	// The failed batch is not retried: buffering it would grow without bound
	// while Elasticsearch is down.
	recv.mu.Lock()
	recv.status = http.StatusOK
	recv.mu.Unlock()

	require.NoError(t, sink.Handle(slog.LevelInfo, time.Now(), "next", nil))
	_, _, docs := recv.snapshot()
	require.Len(t, docs, 2, "each Handle sent exactly one document")
	require.Equal(t, "next", docs[1]["message"])
}

func TestElasticsearchSink_CloseIsIdempotent(t *testing.T) {
	recv := newBulkRecorder(t)
	sink := newElasticsearchSink(testESConfig(recv.URL))

	require.NoError(t, sink.Close())
	require.NoError(t, sink.Close(), "a second Close must not panic on a closed channel")
}

func TestElasticsearchSink_UsesAHealthCheckedTransport(t *testing.T) {
	sink := newElasticsearchSink(ElasticsearchConfig{URL: "http://example.invalid"})
	defer func() { _ = sink.Close() }()

	transport, ok := sink.client.Transport.(*http.Transport)
	require.True(t, ok, "the sink should carry its own transport")

	// Same reasoning as the telemetry client: this sink posts to the same kind
	// of endpoint and would stall the same way on a dead pooled connection.
	require.NotNil(t, transport.HTTP2,
		"http/2 health checks should already be configured")
	require.Equal(t,
		httpclient.DefaultH2HealthCheckInterval, transport.HTTP2.SendPingTimeout)
	require.Equal(t,
		httpclient.DefaultH2PingTimeout, transport.HTTP2.PingTimeout)
}

func TestECSLevel(t *testing.T) {
	require.Equal(t, "DEBUG", ecsLevel(slog.LevelDebug))
	require.Equal(t, "INFO", ecsLevel(slog.LevelInfo))
	require.Equal(t, "WARN", ecsLevel(slog.LevelWarn))
	require.Equal(t, "ERROR", ecsLevel(slog.LevelError))
	require.Equal(t, "CRITICAL", ecsLevel(LevelCritical))
}

func TestElasticsearchSink_URLNeedsNoTrailingSlashCare(t *testing.T) {
	sink := newElasticsearchSink(ElasticsearchConfig{URL: "http://es:9200/"})
	require.Equal(t, "http://es:9200/_bulk", sink.bulkURL)
	require.NoError(t, sink.Close())
}

func TestElasticsearchSink_CallstackGoesUnderLogOrigin(t *testing.T) {
	recv := newBulkRecorder(t)
	cfg := testESConfig(recv.URL)
	cfg.BatchSize = 1
	sink := newElasticsearchSink(cfg)
	defer func() { _ = sink.Close() }()

	require.NoError(t, sink.Handle(
		slog.LevelError, time.Now(), "it broke",
		map[string]any{
			"source":     "/app/main.go:42",
			"error":      "boom",
			callstackKey: "app/api.(*H).Get:12\napp/router.Serve:40",
		},
	))

	_, _, docs := recv.snapshot()
	require.Len(t, docs, 1)
	doc := docs[0]

	origin := doc["log"].(map[string]any)["origin"].(map[string]any)
	require.Equal(t, "/app/main.go:42", origin["file"].(map[string]any)["name"],
		"the call site keeps its place")
	require.Contains(t, origin["stack_trace"], "app/api.(*H).Get:12",
		"and the call stack sits beside it, under log.origin")

	require.NotContains(t, doc["error"], "stack_trace",
		"a call stack is not an error's stack trace")

	extra, _ := doc["log"].(map[string]any)["extra"].(map[string]any)
	require.NotContains(t, extra, callstackKey,
		"it has an ECS home, so it is not duplicated into log.extra")
}

func TestElasticsearchSink_CallstackWithNoSource(t *testing.T) {
	// log.origin may not exist yet when AddSource is off.
	recv := newBulkRecorder(t)
	cfg := testESConfig(recv.URL)
	cfg.BatchSize = 1
	sink := newElasticsearchSink(cfg)
	defer func() { _ = sink.Close() }()

	require.NoError(t, sink.Handle(
		slog.LevelError, time.Now(), "it broke",
		map[string]any{callstackKey: "app/api.(*H).Get:12"},
	))

	_, _, docs := recv.snapshot()
	require.Len(t, docs, 1)
	origin := docs[0]["log"].(map[string]any)["origin"].(map[string]any)
	require.Equal(t, "app/api.(*H).Get:12", origin["stack_trace"])
	require.NotContains(t, origin, "file")
}

func TestElasticsearchSink_MapsADbRecord(t *testing.T) {
	recv := newBulkRecorder(t)
	cfg := testESConfig(recv.URL)
	cfg.BatchSize = 1
	sink := newElasticsearchSink(cfg)
	defer func() { _ = sink.Close() }()

	require.NoError(t, sink.Handle(slog.LevelWarn, time.Now(), "slow query",
		map[string]any{
			"group":     "db",
			"statement": "SELECT id FROM users WHERE email = $1",
			"operation": "SELECT",
			"duration":  250 * time.Millisecond,
			"rows":      int64(1),
			"name":      "primary",
			"op":        "invoice reconciliation",
		}))

	_, _, docs := recv.snapshot()
	require.Len(t, docs, 1)
	doc := docs[0]

	db := doc["db"].(map[string]any)
	require.Equal(t, "SELECT id FROM users WHERE email = $1", db["statement"],
		"the parameterized statement is searchable, not just prose in a message")
	require.Equal(t, "SELECT", db["operation"])
	require.Equal(t, float64(1), db["rows_affected"])
	require.Equal(t, "primary", db["instance"].(map[string]any)["name"])

	require.Equal(t, float64(250_000_000),
		doc["event"].(map[string]any)["duration"],
		"ECS measures duration in nanoseconds")

	// The message still reads as prose in Discover.
	require.Equal(t, "slow query", doc["message"])

	extra, _ := doc["log"].(map[string]any)["extra"].(map[string]any)
	require.NotContains(t, extra, "statement",
		"a field with an ECS home is not duplicated into log.extra")
	require.NotContains(t, extra, "duration")
	require.Equal(t, "invoice reconciliation", extra["op"],
		"the caller's operation name has no ECS home, so it ships as extra")
}

func TestElasticsearchSink_ANonDbRecordKeepsItsAttributes(t *testing.T) {
	recv := newBulkRecorder(t)
	cfg := testESConfig(recv.URL)
	cfg.BatchSize = 1
	sink := newElasticsearchSink(cfg)
	defer func() { _ = sink.Close() }()

	// "statement" outside a db record means something else entirely.
	require.NoError(t, sink.Handle(slog.LevelInfo, time.Now(), "filed",
		map[string]any{"statement": "a bank statement"}))

	_, _, docs := recv.snapshot()
	require.Len(t, docs, 1)
	require.NotContains(t, docs[0], "db")
	require.Equal(t, "a bank statement",
		docs[0]["log"].(map[string]any)["extra"].(map[string]any)["statement"])
}
