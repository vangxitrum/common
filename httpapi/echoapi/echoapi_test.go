package echoapi_test

import (
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"

	"gitlab.internal/tuan.quang.tran/aioz-common/apierr"
	"gitlab.internal/tuan.quang.tran/aioz-common/httpapi"
	"gitlab.internal/tuan.quang.tran/aioz-common/httpapi/echoapi"
	"gitlab.internal/tuan.quang.tran/aioz-common/httpapi/internal/conformance"
)

func TestConformance(t *testing.T) {
	e := echo.New()
	logs := &conformance.Logs{}
	echoapi.Setup(e, conformance.Logger(logs), conformance.Stack(t), conformance.Options...)

	v1 := e.Group("/v1")
	v1.GET("/items/:id", func(c echo.Context) error {
		if c.Param("id") == "missing" {
			return apierr.NotFound("item-not-found", "no such item")
		}
		return echoapi.OK(c, map[string]string{"id": c.Param("id")})
	})
	v1.GET("/items", func(c echo.Context) error {
		page, err := echoapi.Page(c, httpapi.PageConfig{})
		if err != nil {
			return err
		}
		items := []map[string]string{{"id": "a"}, {"id": "b"}}
		return echoapi.List(c, "items", items, 53, page)
	})
	v1.GET("/cached", func(c echo.Context) error {
		if echoapi.CheckETag(c, httpapi.ETag("v1")) {
			return nil
		}
		return echoapi.OK(c, map[string]int{"v": 1})
	}, echoapi.Cache(httpapi.CachePolicy{Type: httpapi.Revalidate}))
	v1.GET("/public", func(c echo.Context) error {
		return echoapi.OK(c, map[string]int{"v": 1})
	}, echoapi.Cache(conformance.Public))
	v1.GET("/public-fail", func(c echo.Context) error {
		return apierr.Conflict("busy", "try later")
	}, echoapi.Cache(conformance.Public))
	v1.GET(
		"/plain",
		func(c echo.Context) error { return errors.New("db down: host 10.0.0.5") },
	)
	v1.GET("/panic", func(c echo.Context) error { panic("boom") })
	v1.GET("/limited", func(c echo.Context) error {
		return apierr.TooManyRequests("slow-down", "too many requests")
	})
	v1.POST("/password", func(c echo.Context) error {
		return echoapi.Send(c, http.StatusOK, httpapi.Message("password changed"))
	})
	v1.POST("/publish", func(c echo.Context) error {
		return echoapi.Send(c, http.StatusCreated,
			httpapi.Success(map[string]string{"id": "v1"}).WithMessage("video published"))
	})
	v1.POST("/echo", func(c echo.Context) error {
		var in struct {
			Name string `json:"name"`
		}
		if err := echoapi.Bind(c, &in); err != nil {
			return err
		}
		return echoapi.Created(c, in)
	})

	v1.POST("/lenient", func(c echo.Context) error {
		var in struct {
			Name string `json:"name"`
		}
		if err := echoapi.Bind(c, &in, httpapi.AllowUnknownFields()); err != nil {
			return err
		}
		return echoapi.Created(c, in)
	})
	v1.POST("/upload", func(c echo.Context) error {
		n, err := io.Copy(io.Discard, c.Request().Body)
		if err != nil {
			return err
		}
		return echoapi.OK(c, map[string]int64{"bytes": n})
	}, echoapi.BodyLimit(conformance.UploadLimit))
	v1.POST("/webhook", func(c echo.Context) error {
		var in struct {
			Name string `json:"name"`
		}
		if err := echoapi.Bind(c, &in); err != nil {
			return err
		}
		return echoapi.Created(c, in)
	}, echoapi.BodyLimit(conformance.WebhookLimit))
	e.GET("/health", func(c echo.Context) error {
		return echoapi.OK(c, map[string]bool{"ok": true})
	})

	conformance.Run(t, e)
	conformance.RunLogs(t, e, logs)
}

func TestWithout(t *testing.T) {
	e := echo.New()
	logs := &conformance.Logs{}
	echoapi.Setup(e, conformance.Logger(logs), conformance.Stack(t),
		httpapi.WithoutAccessLog(), httpapi.WithoutRecovery(), httpapi.WithoutMetrics())
	e.GET("/v1/items/:id", func(c echo.Context) error {
		return echoapi.OK(c, map[string]string{"id": c.Param("id")})
	})
	e.GET("/v1/panic", func(c echo.Context) error { panic("boom") })

	conformance.RunWithout(t, e, logs)
}

func TestBodyLimitRejectsANonPositiveLimit(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("BodyLimit(0) did not panic")
		}
	}()
	echoapi.BodyLimit(0)
}
