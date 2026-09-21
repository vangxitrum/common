// Package logging builds the process logger from its own configuration.
//
// The Config lives here, next to the code that consumes it, rather than in a
// central config package: a package owns its settings, and the root Config
// composes them.
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	logger "gitlab.internal/tuan.quang.tran/aioz-common/logger"
)

// Config configures the process logger. It mirrors logger.Config, flattened,
// because aioz-config cannot bind pointer fields: each optional sink is gated
// by its own Enabled flag instead.
type Config struct {
	Level           string `help:"minimum level: debug, info, warn, error" default:"info" devDefault:"debug" user:"true"`
	Format          string `help:"encoding: text or json" default:"text" releaseDefault:"json" user:"true"`
	AddSource       bool   `help:"annotate records with source file:line" default:"false" devDefault:"true"`
	AsyncBufferSize int    `help:"records buffered before dropping starts" default:"4096"`

	Redact []string `help:"attribute keys whose values are logged as [redacted], beyond the built-in password, token, authorization, cookie and api_key family" default:""`

	Output      string `help:"where the console format is written: stdout, stderr, or a file path" default:""`
	CustomLevel string `help:"per-component minimum levels as COMPONENT=LEVEL pairs, e.g. audit=warn,relay=error; a component is the \"component\" attribute" default:""`

	File          FileConfig
	Graylog       GraylogConfig
	Elasticsearch ElasticsearchConfig
	Sampling      SamplingConfig
}

// FileConfig configures the rotating file sink.
type FileConfig struct {
	Enabled       bool          `help:"write logs to a file as well as stdout" default:"false"`
	Path          string        `help:"log file path" default:"$CONFDIR/logs/service.log" path:"true"`
	Format        string        `help:"file encoding: text or json" default:"json"`
	BufferSize    int           `help:"write buffer size in bytes" default:"65536"`
	FlushInterval time.Duration `help:"how often the buffer is flushed" default:"5s"`
	MaxSize       int64         `help:"rotate once the file exceeds this size in bytes" default:"104857600"`
	MaxBackups    int           `help:"rotated files to keep; 0 keeps all" default:"7"`
}

// GraylogConfig configures the GELF HTTP sink.
type GraylogConfig struct {
	Enabled     bool          `help:"ship logs to Graylog" default:"false"`
	URL         string        `help:"GELF HTTP input endpoint" default:""`
	Facility    string        `help:"syslog facility name" default:"aioz"`
	Service     string        `help:"GELF host field; blank uses the hostname" default:""`
	Timeout     time.Duration `help:"HTTP request timeout" default:"5s"`
	IndexFields []string      `help:"attributes to index; blank sends all" default:""`
}

// ElasticsearchConfig configures the Elasticsearch bulk sink.
type ElasticsearchConfig struct {
	Enabled       bool          `help:"ship logs to Elasticsearch" default:"false"`
	URL           string        `noflag:"true" help:"Elasticsearch base URL, no path" default:""`
	DataStream    string        `help:"target data stream; should match the logs-aioz-* index template" default:"logs-aioz-default"`
	BearerToken   string        `noflag:"true" user:"true" help:"bearer token, for a proxy that swaps in the real credentials" default:""`
	APIKey        string        `user:"true" help:"Elasticsearch API key, when talking to it directly" default:""`
	Username      string        `user:"true" help:"basic auth username" default:""`
	Password      string        `user:"true" help:"basic auth password" default:""`
	BatchSize     int           `help:"records buffered before a bulk flush" default:"500"`
	FlushInterval time.Duration `help:"how often a partial batch is flushed anyway" default:"5s"`
	Timeout       time.Duration `help:"HTTP request timeout" default:"10s"`
	IndexFields   []string      `help:"attributes to index for search; every other attribute still ships, under log.extra, unmapped; blank indexes service and role" default:""`
	Service       string        `help:"service.name on every document; blank uses the hostname" default:""`
	Role          string        `help:"service.role on every document: what this process does, such as api or worker" default:""`
}

// SamplingConfig configures per-level count sampling.
type SamplingConfig struct {
	Enabled    bool          `help:"rate limit repetitive records" default:"false"`
	Tick       time.Duration `help:"sampling window" default:"1s"`
	Initial    int           `help:"records that always pass per window" default:"100"`
	Thereafter int           `help:"after the burst, every Nth record passes" default:"100"`
}

