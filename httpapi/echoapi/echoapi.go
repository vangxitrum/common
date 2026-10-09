// Package echoapi is httpapi for echo v4: the standard middleware stack, the
// error handler and the response helpers, so an echo service follows the team
// HTTP API standard without writing any of it.
//
//	stack, err := httpapi.NewStack(cfg.API)
//	if err != nil {
//	    return err
//	}
//	e := echo.New()
//	echoapi.Setup(e, log, stack)
//
//	api := e.Group("/v1")
//	api.GET("/videos/:id", getVideo, echoapi.Cache(httpapi.CachePolicy{Type: httpapi.Revalidate}))
//
// Every behavior lives in httpapi and is shared with ginapi; the conformance
// suite holds both to the same bytes.
package echoapi

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/spacemonkeygo/monkit/v3"

	"gitlab.internal/tuan.quang.tran/aioz-common/apierr"
	"gitlab.internal/tuan.quang.tran/aioz-common/httpapi"
)

var mon = monkit.Package()

// Setup installs the standard stack on e, in this order:
//
//  1. client IP from the trusted proxies (c.RealIP)
//  2. request id: the caller's when sane, else a new one; on the response,
//     the context and every outbound call
//  3. security headers and CORS, before routing so a preflight to any path is
//     answered
//  4. access log, which sees the status the error handler finally wrote
//  5. panic recovery, as a classified 500
//  6. body limit
//  7. per-route metrics
//
// and makes ErrorHandler e's error handler.
//
// opts leave out the access log, the panic recovery or the metrics, or keep
// some paths out of the access log; see httpapi.Option. A route changes the
// body limit with BodyLimit.
func Setup(e *echo.Echo, log *slog.Logger, stack *httpapi.Stack, opts ...httpapi.Option) {
	o := httpapi.NewOptions(opts...)
	e.HideBanner = true
	e.HidePort = true
	e.HTTPErrorHandler = ErrorHandler(log)
	e.IPExtractor = stack.ClientIP

	e.Pre(requestID, func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			h := c.Response().Header()
			stack.Headers.Apply(h)
			if stack.CORS.Handle(h, c.Request()) {
				return c.NoContent(http.StatusNoContent)
			}
			return next(c)
		}
	})
	var chain []echo.MiddlewareFunc
	if o.AccessLog() {
		chain = append(chain, accessLog(log, stack.PlainLog, o))
	}
	if o.Recovery() {
		chain = append(chain, recoverPanic)
	}
	chain = append(chain, bodyLimit(stack.BodyLimit))
	if o.Metrics() {
		chain = append(chain, metrics)
	}
	e.Use(chain...)
}

func requestID(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		id := httpapi.RequestID(c.Request())
		c.Response().Header().Set(httpapi.RequestIDHeader, id)
		c.SetRequest(
			c.Request().
				WithContext(httpapi.WithRequestID(c.Request().Context(), id)),
		)
		return next(c)
	}
}

func accessLog(log *slog.Logger, plain bool, o httpapi.Options) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			start := time.Now()
			err := next(c)
			if err != nil {
				// Render now, so the status logged is the one the client got.
				c.Error(err)
			}
			r := c.Request()
			if !o.Logged(r.URL.Path, c.Response().Status) {
				return nil
			}
			httpapi.LogAccess(r.Context(), log, httpapi.Access{
				Method:    r.Method,
				Path:      r.URL.Path,
				Route:     c.Path(),
				Status:    c.Response().Status,
				Latency:   time.Since(start),
				Bytes:     c.Response().Size,
				RemoteIP:  c.RealIP(),
				RequestID: c.Response().Header().Get(httpapi.RequestIDHeader),
				UserAgent: r.UserAgent(),
				Err:       classify(err),
				Plain:     plain,
			})
			return nil
		}
	}
}

func recoverPanic(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) (err error) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				err = httpapi.Recovered(v)
			}
		}()
		return next(c)
	}
}

func bodyLimit(limit int64) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if err := httpapi.LimitBody(
				c.Response(),
				c.Request(),
				limit,
			); err != nil {
				return err
			}
			return next(c)
		}
	}
}

// metrics records a latency distribution, a status series and a counter per
// route pattern, never per raw path, so cardinality stays bounded.
func metrics(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		start := time.Now()
		err := next(c)
		route := c.Path()
		if route == "" {
			route = "unmatched"
		}
		tag := monkit.NewSeriesTag("route", route)
		mon.DurationVal("request_duration", tag).Observe(time.Since(start))
		mon.Counter("requests", tag).Inc(1)
		return err
	}
}

