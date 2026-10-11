// Package ginapi is httpapi for gin: the standard middleware stack, the error
// renderer and the response helpers, so a gin service follows the team HTTP
// API standard without writing any of it.
//
//	stack, err := httpapi.NewStack(cfg.API)
//	if err != nil {
//	    return err
//	}
//	r := gin.New()
//	ginapi.Setup(r, log, stack)
//
//	api := r.Group("/v1")
//	api.GET("/videos/:id", ginapi.Cache(httpapi.CachePolicy{Type: httpapi.Revalidate}), ginapi.Handle(getVideo))
//
// gin handlers return nothing, so Handle adapts one that returns an error: the
// error is rendered by the stack, exactly as echoapi renders it.
package ginapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/spacemonkeygo/monkit/v3"

	"gitlab.internal/tuan.quang.tran/aioz-common/httpapi"
	"gitlab.internal/tuan.quang.tran/aioz-common/httpsec"
)

var mon = monkit.Package()

const clientIPKey = "httpapi.client_ip"

// Setup installs the standard stack on r, in the same order as echoapi:
// client IP, request id, security headers and CORS, access log, error
// rendering, panic recovery, body limit, per-route metrics. Unrouted paths
// and unsupported methods are answered by the same error renderer.
//
// opts leave out the access log, the panic recovery or the metrics, or keep
// some paths out of the access log; see httpapi.Option. A route changes the
// body limit with BodyLimit.
//
// gin's own client-IP guess is switched off; read the address with ClientIP.
func Setup(r *gin.Engine, log *slog.Logger, stack *httpapi.Stack, opts ...httpapi.Option) {
	o := httpapi.NewOptions(opts...)
	_ = r.SetTrustedProxies(nil)
	r.ForwardedByClientIP = false
	r.HandleMethodNotAllowed = true

	chain := []gin.HandlerFunc{requestID, security(stack)}
	if o.AccessLog() {
		chain = append(chain, accessLog(log, stack, o))
	}
	chain = append(chain, renderErrors(log))
	if o.Recovery() {
		chain = append(chain, recoverPanic)
	}
	chain = append(chain, bodyLimit(stack.BodyLimit))
	if o.Metrics() {
		chain = append(chain, metrics)
	}
	r.Use(chain...)
	r.NoRoute(
		func(c *gin.Context) { abort(c, httpapi.FromStatus(http.StatusNotFound, "")) },
	)
	r.NoMethod(
		func(c *gin.Context) { abort(c, httpapi.FromStatus(http.StatusMethodNotAllowed, "")) },
	)
}

// ClientIP is the address the trusted proxies saw.
func ClientIP(c *gin.Context) string { return c.GetString(clientIPKey) }

func security(stack *httpapi.Stack) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(clientIPKey, stack.ClientIP(c.Request))
		h := c.Writer.Header()
		stack.Headers.Apply(h)
		if stack.CORS.Handle(h, c.Request) {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

func requestID(c *gin.Context) {
	id := httpapi.NewRequestID()
	c.Writer.Header().Set(httpapi.RequestIDHeader, id)
	c.Request = c.Request.WithContext(
		httpapi.WithRequestID(c.Request.Context(), id),
	)
	c.Next()
}

func accessLog(log *slog.Logger, stack *httpapi.Stack, o httpapi.Options) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		if !o.Logged(c.Request.URL.Path, c.Writer.Status()) {
			return
		}
		var err error
		if last := c.Errors.Last(); last != nil {
			err = last.Err
		}
		httpapi.LogAccess(c.Request.Context(), log, httpapi.Access{
			Method:    c.Request.Method,
			Path:      c.Request.URL.Path,
			Route:     c.FullPath(),
			Status:    c.Writer.Status(),
			Latency:   time.Since(start),
			Bytes:     int64(max(c.Writer.Size(), 0)),
			RemoteIP:  stack.ClientIP(c.Request),
			RequestID: c.Writer.Header().Get(httpapi.RequestIDHeader),
			UserAgent: c.Request.UserAgent(),
			Err:       err,
			Plain:     stack.PlainLog,
		})
	}
}

// renderErrors writes the last error a handler recorded, unless the handler
// had already written a response.
func renderErrors(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		last := c.Errors.Last()
		if last == nil || c.Writer.Written() {
			return
		}
		requestID := c.Writer.Header().Get(httpapi.RequestIDHeader)
		httpapi.LogError(
			c.Request.Context(),
			log,
			last.Err,
			c.FullPath(),
			requestID,
		)
		_, body := httpapi.Render(last.Err, "")
		mon.Counter("request_failures", monkit.NewSeriesTag("reason", body.Reason)).
			Inc(1)
		httpapi.WriteError(c.Writer, last.Err, requestID)
	}
}

func recoverPanic(c *gin.Context) {
	defer func() {
		if v := recover(); v != nil {
			if v == http.ErrAbortHandler {
				panic(v)
			}
			abort(c, httpapi.Recovered(v))
		}
	}()
	c.Next()
}

