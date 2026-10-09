module gitlab.internal/tuan.quang.tran/aioz-common/httpapi/echoapi

go 1.26.0

require (
	github.com/labstack/echo/v4 v4.16.0
	github.com/spacemonkeygo/monkit/v3 v3.0.25-0.20260113195619-706ad4b46206
	gitlab.internal/tuan.quang.tran/aioz-common v0.0.0-00010101000000-000000000000
)

require (
	github.com/blang/semver/v4 v4.0.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/labstack/gommon v0.5.0 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.22 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	github.com/valyala/fasttemplate v1.2.2 // indirect
	github.com/zeebo/errs/v2 v2.0.5 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

// Until aioz-common is released with httpapi; see the README's release steps.
replace gitlab.internal/tuan.quang.tran/aioz-common => ../../
