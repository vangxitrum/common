package logger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitlab.internal/tuan.quang.tran/aioz-common/httpclient"
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

	// IndexFields names the attributes a service wants to search and
	// aggregate on. They are written where Elasticsearch maps them: the ones
	// with an ECS home go there, the rest become labels.<name>, which the
	// index template maps as a keyword.
	//
	// Every other attribute is still shipped, under log.extra, which the
	// template maps with "enabled": false. It stays in _source, so it is there
	// in Discover and in an API response, but it creates no mapping entry and
	// cannot be filtered on. That split is the point: a field the template
	// maps is a field that exists in the mapping for ever, and an index has a
	// field limit - a thousand by default - that a service inventing attribute
	// names at runtime will eventually reach.
	//
	// Empty takes [DefaultIndexFields].
	//
	// This is not GraylogConfig.IndexFields, which drops what it does not
	// list.
	IndexFields []string

	// Service is stamped on every document as service.name, Role as
	// service.role and Host as host.name. Without them every document in a
	// shared data stream looks alike, so neither Kibana nor a Grafana panel
	// can split by service, and "every error from the workers" is not a query
	// anyone can write.
	//
	// Role is what this process does - "api", "worker", "job-runner" - for a
	// service deployed as several of them. It is fixed for the process rather
	// than read from each record: a value that can be forgotten at a call site
	// is one that will be.
	Service string
	Role    string
	Host    string

	// Env is stamped on every document as service.environment: which
	// deployment - staging or production - wrote it. Two deployments of one
	// service write to the same data stream, so without it their records are
	// indistinguishable. Blank writes no field.
	Env string
}

// DefaultIndexFields are the attributes indexed when IndexFields is empty.
//
// Each names a dimension a dashboard splits by rather than a detail of one
// event, which is what makes them worth a permanent mapping entry: the
// service, its process, and the part of it - the [Named] logger - that wrote
// the line.
var DefaultIndexFields = []string{"service", "role", "component"}

// ecsPaths are the attributes that have a home in ECS, which is where Kibana's
// built-in views and the logs-* templates look for them. An indexed attribute
// not named here becomes labels.<name>.
var ecsPaths = map[string][2]string{
	"service": {"service", "name"},
	"role":    {"service", "role"},
	"env":     {"service", "environment"},

	// A logger's name, which is what a component is: see Named.
	"component": {"log", "logger"},
	"host":      {"host", "name"},
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
	indexFields map[string]struct{} // the attributes worth a mapping entry

	mu      sync.Mutex
	buf     bytes.Buffer
	pending int

	done chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

// shipTransport is the default transport plus HTTP/2 health checks. The sink
// posts to the same kind of endpoint the telemetry client does - often the
// same tunnel - and has the same failure mode: a connection that died without
// a reset is reused until every flush times out, and the batches in between
// are dropped rather than queued.
func shipTransport() *http.Transport {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil
	}

	transport = transport.Clone()
	httpclient.ConfigureH2HealthChecks(transport, 0, 0)

	return transport
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
		client:  &http.Client{Timeout: cfg.Timeout, Transport: shipTransport()},
		bulkURL: strings.TrimSuffix(cfg.URL, "/") + "/_bulk",
		done:    make(chan struct{}),
	}
	fields := cfg.IndexFields
	if len(fields) == 0 {
		fields = DefaultIndexFields
	}
	s.indexFields = make(map[string]struct{}, len(fields))
	for _, f := range fields {
		s.indexFields[f] = struct{}{}
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

	// An api-group record's message is a formatted request line rather than
	// prose: "GET status:200 latency:002ms url:/readyz". Left alone it is only
	// full-text searchable, so "all 5xx on /upload" is not a query anyone can
	// write. The Graylog sink already splits it into discrete fields; this
	// does the same, into the ECS names Kibana and Grafana both understand.
	group, _ := attrs["group"].(string)
	if group == "api" {
		parseRequestLine(doc, doc["message"].(string))
	}
	if s.cfg.Service != "" {
		setNestedValue(doc, "service", "name", s.cfg.Service)
	}
	if s.cfg.Role != "" {
		setNestedValue(doc, "service", "role", s.cfg.Role)
	}
	if s.cfg.Host != "" {
		setNestedValue(doc, "host", "name", s.cfg.Host)
	}
	if s.cfg.Env != "" {
		setNestedValue(doc, "service", "environment", s.cfg.Env)
	}

	labels := make(map[string]any, len(attrs))
	extra := make(map[string]any, len(attrs))
	for k, v := range attrs {
		if k == "logged_at" {
			continue
		}

		// These four describe the event itself rather than a dimension of it.
		// They are always written where ECS puts them, declared or not: a
		// stack trace filed under log.extra is a stack trace nobody finds, and
		// a trace id that cannot be searched defeats having one.
		// A db-group record carries the statement, its shape and its cost as
		// discrete attributes. Left in log.extra they would be stored but not
		// mapped, so "every UPDATE over 200ms" would not be a query anyone
		// could write - which is the reason the statement is logged at all.
		if group == "db" {
			if mapped := setDBField(doc, k, v); mapped {
				continue
			}
		}

		switch k {
		case "source":
			if src, ok := v.(string); ok {
				// Merged, not assigned: attrs is a map, so "callstack" may
				// have been handled first and already put log.origin there.
				logOrigin(doc)["file"] = map[string]any{"name": src}
				continue
			}
		case callstackKey:
			// This is where the process was when it logged, which is a
			// property of the log event and not of any error. It sits beside
			// log.origin.file.name, which says the same thing one frame deep.
			logOrigin(doc)["stack_trace"] = stripANSI(fmt.Sprint(v))
			continue
		case "error":
			// ECS files the failure under error.message, where Kibana looks
			// for it.
			setNestedValue(doc, "error", "message", stripANSI(fmt.Sprint(v)))
			continue
		case "trace_id":
			doc["trace"] = map[string]any{"id": fmt.Sprint(v)}
			continue
		}

		if str, ok := v.(string); ok {
			v = stripANSI(str)
		}

		// Declared: somewhere the template maps, so it can be filtered and
		// aggregated. Undeclared: log.extra, which the template does not map,
		// so it is readable but adds no field to the index for ever.
		if _, ok := s.indexFields[k]; !ok {
			extra[k] = v
			continue
		}
		if path, ok := ecsPaths[k]; ok {
			setNestedValue(doc, path[0], path[1], fmt.Sprint(v))
			continue
		}
		labels[k] = v
	}
	if len(labels) > 0 {
		doc["labels"] = labels
	}
	if len(extra) > 0 {
		doc["log"].(map[string]any)["extra"] = extra
	}
	return doc
}

