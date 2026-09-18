package logger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ElasticsearchConfig configures the Elasticsearch bulk-ingest sink.
type ElasticsearchConfig struct {
	// URL is the Elasticsearch base URL, without a path (required).
	// Example: "https://logs.example.com". The sink appends /_bulk.
	URL string
	// DataStream is the target data stream, e.g. "logs-aioz-backend". It
	// should match the index template that carries the ILM policy, so
	// retention applies without any per-service setup.
	DataStream string
	// BearerToken, if set, is sent as "Authorization: Bearer ...". This is
	// what a service uses when a reverse proxy in front of Elasticsearch
	// checks the token and swaps in the real credentials, so no
	// Elasticsearch password has to be deployed with the service.
	BearerToken string
	// APIKey, if set, is sent as "Authorization: ApiKey ...". Used when
	// talking to Elasticsearch directly.
	APIKey string
	// Username and Password, if set, are sent as HTTP basic auth. Lowest
	// precedence of the three.
	Username string
	Password string

	// BatchSize is how many records are buffered before a flush. Defaults to
	// 500.
	BatchSize int
	// FlushInterval is how often a partial batch is flushed anyway, so a
	// quiet service's logs still show up promptly. Defaults to 5s.
	FlushInterval time.Duration
	// Timeout is the HTTP request timeout. Defaults to 10s.
	Timeout time.Duration

	// IndexFields is an allowlist of attribute names to forward. Empty
	// forwards everything. Mirrors GraylogConfig.IndexFields.
	IndexFields []string

	// Service is stamped on every document as service.name, and Host as
	// host.name. Without them every document in a shared data stream looks
	// alike, so neither Kibana nor a Grafana panel can split by service.
	Service string
	Host    string
}

// elasticsearchSink ships records to Elasticsearch's _bulk API.
//
// Unlike the Graylog sink, which POSTs one record at a time, this one batches.
// Handle runs on the single async consumer goroutine (see async.go), so a
// per-record round trip would put the queue's throughput at the mercy of
// network latency and show up as a rising Handler.Dropped() rather than as
// slow logging.
type elasticsearchSink struct {
	cfg         ElasticsearchConfig
	client      *http.Client
	bulkURL     string
	indexFields map[string]struct{} // nil means forward all

	mu      sync.Mutex
	buf     bytes.Buffer
	pending int

	done chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

func newElasticsearchSink(cfg ElasticsearchConfig) *elasticsearchSink {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 500
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 5 * time.Second
	}
	if cfg.DataStream == "" {
		cfg.DataStream = "logs-aioz-default"
	}

	s := &elasticsearchSink{
		cfg:     cfg,
		client:  &http.Client{Timeout: cfg.Timeout},
		bulkURL: strings.TrimSuffix(cfg.URL, "/") + "/_bulk",
		done:    make(chan struct{}),
	}
	if len(cfg.IndexFields) > 0 {
		s.indexFields = make(map[string]struct{}, len(cfg.IndexFields))
		for _, f := range cfg.IndexFields {
			s.indexFields[f] = struct{}{}
		}
	}

	s.wg.Add(1)
	go s.flushLoop()
	return s
}

