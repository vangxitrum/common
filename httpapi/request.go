package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"gitlab.internal/tuan.quang.tran/aioz-common/apierr"
	"gitlab.internal/tuan.quang.tran/aioz-common/httpclient"
	"gitlab.internal/tuan.quang.tran/aioz-common/uuid"
)

// RequestIDHeader carries the request id in and out.
const RequestIDHeader = "X-Request-Id"

// RequestID returns the id a request is known by: the caller's own, when it
// sent a sane one, so a trace crosses services under one id; otherwise a new
// UUIDv7. A caller's id is sane when it is 1-128 characters of letters,
// digits, '-', '_', '.' and ':'; anything else could forge a log line or
// blow up a log index, and is replaced.
func RequestID(r *http.Request) string {
	if id := r.Header.Get(RequestIDHeader); requestIDSafe(id) {
		return id
	}
	id, err := uuid.New()
	if err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return id.String()
}

func requestIDSafe(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.' || c == ':':
		default:
			return false
		}
	}
	return true
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

// LimitBody bounds r's body at limit bytes. A declared Content-Length over the
// limit is refused before anything is read; an undeclared one fails the first
// read past the limit, which Render turns into a 413.
func LimitBody(w http.ResponseWriter, r *http.Request, limit int64) error {
	if limit <= 0 || r.Body == nil {
		return nil
	}
	if r.ContentLength > limit {
		return FromStatus(
			http.StatusRequestEntityTooLarge,
			"request body too large",
		)
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	return nil
}

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