// setNestedValue writes doc[top][key], creating the object as needed. A record
// attribute overwrites what the config stamped, because the call site knew
// something the process-wide setting did not.
func setNestedValue(doc map[string]any, top, key string, value any) {
	outer, _ := doc[top].(map[string]any)
	if outer == nil {
		outer = map[string]any{}
		doc[top] = outer
	}
	outer[key] = value
}

// parseRequestLine pulls the fields out of an api-group message and writes
// them as ECS: the first word is the method, and the rest are key:value pairs.
// The message itself is left in place - it is what a human reads in Discover.
//
// Anything it cannot parse is left alone rather than guessed at: a malformed
// line should lose its extra fields, not its content.
func parseRequestLine(doc map[string]any, msg string) {
	fields := strings.Fields(msg)
	if len(fields) == 0 {
		return
	}

	if method, ok := httpMethod(fields[0]); ok {
		setNested(doc, "http", "request", map[string]any{"method": method})
	}

	for _, f := range fields[1:] {
		key, value, ok := strings.Cut(f, ":")
		if !ok || value == "" {
			continue
		}
		switch key {
		case "status":
			if code, err := strconv.Atoi(value); err == nil {
				setNested(doc, "http", "response", map[string]any{"status_code": code})
			}
		case "url":
			doc["url"] = map[string]any{"path": value}
		case "latency":
			if d, err := time.ParseDuration(value); err == nil {
				// ECS measures event.duration in nanoseconds.
				doc["event"] = map[string]any{"duration": d.Nanoseconds()}
			}
		}
	}
}

// setDBField files one attribute of a db-group record under its ECS-style
// home, reporting whether it belonged to one. Anything else - the caller's
// operation name, say - falls through to the ordinary label/extra split.
//
// The names follow OpenTelemetry's db semantic conventions rather than ECS,
// which has no database fieldset: db.statement, db.operation and
// db.rows_affected. The index template maps them.
func setDBField(doc map[string]any, key string, value any) bool {
	switch key {
	case "group":
		return true // the marker itself is not a field
	case "statement", "operation":
		setNestedValue(doc, "db", key, value)
		return true
	case "rows":
		setNestedValue(doc, "db", "rows_affected", value)
		return true
	case "name":
		// Which pool, for a service with more than one.
		setNested(doc, "db", "instance", map[string]any{"name": value})
		return true
	case "duration":
		if d, ok := value.(time.Duration); ok {
			// ECS measures event.duration in nanoseconds.
			doc["event"] = map[string]any{"duration": d.Nanoseconds()}
			return true
		}
	}
	return false
}

// setNested merges sub into doc[top][mid], creating the objects as needed.
func setNested(doc map[string]any, top, mid string, sub map[string]any) {
	outer, _ := doc[top].(map[string]any)
	if outer == nil {
		outer = map[string]any{}
		doc[top] = outer
	}
	inner, _ := outer[mid].(map[string]any)
	if inner == nil {
		inner = map[string]any{}
		outer[mid] = inner
	}
	for k, v := range sub {
		inner[k] = v
	}
}

// httpMethod reads the first word of a request line as an HTTP method, so a
// message that merely starts with a capitalised word does not get one invented
// for it. The request line names POST, PATCH and DELETE in three letters (PST,
// PTC, DEL) so its columns line up; ECS wants the method itself.
func httpMethod(s string) (string, bool) {
	switch s {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "CONNECT", "OPTIONS", "TRACE":
		return s, true
	case "PST":
		return "POST", true
	case "PTC":
		return "PATCH", true
	case "DEL":
		return "DELETE", true
	}
	return "", false
}

// logOrigin returns doc's log.origin object, creating it if this is the first
// field to land there. Both "source" and "callstack" write into it, and map
// iteration decides which arrives first.
func logOrigin(doc map[string]any) map[string]any {
	log, _ := doc["log"].(map[string]any)
	if log == nil {
		log = map[string]any{}
		doc["log"] = log
	}
	origin, _ := log["origin"].(map[string]any)
	if origin == nil {
		origin = map[string]any{}
		log["origin"] = origin
	}
	return origin
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