// levels maps every accepted level name to its slog.Level. It is the one
// source of truth for parsing and for shell completion, so the two cannot
// drift apart.
var levels = map[string]slog.Level{
	"debug":    slog.LevelDebug,
	"info":     slog.LevelInfo,
	"warn":     slog.LevelWarn,
	"warning":  slog.LevelWarn,
	"error":    slog.LevelError,
	"critical": logger.LevelCritical,
}

// formats maps every accepted encoding name to its logger.Format.
var formats = map[string]logger.Format{
	"text": logger.FormatText,
	"json": logger.FormatJSON,
}

// Levels returns the level names to suggest, in increasing severity. The
// aliases ParseLevel also accepts are left out: one spelling per level is what
// should be offered.
func Levels() []string {
	return []string{"debug", "info", "warn", "error", "critical"}
}

// Formats returns the encoding names to suggest.
func Formats() []string {
	return []string{"text", "json"}
}

// ParseLevel converts a configured level name into a slog.Level.
//
// The level is a string in the config rather than a slog.Level because
// aioz-config binds only int, int64, uint, uint64, float64, string, bool,
// time.Duration and []string; any other field type panics at bind time.
func ParseLevel(name string) (slog.Level, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return slog.LevelInfo, nil
	}
	if level, ok := levels[name]; ok {
		return level, nil
	}
	return 0, Error.Errorf("unknown log level %q: want one of %s",
		name, strings.Join(Levels(), ", "))
}

// ParseFormat converts a configured encoding name into a logger.Format.
func ParseFormat(name string) (logger.Format, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return logger.FormatText, nil
	}
	if format, ok := formats[name]; ok {
		return format, nil
	}
	return 0, Error.Errorf("unknown log format %q: want one of %s",
		name, strings.Join(Formats(), ", "))
}

// ParseCustomLevel parses COMPONENT=LEVEL pairs separated by commas.
func ParseCustomLevel(spec string) (map[string]slog.Level, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	levels := make(map[string]slog.Level)
	for _, pair := range strings.Split(spec, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, value, ok := strings.Cut(pair, "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || name == "" {
			return nil, Error.Errorf("custom level %q: want COMPONENT=LEVEL", pair)
		}
		level, err := ParseLevel(value)
		if err != nil {
			return nil, Error.Errorf("custom level %q: %v", pair, err)
		}
		levels[name] = level
	}
	return levels, nil
}

// openOutput resolves Output to a writer: empty or stdout is the handler's
// default, stderr is os.Stderr, anything else is a file appended to. The file
// is the process's console and stays open for the life of the process.
func openOutput(output string) (io.Writer, error) {
	switch strings.TrimSpace(output) {
	case "", "stdout":
		return nil, nil
	case "stderr":
		return os.Stderr, nil
	default:
		f, err := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, Error.Wrap(err)
		}
		return f, nil
	}
}

// Validate reports settings that bind and parse but cannot work.
func (c Config) Validate() error {
	if _, err := ParseLevel(c.Level); err != nil {
		return err
	}
	if _, err := ParseFormat(c.Format); err != nil {
		return err
	}
	if _, err := ParseCustomLevel(c.CustomLevel); err != nil {
		return err
	}
	if c.Elasticsearch.Enabled {
		if c.Elasticsearch.URL == "" {
			return Error.Errorf("log.elasticsearch.url is required when log.elasticsearch.enabled is true")
		}
		if !strings.HasPrefix(c.Elasticsearch.URL, "http://") &&
			!strings.HasPrefix(c.Elasticsearch.URL, "https://") {
			// Without a scheme the sink builds a request that cannot be sent,
			// and the failure arrives once per flush, at the transport, with
			// nothing in the configuration to point at.
			return Error.Errorf("log.elasticsearch.url must start with http:// or https://")
		}
		if strings.HasSuffix(strings.TrimSuffix(c.Elasticsearch.URL, "/"), "/_bulk") {
			return Error.Errorf("log.elasticsearch.url is the base URL; the sink appends /_bulk itself")
		}
	}
	if c.File.Enabled {
		if c.File.Path == "" {
			return Error.Errorf("log.file.path is required when log.file.enabled is true")
		}
		if _, err := ParseFormat(c.File.Format); err != nil {
			return Error.Wrap(err)
		}
	}
	if c.Graylog.Enabled && c.Graylog.URL == "" {
		return Error.Errorf("log.graylog.url is required when log.graylog.enabled is true")
	}
	return nil
}

