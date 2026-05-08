package logger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// GraylogConfig configures the Graylog GELF HTTP sink.
type GraylogConfig struct {
	// URL is the GELF HTTP input endpoint (required).
	// Example: "http://graylog:12201/gelf"
	URL string
	// Facility is the syslog facility name sent with every record.
	Facility string
	// Service identifies the host/service in GELF's host field.
	Service string
	// Timeout is the HTTP request timeout. Defaults to 5s.
	Timeout time.Duration
	// IndexFields is an allowlist of log attribute names to forward as GELF
	// additional fields (underscore-prefixed, indexed by Graylog).
	// When empty, all attributes are forwarded.
	// Example: []string{"trace_id", "user_id", "env", "region"}
	IndexFields []string
}

type graylogSink struct {
	cfg         GraylogConfig
	client      *http.Client
	indexFields map[string]struct{} // nil means forward all
}

func newGraylogSink(cfg GraylogConfig) *graylogSink {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	s := &graylogSink{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.Timeout},
	}
	if len(cfg.IndexFields) > 0 {
		s.indexFields = make(map[string]struct{}, len(cfg.IndexFields))
		for _, f := range cfg.IndexFields {
			s.indexFields[f] = struct{}{}
		}
	}
	return s
}

// Handle ships a GELF 1.1 message to Graylog over HTTP.
// This runs inside the async consumer goroutine, so the HTTP call does not
// block log callers.
func (s *graylogSink) Handle(level slog.Level, t time.Time, msg string, attrs map[string]any) error {
	loggedAt, _ := attrs["logged_at"].(time.Time)
	if loggedAt.IsZero() {
		loggedAt = t
	}

	extra := make(map[string]any, len(attrs))
	for k, v := range attrs {
		if k == "logged_at" {
			continue
		}
		if s.indexFields != nil {
			if _, ok := s.indexFields[k]; !ok {
				continue
			}
		}
		extra[k] = v
	}

	// Special handling for API group records: extract HTTP method and endpoint
	// from the formatted message so Graylog can index them as discrete fields.
	if group, _ := attrs["group"].(string); group == "api" {
		words := strings.Fields(stripANSI(msg))
		for i, w := range words {
			if i == 0 {
				extra["method"] = w
			} else if parts := strings.SplitN(w, ":", 2); len(parts) == 2 {
				extra[parts[0]] = parts[1]
			}
		}
		msg = strings.Join(words, " ")
	} else {
		msg = stripANSI(msg)
	}

	fullMsg, _ := attrs["full_message"].(string)

	payload := &gelfMessage{
		Version:      "1.1",
		Host:         s.cfg.Service,
		ShortMessage: msg,
		FullMessage:  fullMsg,
		Timestamp:    float64(loggedAt.UnixNano()) / 1e9,
		Level:        slogToSyslog(level),
		Facility:     s.cfg.Facility,
		Service:      s.cfg.Service,
		Extra:        extra,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("graylog: marshal: %w", err)
	}

	resp, err := s.client.Post(s.cfg.URL, "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("graylog: post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("graylog: unexpected status %d", resp.StatusCode)
	}
	return nil
}

func (s *graylogSink) Close() error { return nil }

// gelfMessage is a GELF 1.1 log message.
type gelfMessage struct {
	Version      string         `json:"version"`
	Host         string         `json:"host"`
	ShortMessage string         `json:"short_message"`
	FullMessage  string         `json:"full_message,omitempty"`
	Timestamp    float64        `json:"timestamp"`
	Level        int            `json:"level"`
	Facility     string         `json:"facility"`
	Service      string         `json:"_service"`
	Extra        map[string]any `json:"-"`
}

// MarshalJSON serializes g and merges Extra fields with a "_" prefix as
// required by the GELF spec for additional fields.
func (g *gelfMessage) MarshalJSON() ([]byte, error) {
	type alias gelfMessage
	raw := make(map[string]any)
	b, err := json.Marshal((*alias)(g))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	for k, v := range g.Extra {
		raw["_"+k] = v
	}
	return json.Marshal(raw)
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// slogToSyslog maps slog levels to syslog severity numbers used by GELF.
func slogToSyslog(level slog.Level) int {
	switch {
	case level >= LevelCritical:
		return 2 // critical
	case level >= slog.LevelError:
		return 3 // error
	case level >= slog.LevelWarn:
		return 4 // warning
	case level >= slog.LevelInfo:
		return 6 // informational
	default:
		return 7 // debug
	}
}
