package ginapi_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"gitlab.internal/tuan.quang.tran/aioz-common/apierr"
	"gitlab.internal/tuan.quang.tran/aioz-common/httpapi"
	"gitlab.internal/tuan.quang.tran/aioz-common/httpapi/ginapi"
	"gitlab.internal/tuan.quang.tran/aioz-common/httpapi/internal/conformance"
	"gitlab.internal/tuan.quang.tran/aioz-common/httpsec"
)

func TestConformance(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	logs := &conformance.Logs{}
	ginapi.Setup(r, conformance.Logger(logs), conformance.Stack(t), conformance.Options...)

	v1 := r.Group("/v1")
	v1.GET("/items/:id", ginapi.Handle(func(c *gin.Context) error {
		if c.Param("id") == "missing" {
			return apierr.NotFound("item-not-found", "no such item")
		}
		return ginapi.OK(c, map[string]string{"id": c.Param("id")})
	}))
	v1.GET("/items", ginapi.Handle(func(c *gin.Context) error {
		page, err := ginapi.Page(c, httpapi.PageConfig{})
		if err != nil {
			return err
		}
		items := []map[string]string{{"id": "a"}, {"id": "b"}}
		return ginapi.List(c, "items", items, 53, page)
	}))
	v1.GET(
		"/cached",
		ginapi.Cache(httpapi.CachePolicy{Type: httpapi.Revalidate}),
		ginapi.Handle(func(c *gin.Context) error {
			if ginapi.CheckETag(c, httpapi.ETag("v1")) {
				return nil
			}
			return ginapi.OK(c, map[string]int{"v": 1})
		}),
	)
	v1.GET(
		"/public",
		ginapi.Cache(conformance.Public),
		ginapi.Handle(func(c *gin.Context) error {
			return ginapi.OK(c, map[string]int{"v": 1})
		}),
	)
	v1.GET(
		"/public-fail",
		ginapi.Cache(conformance.Public),
		ginapi.Handle(func(c *gin.Context) error {
			return apierr.Conflict("busy", "try later")
		}),
	)
	v1.GET(
		"/plain",
		ginapi.Handle(
			func(c *gin.Context) error { return errors.New("db down: host 10.0.0.5") },
		),
	)
	v1.GET(
		"/panic",
		ginapi.Handle(func(c *gin.Context) error { panic("boom") }),
	)
	v1.GET("/limited", ginapi.Handle(func(c *gin.Context) error {
		return apierr.TooManyRequests("slow-down", "too many requests")
	}))
	v1.POST("/password", ginapi.Handle(func(c *gin.Context) error {
		return ginapi.Send(c, http.StatusOK, httpapi.Message("password changed"))
	}))
	v1.POST("/publish", ginapi.Handle(func(c *gin.Context) error {
		return ginapi.Send(c, http.StatusCreated,
			httpapi.Success(map[string]string{"id": "v1"}).WithMessage("video published"))
	}))
	v1.POST("/echo", ginapi.Handle(func(c *gin.Context) error {
		var in struct {
			Name string `json:"name"`
		}
		if err := ginapi.Bind(c, &in); err != nil {
			return err
		}
		return ginapi.Created(c, in)
	}))

	v1.POST("/lenient", ginapi.Handle(func(c *gin.Context) error {
		var in struct {
			Name string `json:"name"`
		}
		if err := ginapi.Bind(c, &in, httpapi.AllowUnknownFields()); err != nil {
			return err
		}
		return ginapi.Created(c, in)
	}))
	v1.POST("/upload", ginapi.BodyLimit(conformance.UploadLimit), ginapi.Handle(func(c *gin.Context) error {
		n, err := io.Copy(io.Discard, c.Request.Body)
		if err != nil {
			return err
		}
		return ginapi.OK(c, map[string]int64{"bytes": n})
	}))
	v1.POST("/webhook", ginapi.BodyLimit(conformance.WebhookLimit), ginapi.Handle(func(c *gin.Context) error {
		var in struct {
			Name string `json:"name"`
		}
		if err := ginapi.Bind(c, &in); err != nil {
			return err
		}
		return ginapi.Created(c, in)
	}))
	r.GET("/health", ginapi.Handle(func(c *gin.Context) error {
		return ginapi.OK(c, map[string]bool{"ok": true})
	}))

	r.GET("/docs", ginapi.ContentSecurityPolicy(httpsec.SwaggerUIPolicy), ginapi.Handle(func(c *gin.Context) error {
		return ginapi.OK(c, map[string]bool{"docs": true})
	}))

	r.GET("/embed", ginapi.CrossOriginResourcePolicy("cross-origin"), ginapi.Handle(func(c *gin.Context) error {
		return ginapi.OK(c, map[string]bool{"embed": true})
	}))

	conformance.Run(t, r)
	conformance.RunLogs(t, r, logs)
}

func TestWithout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	logs := &conformance.Logs{}
	ginapi.Setup(r, conformance.Logger(logs), conformance.Stack(t),
		httpapi.WithoutAccessLog(), httpapi.WithoutRecovery(), httpapi.WithoutMetrics())
	r.GET("/v1/items/:id", ginapi.Handle(func(c *gin.Context) error {
		return ginapi.OK(c, map[string]string{"id": c.Param("id")})
	}))
	r.GET("/v1/panic", ginapi.Handle(func(c *gin.Context) error { panic("boom") }))

	conformance.RunWithout(t, r, logs)
}

func TestBodyLimitRejectsANonPositiveLimit(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("BodyLimit(0) did not panic")
		}
	}()
	ginapi.BodyLimit(0)
}

func TestContentSecurityPolicyRejectsAnInvalidPolicy(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("ContentSecurityPolicy(\"\") did not panic")
		}
	}()
	ginapi.ContentSecurityPolicy("")
}

// Setup leaves gin's own client-IP detection as gin, or the service, set it:
// c.ClientIP() keeps working for code that already reads it. ginapi.ClientIP
// is the stack's answer, from its own trusted proxies.
func TestSetupKeepsGinsClientIPDetection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for name, configure := range map[string]func(*gin.Engine){
		"gin's default": func(*gin.Engine) {},
		"the service's own proxies": func(r *gin.Engine) {
			if err := r.SetTrustedProxies([]string{"192.0.2.0/24"}); err != nil {
				t.Fatal(err)
			}
		},
	} {
		r := gin.New()
		configure(r)
		ginapi.Setup(r, nil, conformance.Stack(t))
		var ginIP, stackIP string
		r.GET("/ip", func(c *gin.Context) {
			ginIP, stackIP = c.ClientIP(), ginapi.ClientIP(c)
		})

		req := httptest.NewRequest(http.MethodGet, "/ip", nil) // from 192.0.2.1
		req.Header.Set("X-Forwarded-For", "203.0.113.7")
		r.ServeHTTP(httptest.NewRecorder(), req)

		if ginIP != "203.0.113.7" {
			t.Errorf("%s: c.ClientIP() = %q, want the forwarded 203.0.113.7", name, ginIP)
		}
		if stackIP != "192.0.2.1" {
			t.Errorf("%s: ginapi.ClientIP = %q, want 192.0.2.1: the stack trusts no proxy", name, stackIP)
		}
	}
}

func TestCrossOriginResourcePolicyRejectsAnInvalidPolicy(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("CrossOriginResourcePolicy(\"cross-site\") did not panic")
		}
	}()
	ginapi.CrossOriginResourcePolicy("cross-site")
}
