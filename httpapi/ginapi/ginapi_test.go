package ginapi_test

import (
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"gitlab.internal/tuan.quang.tran/aioz-common/apierr"
	"gitlab.internal/tuan.quang.tran/aioz-common/httpapi"
	"gitlab.internal/tuan.quang.tran/aioz-common/httpapi/ginapi"
	"gitlab.internal/tuan.quang.tran/aioz-common/httpapi/internal/conformance"
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