func bodyLimit(limit int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := httpapi.LimitBody(c.Writer, c.Request, limit); err != nil {
			abort(c, err)
			return
		}
		c.Next()
	}
}

func metrics(c *gin.Context) {
	start := time.Now()
	c.Next()
	route := c.FullPath()
	if route == "" {
		route = "unmatched"
	}
	tag := monkit.NewSeriesTag("route", route)
	mon.DurationVal("request_duration", tag).Observe(time.Since(start))
	mon.Counter("requests", tag).Inc(1)
}

func abort(c *gin.Context, err error) {
	_ = c.Error(err)
	c.Abort()
}

// Handle adapts a handler that returns an error. A returned error is rendered
// by the stack as the standard error body.
func Handle(fn func(c *gin.Context) error) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := fn(c); err != nil {
			abort(c, err)
		}
	}
}

// BodyLimit sets a route's request body limit in bytes, in place of the
// stack's: higher for an upload, lower for a webhook. A limit of zero or less
// is a programming error found when routes are registered, so it panics.
func BodyLimit(limit int64) gin.HandlerFunc {
	if limit <= 0 {
		panic(httpapi.Error.Errorf("body limit must be positive, got %d", limit))
	}
	return func(c *gin.Context) {
		if err := httpapi.SetBodyLimit(c.Writer, c.Request, limit); err != nil {
			abort(c, err)
			return
		}
		c.Next()
	}
}

// ContentSecurityPolicy sets a route's or group's Content-Security-Policy in
// place of the stack's, which lets nothing load and suits only JSON. Use it
// on routes that serve a page, such as Swagger UI:
//
//	r.GET("/swagger/*any", ginapi.ContentSecurityPolicy(httpsec.SwaggerUIPolicy), ginSwagger.WrapHandler(swaggerFiles.Handler))
//
// An invalid policy is a programming error found when routes are registered,
// so it panics.
func ContentSecurityPolicy(policy string) gin.HandlerFunc {
	csp, err := httpsec.NewContentSecurityPolicy(policy)
	if err != nil {
		panic(err)
	}
	return func(c *gin.Context) {
		csp.Apply(c.Writer.Header())
		c.Next()
	}
}

// Cache applies a route's cache policy. An invalid policy is a programming
// error found when routes are registered, so it panics.
func Cache(policy httpapi.CachePolicy) gin.HandlerFunc {
	if err := policy.Validate(); err != nil {
		panic(err)
	}
	return func(c *gin.Context) {
		policy.Apply(c.Writer.Header())
		c.Next()
	}
}

// NoStore marks a route as never cached.
func NoStore() gin.HandlerFunc { return Cache(httpapi.CachePolicy{Type: httpapi.NoStore}) }

// CheckETag sets the ETag and answers 304 when the client already holds it;
// on true the handler returns without rendering.
func CheckETag(c *gin.Context, etag string) bool {
	if !httpapi.CheckETag(c.Writer, c.Request, etag) {
		return false
	}
	c.Writer.WriteHeaderNow()
	return true
}

// Bind decodes a JSON body strictly, unless opts loosen it; see
// httpapi.DecodeJSON.
func Bind(c *gin.Context, v any, opts ...httpapi.DecodeOption) error {
	return httpapi.DecodeJSON(c.Request, v, opts...)
}

// Page reads offset, limit and cursor; see httpapi.ParsePage.
func Page(c *gin.Context, config httpapi.PageConfig) (httpapi.Page, error) {
	return httpapi.ParsePage(c.Request.URL.Query(), config)
}

// Send answers status with any success envelope, for one that carries a
// message:
//
//	return Send(c, http.StatusOK, httpapi.Message("password changed"))
//	return Send(c, http.StatusCreated, httpapi.Success(video).WithMessage("video published"))
func Send(c *gin.Context, status int, envelope httpapi.Envelope) error {
	return httpapi.WriteJSON(c.Writer, status, envelope)
}

// OK answers 200 with data in the success envelope.
func OK(c *gin.Context, data any) error {
	return httpapi.WriteJSON(c.Writer, http.StatusOK, httpapi.Success(data))
}

// Created answers 201 with data in the success envelope.
func Created(c *gin.Context, data any) error {
	return httpapi.WriteJSON(
		c.Writer,
		http.StatusCreated,
		httpapi.Success(data),
	)
}

// List answers 200 with one page of a collection: the items under key, the
// total, and the query as served. See httpapi.List.
func List[T any](c *gin.Context, key string, items []T, total int64, query any) error {
	return httpapi.WriteJSON(c.Writer, http.StatusOK, httpapi.List(key, items, total, query))
}

// CursorList answers 200 with one page of a cursor-paginated collection. See
// httpapi.CursorList.
func CursorList[T any](c *gin.Context, key string, items []T, nextCursor string, query any) error {
	return httpapi.WriteJSON(c.Writer, http.StatusOK, httpapi.CursorList(key, items, nextCursor, query))
}