// ErrorHandler renders every error as the standard error body and logs server
// faults. A framework error (an unrouted path, a method with no handler, a
// bind failure) is classified by its status, with the status text as its
// message: echo's own messages quote the parser and are not for a client.
func ErrorHandler(log *slog.Logger) echo.HTTPErrorHandler {
	return func(err error, c echo.Context) {
		if c.Response().Committed {
			return
		}
		err = classify(err)
		requestID := c.Response().Header().Get(httpapi.RequestIDHeader)
		httpapi.LogError(c.Request().Context(), log, err, c.Path(), requestID)
		mon.Counter("request_failures", monkit.NewSeriesTag("reason", reasonOf(err))).
			Inc(1)
		httpapi.WriteError(c.Response(), err, requestID)
	}
}

// classify turns a framework error (an unrouted path, a method with no handler,
// a bind failure) into an apierr by its status, so it is rendered and logged
// exactly like gin's. An apierr or an over-limit body is left as it is.
func classify(err error) error {
	var apiErr *apierr.Error
	var tooLarge *http.MaxBytesError
	var httpErr *echo.HTTPError
	if !errors.As(err, &apiErr) && !errors.As(err, &tooLarge) && errors.As(err, &httpErr) {
		return httpapi.FromStatus(httpErr.Code, "")
	}
	return err
}

func reasonOf(err error) string {
	_, body := httpapi.Render(err, "")
	return body.Reason
}

// BodyLimit sets a route's request body limit in bytes, in place of the
// stack's: higher for an upload, lower for a webhook. A limit of zero or less
// is a programming error found when routes are registered, so it panics, like
// Cache.
func BodyLimit(limit int64) echo.MiddlewareFunc {
	if limit <= 0 {
		panic(httpapi.Error.Errorf("body limit must be positive, got %d", limit))
	}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if err := httpapi.SetBodyLimit(c.Response(), c.Request(), limit); err != nil {
				return err
			}
			return next(c)
		}
	}
}

// Cache applies a route's cache policy. An invalid policy is a programming
// error found when routes are registered, so it panics, like
// regexp.MustCompile.
func Cache(policy httpapi.CachePolicy) echo.MiddlewareFunc {
	if err := policy.Validate(); err != nil {
		panic(err)
	}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			policy.Apply(c.Response().Header())
			return next(c)
		}
	}
}

// NoStore marks a route as never cached. It is also what a route without a
// policy gets; say it explicitly on tokens, personal data and money.
func NoStore() echo.MiddlewareFunc { return Cache(httpapi.CachePolicy{Type: httpapi.NoStore}) }

// CheckETag sets the ETag and answers 304 when the client already holds it;
// on true the handler returns nil without rendering.
func CheckETag(c echo.Context, etag string) bool {
	return httpapi.CheckETag(c.Response(), c.Request(), etag)
}

// Bind decodes a JSON body strictly, unless opts loosen it; see
// httpapi.DecodeJSON.
func Bind(c echo.Context, v any, opts ...httpapi.DecodeOption) error {
	return httpapi.DecodeJSON(c.Request(), v, opts...)
}

// Page reads offset, limit and cursor; see httpapi.ParsePage.
func Page(c echo.Context, config httpapi.PageConfig) (httpapi.Page, error) {
	return httpapi.ParsePage(c.QueryParams(), config)
}

// Send answers status with any success envelope, for one that carries a
// message:
//
//	return Send(c, http.StatusOK, httpapi.Message("password changed"))
//	return Send(c, http.StatusCreated, httpapi.Success(video).WithMessage("video published"))
func Send(c echo.Context, status int, envelope httpapi.Envelope) error {
	return httpapi.WriteJSON(c.Response(), status, envelope)
}

// OK answers 200 with data in the success envelope.
func OK(c echo.Context, data any) error {
	return httpapi.WriteJSON(c.Response(), http.StatusOK, httpapi.Success(data))
}

// Created answers 201 with data in the success envelope.
func Created(c echo.Context, data any) error {
	return httpapi.WriteJSON(
		c.Response(),
		http.StatusCreated,
		httpapi.Success(data),
	)
}

// List answers 200 with one page of a collection: the items under key, the
// total, and the query as served. See httpapi.List.
func List[T any](c echo.Context, key string, items []T, total int64, query any) error {
	return httpapi.WriteJSON(c.Response(), http.StatusOK, httpapi.List(key, items, total, query))
}

// CursorList answers 200 with one page of a cursor-paginated collection. See
// httpapi.CursorList.
func CursorList[T any](c echo.Context, key string, items []T, nextCursor string, query any) error {
	return httpapi.WriteJSON(c.Response(), http.StatusOK, httpapi.CursorList(key, items, nextCursor, query))
}
