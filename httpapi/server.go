package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"gitlab.internal/tuan.quang.tran/aioz-common/apierr"
	"gitlab.internal/tuan.quang.tran/aioz-common/clientip"
	"gitlab.internal/tuan.quang.tran/aioz-common/httpsec"
)

// DefaultBodyLimit is the request body limit when Config leaves it zero.
const DefaultBodyLimit = 1 << 20

// Config is the part of a public API's configuration the standard middleware
// stack needs. echoapi.Setup and ginapi.Setup take the same one.
type Config struct {
	// TrustedProxies are the reverse proxies whose X-Forwarded-For is
	// believed. Empty believes none.
	TrustedProxies []string `help:"CIDRs or addresses of the reverse proxies whose X-Forwarded-For header is believed; empty believes none" default:""`
	// RequestLogPlain writes the request log line without ANSI colors. The
	// logger's sinks strip them either way; this is for a plain-text file.
	RequestLogPlain bool `help:"write the request log line without ANSI colors" default:"false"`
	// BodyLimit bounds every request body, in bytes.
	BodyLimit int64 `help:"max request body size in bytes" default:"1048576"`
	// Headers are the OWASP response headers; the zero value suits a JSON API.
	Headers httpsec.HeadersConfig
	// CORS says which pages may read responses; no origins is CORS off.
	CORS httpsec.CORSConfig
}

// Stack is a validated Config, built once at startup so a setting that would
// switch a control off stops the service before it listens.
type Stack struct {
	Resolver  *clientip.Resolver
	Headers   *httpsec.Headers
	CORS      *httpsec.CORS
	BodyLimit int64
	PlainLog  bool
}

// NewStack validates config.
func NewStack(config Config) (*Stack, error) {
	resolver, err := clientip.New(
		clientip.Config{TrustedProxies: config.TrustedProxies},
	)
	if err != nil {
		return nil, Error.Wrap(err)
	}
	headers, err := httpsec.NewHeaders(config.Headers)
	if err != nil {
		return nil, Error.Wrap(err)
	}
	cors, err := httpsec.NewCORS(config.CORS)
	if err != nil {
		return nil, Error.Wrap(err)
	}
	if config.BodyLimit < 0 {
		return nil, Error.Errorf("body limit must not be negative")
	}
	if config.BodyLimit == 0 {
		config.BodyLimit = DefaultBodyLimit
	}
	return &Stack{
		Resolver:  resolver,
		Headers:   headers,
		CORS:      cors,
		BodyLimit: config.BodyLimit,
		PlainLog:  config.RequestLogPlain,
	}, nil
}

// ClientIP is the address the trusted proxies saw, or "" when there is none.
func (s *Stack) ClientIP(r *http.Request) string {
	if ip := s.Resolver.ClientIP(r); ip.IsValid() {
		return ip.String()
	}
	return ""
}

// DecodeJSON reads a JSON request body into v, strictly: the body must be
// declared as JSON, be one JSON value, and name only fields v has. Each
// failure is a classified 400 or 415 the client can act on, never the parser's
// own text.
func DecodeJSON(r *http.Request, v any) error {
	if mediaType, _, err := mime.ParseMediaType(
		r.Header.Get("Content-Type"),
	); err != nil ||
		mediaType != "application/json" {
		return apierr.New(
			"unsupported-media-type",
			http.StatusUnsupportedMediaType,
			"request body must be application/json",
		)
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return err
		}
		if errors.Is(err, io.EOF) {
			return apierr.BadRequest("empty-body", "request body is empty")
		}
		if field, ok := strings.CutPrefix(
			err.Error(),
			"json: unknown field ",
		); ok {
			return WithDetails(
				apierr.BadRequest(
					"unknown-field",
					"request body has a field this API does not accept",
				),
				Detail{
					Field:  strings.Trim(field, `"`),
					Reason: "unknown-field",
				},
			)
		}
		return apierr.BadRequest(
			"invalid-json",
			"request body is not valid JSON for this endpoint",
		)
	}
	if dec.More() {
		return apierr.BadRequest(
			"invalid-json",
			"request body must be a single JSON value",
		)
	}
	return nil
}