// Bootstrap returns the logger used before the configuration is loaded. It
// writes plain text to stderr and is handed to config.Load with WithLogger, so
// that problems reading the config file are reported by something other than
// the logger that config file is supposed to configure.
func Bootstrap() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level:       slog.LevelInfo,
		ReplaceAttr: RedactAttr(),
	}))
}

// Option adjusts what New builds, for the parts of a handler that are not
// configuration: a writer is the process's own, not something an operator
// sets in a file.
type Option func(*logger.Config)

// WithOutput sends the console format to w instead of os.Stdout.
//
// Config.Output covers stdout, stderr and a file path; this is for a writer
// the process already holds - a test's buffer, or io.Discard for a process
// that wants the handler for its sinks alone.
func WithOutput(w io.Writer) Option {
	return func(lc *logger.Config) { lc.Output = w }
}

// New builds the real logger. The returned LevelVar is what the debug server's
// /logging endpoint mutates, so the level can be retuned at runtime without a
// restart; the returned Handler must be Shutdown before exit or buffered
// records are lost.
func New(cfg Config, opts ...Option) (*logger.Handler, *slog.LevelVar, error) {
	level, err := ParseLevel(cfg.Level)
	if err != nil {
		return nil, nil, Error.Wrap(err)
	}
	levelVar := new(slog.LevelVar)
	levelVar.Set(level)

	format, err := ParseFormat(cfg.Format)
	if err != nil {
		return nil, nil, Error.Wrap(err)
	}
	componentLevels, err := ParseCustomLevel(cfg.CustomLevel)
	if err != nil {
		return nil, nil, err
	}
	output, err := openOutput(cfg.Output)
	if err != nil {
		return nil, nil, err
	}

	// Level is the LevelVar itself, not its current value, so /logging takes
	// effect on the running handler.
	lc := logger.Config{
		Level:           levelVar,
		Format:          format,
		AddSource:       cfg.AddSource,
		AsyncBufferSize: cfg.AsyncBufferSize,
		ReplaceAttr:     RedactAttr(cfg.Redact...),
		Output:          output,
		ComponentLevels: componentLevels,
	}

	if cfg.File.Enabled {
		fileFormat, err := ParseFormat(cfg.File.Format)
		if err != nil {
			return nil, nil, Error.Wrap(err)
		}
		lc.File = &logger.FileConfig{
			Path:          cfg.File.Path,
			Format:        fileFormat,
			BufferSize:    cfg.File.BufferSize,
			FlushInterval: cfg.File.FlushInterval,
			MaxSize:       cfg.File.MaxSize,
			MaxBackups:    cfg.File.MaxBackups,
		}
	}

	if cfg.Graylog.Enabled {
		service := cfg.Graylog.Service
		if service == "" {
			service, _ = os.Hostname()
		}
		lc.Graylog = &logger.GraylogConfig{
			URL:         cfg.Graylog.URL,
			Facility:    cfg.Graylog.Facility,
			Service:     service,
			Timeout:     cfg.Graylog.Timeout,
			IndexFields: cfg.Graylog.IndexFields,
		}
	}

	if cfg.Elasticsearch.Enabled {
		hostname, _ := os.Hostname()
		esService := cfg.Elasticsearch.Service
		if esService == "" {
			esService = hostname
		}
		lc.Elasticsearch = &logger.ElasticsearchConfig{
			URL:           cfg.Elasticsearch.URL,
			DataStream:    cfg.Elasticsearch.DataStream,
			BearerToken:   cfg.Elasticsearch.BearerToken,
			APIKey:        cfg.Elasticsearch.APIKey,
			Username:      cfg.Elasticsearch.Username,
			Password:      cfg.Elasticsearch.Password,
			BatchSize:     cfg.Elasticsearch.BatchSize,
			FlushInterval: cfg.Elasticsearch.FlushInterval,
			Timeout:       cfg.Elasticsearch.Timeout,
			IndexFields:   cfg.Elasticsearch.IndexFields,
			Service:       esService,
			Role:          cfg.Elasticsearch.Role,
			Host:          hostname,
		}
	}

	if cfg.Sampling.Enabled {
		lc.Sampling = &logger.SamplingConfig{
			Tick:       cfg.Sampling.Tick,
			Initial:    cfg.Sampling.Initial,
			Thereafter: cfg.Sampling.Thereafter,
		}
	}

	for _, opt := range opts {
		opt(&lc)
	}

	handler, err := logger.New(lc)
	if err != nil {
		return nil, nil, Error.Wrap(err)
	}
	return handler, levelVar, nil
}
