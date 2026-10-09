package echoapi_test

import (
	"errors"
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
	echoapi.Setup(e, conformance.Logger(logs), conformance.Stack(t))

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
		return echoapi.List(c, items, page.Metadata(53))
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

	conformance.Run(t, e)
	conformance.RunLogs(t, e, logs)
}
