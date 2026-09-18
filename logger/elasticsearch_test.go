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
		stackKey:    "main.boom\n\t/app/main.go:42",
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
	require.Contains(t, doc["error"].(map[string]any)["stack_trace"], "main.boom")

	origin := doc["log"].(map[string]any)["origin"].(map[string]any)
	require.Equal(t, "/app/main.go:42", origin["file"].(map[string]any)["name"])

	labels := doc["labels"].(map[string]any)
	require.Equal(t, "user-1", labels["user_id"])
	require.Equal(t, float64(3), labels["attempts"])
	require.NotContains(t, labels, "logged_at", "the timestamp is not also a label")
	require.NotContains(t, labels, "trace_id", "a field with an ECS home is not duplicated")
}

func TestElasticsearchSink_ErrorMessageJoinsTheStackTrace(t *testing.T) {
	recv := newBulkRecorder(t)
	cfg := testESConfig(recv.URL)
	cfg.BatchSize = 1
	sink := newElasticsearchSink(cfg)
	defer func() { _ = sink.Close() }()

	require.NoError(t, sink.Handle(LevelCritical, time.Now(), "it broke", map[string]any{
		"error":  "boom",
		stackKey: "main.boom",
	}))

	_, _, docs := recv.snapshot()
	errObj := docs[0]["error"].(map[string]any)
	require.Equal(t, "boom", errObj["message"], "the message belongs with the stack trace")
	require.Equal(t, "main.boom", errObj["stack_trace"])
	require.NotContains(t, docs[0], "labels", "nothing should be left over in labels")
}

func TestElasticsearchSink_IndexFieldsAllowlist(t *testing.T) {
	recv := newBulkRecorder(t)
	cfg := testESConfig(recv.URL)
	cfg.BatchSize = 1
	cfg.IndexFields = []string{"keep"}
	sink := newElasticsearchSink(cfg)
	defer func() { _ = sink.Close() }()

	require.NoError(t, sink.Handle(slog.LevelInfo, time.Now(), "m", map[string]any{
		"keep": "yes",
		"drop": "no",
	}))

	_, _, docs := recv.snapshot()
	labels := docs[0]["labels"].(map[string]any)
	require.Equal(t, "yes", labels["keep"])
	require.NotContains(t, labels, "drop")
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