// Handle appends one record to the pending bulk body, flushing when the batch
// is full. It returns the flush's error, if any, so the async consumer counts
// it the same way it counts a failed file write.
func (s *elasticsearchSink) Handle(level slog.Level, t time.Time, msg string, attrs map[string]any) error {
	line, err := json.Marshal(s.document(level, t, msg, attrs))
	if err != nil {
		return fmt.Errorf("elasticsearch: marshal: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// "create" (not "index") is what a data stream accepts: it is append-only,
	// so a document may not overwrite an existing one.
	fmt.Fprintf(&s.buf, "{\"create\":{\"_index\":%q}}\n", s.cfg.DataStream)
	s.buf.Write(line)
	s.buf.WriteByte('\n')
	s.pending++

	if s.pending >= s.cfg.BatchSize {
		return s.flushLocked()
	}
	return nil
}

// Close stops the flush loop and sends whatever is still buffered.
func (s *elasticsearchSink) Close() error {
	s.once.Do(func() { close(s.done) })
	s.wg.Wait()

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

func (s *elasticsearchSink) flushLoop() {
	defer s.wg.Done()

	ticker := time.NewTicker(s.cfg.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.mu.Lock()
			// A flush error here has nowhere to go: this goroutine has no
			// caller. Dropping the batch is the same tradeoff the async
			// queue makes - logging must not block or panic the service.
			_ = s.flushLocked()
			s.mu.Unlock()
		case <-s.done:
			return
		}
	}
}

// flushLocked POSTs the buffered bulk body. Caller must hold s.mu.
//
// The buffer is reset whether or not the push succeeds: retrying would need
// unbounded memory for an Elasticsearch that is down, which is exactly the
// failure mode that must not take the service with it.
func (s *elasticsearchSink) flushLocked() error {
	if s.pending == 0 {
		return nil
	}

	body := make([]byte, s.buf.Len())
	copy(body, s.buf.Bytes())
	s.buf.Reset()
	s.pending = 0

	req, err := http.NewRequest(http.MethodPost, s.bulkURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("elasticsearch: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	s.setAuth(req)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("elasticsearch: post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("elasticsearch: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// setAuth attaches credentials, if configured. Bearer wins over API key,
// which wins over basic auth.
func (s *elasticsearchSink) setAuth(req *http.Request) {
	switch {
	case s.cfg.BearerToken != "":
		req.Header.Set("Authorization", "Bearer "+s.cfg.BearerToken)
	case s.cfg.APIKey != "":
		req.Header.Set("Authorization", "ApiKey "+s.cfg.APIKey)
	case s.cfg.Username != "" || s.cfg.Password != "":
		req.SetBasicAuth(s.cfg.Username, s.cfg.Password)
	}
}

// document builds the ECS-shaped body for one record. ECS field names are
// what Kibana's built-in log views and the logs-* index templates expect, so
// a data view over logs-aioz-* works without per-field configuration.
func (s *elasticsearchSink) document(level slog.Level, t time.Time, msg string, attrs map[string]any) map[string]any {
	loggedAt, _ := attrs["logged_at"].(time.Time)
	if loggedAt.IsZero() {
		loggedAt = t
	}

	doc := map[string]any{
		"@timestamp": loggedAt.UTC().Format(time.RFC3339Nano),
		"message":    stripANSI(msg),
		"log":        map[string]any{"level": ecsLevel(level)},
	}
	if s.cfg.Service != "" {
		doc["service"] = map[string]any{"name": s.cfg.Service}
	}
	if s.cfg.Host != "" {
		doc["host"] = map[string]any{"name": s.cfg.Host}
	}

	labels := make(map[string]any, len(attrs))
	for k, v := range attrs {
		if k == "logged_at" {
			continue
		}
		if s.indexFields != nil {
			if _, ok := s.indexFields[k]; !ok {
				continue
			}
		}

		// A handful of attributes have a canonical ECS home. Everything else
		// goes under labels.*, which the index template maps as a dynamic
		// object, so a new attribute never needs a mapping change.
		switch k {
		case "source":
			if src, ok := v.(string); ok {
				// log.level is already there; add origin beside it.
				doc["log"].(map[string]any)["origin"] = map[string]any{
					"file": map[string]any{"name": src},
				}
				continue
			}
		case stackKey:
			setError(doc, "stack_trace", stripANSI(fmt.Sprint(v)))
			continue
		case "error":
			// ECS keeps the message next to the stack trace. Leaving it in
			// labels instead would split one failure across two places.
			setError(doc, "message", stripANSI(fmt.Sprint(v)))
			continue
		case "trace_id":
			doc["trace"] = map[string]any{"id": fmt.Sprint(v)}
			continue
		case "service":
			doc["service"] = map[string]any{"name": fmt.Sprint(v)}
			continue
		case "host":
			doc["host"] = map[string]any{"name": fmt.Sprint(v)}
			continue
		}

		if str, ok := v.(string); ok {
			labels[k] = stripANSI(str)
			continue
		}
		labels[k] = v
	}
	if len(labels) > 0 {
		doc["labels"] = labels
	}
	return doc
}

// setError merges one field into the document's error object, which both the
// stack trace and the message write to.
func setError(doc map[string]any, key string, value any) {
	errObj, _ := doc["error"].(map[string]any)
	if errObj == nil {
		errObj = map[string]any{}
		doc["error"] = errObj
	}
	errObj[key] = value
}

// ecsLevel renders a level the way ECS spells it. The console's three-letter
// forms (DBG/INF/WRN/ERR/CRT) are for humans reading a terminal; a log store
// wants the full word, which is also what Kibana's level filters offer.
func ecsLevel(level slog.Level) string {
	switch level {
	case slog.LevelDebug:
		return "DEBUG"
	case slog.LevelInfo:
		return "INFO"
	case slog.LevelWarn:
		return "WARN"
	case slog.LevelError:
		return "ERROR"
	case LevelCritical:
		return "CRITICAL"
	default:
		return strings.ToUpper(level.String())
	}
}
