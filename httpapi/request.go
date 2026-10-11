package httpapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"gitlab.internal/tuan.quang.tran/aioz-common/apierr"
	"gitlab.internal/tuan.quang.tran/aioz-common/httpclient"
	"gitlab.internal/tuan.quang.tran/aioz-common/uuid"
)

// RequestIDHeader carries the request id out, on every response. An incoming
// one is ignored; see NewRequestID.
const RequestIDHeader = "X-Request-Id"

// NewRequestID returns the id a request is known by: a new UUIDv7, made by
// this server for every request. An X-Request-Id the caller sends is never
// used: a client could otherwise reuse another request's id, or a support
// ticket's, and send an investigation down the wrong trail, or send one id on
// every request so the log cannot tell them apart.
func NewRequestID() string {
	id, err := uuid.New()
	if err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return id.String()
}

// WithRequestID puts id on ctx where httpclient finds it, so every outbound
// request made while serving this one carries the same X-Request-Id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return httpclient.WithRequestID(ctx, id)
}

// RequestIDFrom returns the id WithRequestID put on ctx.
func RequestIDFrom(
	ctx context.Context,
) string {
	return httpclient.RequestID(ctx)
}

// LimitBody bounds r's body at limit bytes. The limit takes hold at the first
// read, so a route can still change it with SetBodyLimit after the stack has
// run: a declared Content-Length over the limit fails that read before
// anything is read, and an undeclared one fails the first read past the
// limit. Render turns either into a 413. A limit of zero or less leaves the
// body unbounded.
func LimitBody(w http.ResponseWriter, r *http.Request, limit int64) error {
	if limit <= 0 || r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	r.Body = &boundedBody{
		w:      w,
		body:   r.Body,
		length: r.ContentLength,
		limit:  limit,
	}
	return nil
}

// SetBodyLimit replaces the limit LimitBody put on r's body, for a route whose
// bodies are larger or smaller than the stack's: an upload, or a webhook that
// should never be large. It must run before anything reads the body, and
// fails if something already has. A body LimitBody never bounded is bounded
// now.
func SetBodyLimit(w http.ResponseWriter, r *http.Request, limit int64) error {
	if limit <= 0 {
		return Error.Errorf("body limit must be positive, got %d", limit)
	}
	b, ok := r.Body.(*boundedBody)
	if !ok {
		return LimitBody(w, r, limit)
	}
	if b.reader != nil {
		return Error.Errorf("body limit changed after the body was read")
	}
	b.limit = limit
	return nil
}

// boundedBody is a request body whose limit is fixed at its first read rather
// than when the stack wraps it. The stack runs before a route's own
// middleware, and http.MaxBytesReader cannot be loosened once applied, so an
// eager limit would let a route only ever lower it.
type boundedBody struct {
	w      http.ResponseWriter
	body   io.ReadCloser
	length int64
	limit  int64
	reader io.ReadCloser // set at the first read
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.reader == nil {
		if b.length > b.limit {
			return 0, &http.MaxBytesError{Limit: b.limit}
		}
		b.reader = http.MaxBytesReader(b.w, b.body, b.limit)
	}
	return b.reader.Read(p)
}

func (b *boundedBody) Close() error { return b.body.Close() }

// Recovered turns a recovered panic into a classified 500, carrying the stack
// for LogError. Call it from a deferred recover.
func Recovered(v any) error {
	return apierr.Internal(
		"panic",
		fmt.Errorf("panic: %v\n%s", v, debug.Stack()),
	)
}

// Access is one request, as the access log records it.
type Access struct {
	Method    string
	Path      string // the URL path, never the query: a query can carry a token
	Route     string
	Status    int
	Latency   time.Duration
	Bytes     int64
	RemoteIP  string
	RequestID string
	UserAgent string
	Err       error
	// Plain leaves the ANSI colors out of the line.
	Plain bool
}

// The ANSI colors of the request line, as aioz-stream writes it.
const (
	colorRed          = 31
	colorGreen        = 32
	colorYellow       = 33
	colorLightRed     = 91
	colorLightGreen   = 92
	colorLightYellow  = 93
	colorLightBlue    = 94
	colorLightMagenta = 95
)

// methodLabels are three letters each so the columns of a tailed log line up.
// The logger's Graylog and Elasticsearch sinks strip the colors and file the
// first word as the method, reading PST, PTC and DEL back as POST, PATCH and
// DELETE.
var methodLabels = map[string]struct {
	label string
	color int
}{
	http.MethodGet:    {"GET", colorLightBlue},
	http.MethodPost:   {"PST", colorLightGreen},
	http.MethodPut:    {"PUT", colorLightYellow},
	http.MethodPatch:  {"PTC", colorLightMagenta},
	http.MethodDelete: {"DEL", colorLightRed},
}

// RequestLine is the access log message, in aioz-stream's format, which the
// logger's sinks parse into fields (method, status, latency, url):
//
//	GET status:200 latency:012ms url:/v1/videos/42
//
// Any other method is written in full, uncolored.
func RequestLine(method string, status int, latency time.Duration, path string, plain bool) string {
	label, color := method, 0
	if m, ok := methodLabels[method]; ok {
		label, color = m.label, m.color
	}
	statusColor := colorRed
	switch {
	case status >= 200 && status < 400:
		statusColor = colorGreen
	case status >= 400 && status < 500:
		statusColor = colorYellow
	}
	code := strconv.Itoa(status)
	if !plain {
		if color != 0 {
			label = colorize(color, label)
		}
		code = colorize(statusColor, code)
	}
	return label + " status:" + code + " latency:" + formatLatency(latency) + " url:" + path
}

func colorize(code int, s string) string {
	return "\033[" + strconv.Itoa(code) + "m" + s + "\033[0m"
}

// formatLatency is zero-padded to three digits in the largest unit that fits,
// so a tailed log lines up and time.ParseDuration still reads it.
func formatLatency(d time.Duration) string {
	switch {
	case d >= time.Second:
		return fmt.Sprintf("%03ds", int64(d.Seconds()))
	case d >= time.Millisecond:
		return fmt.Sprintf("%03dms", d.Milliseconds())
	case d >= time.Microsecond:
		return fmt.Sprintf("%03dµs", d.Microseconds())
	default:
		return fmt.Sprintf("%03dns", d.Nanoseconds())
	}
}

// LogAccess writes one access log record, at info, in the group "api": the
// request line as the message, and the request id, route, client address,
// response size and user agent as attributes, plus the error's reason when
// the request failed and its text when that was a server fault. A server fault is logged again,
// with its cause and stack, by LogError.
func LogAccess(ctx context.Context, log *slog.Logger, a Access) {
	if log == nil {
		return
	}
	attrs := []any{
		"group", "api",
		"request_id", a.RequestID,
		"route", a.Route,
		"remote_ip", a.RemoteIP,
		"bytes", a.Bytes,
		"user_agent", a.UserAgent,
	}
	if a.Err != nil {
		_, body := Render(a.Err, "")
		attrs = append(attrs, "reason", body.Reason)
		if a.Status >= http.StatusInternalServerError {
			// The string, not the error: a handler that json.Marshals the
			// value renders an error as {}. A client error's text is the
			// client's own mistake and is already in the reason.
			attrs = append(attrs, "err", a.Err.Error())
		}
	}
	log.InfoContext(ctx, RequestLine(a.Method, a.Status, a.Latency, a.Path, a.Plain), attrs...)
}
