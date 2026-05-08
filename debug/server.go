package debug

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"strings"

	"gitlab.internal/tuan.quang.tran/aioz-stats/version"

	"github.com/spacemonkeygo/monkit/v3"
	"github.com/spacemonkeygo/monkit/v3/present"
	"golang.org/x/sync/errgroup"
)

func init() {
	// zero out the http.DefaultServeMux net/http/pprof so unhelpfully
	// side-effected.
	*http.DefaultServeMux = http.ServeMux{}
}

// Config defines configuration for debug server.
type Config struct {
	Addr string `help:"address to listen on for debug endpoints" default:"127.0.0.1:0"`

	ControlTitle string `hidden:"true"`
	Control      bool   `releaseDefault:"false" devDefault:"true" hidden:"true"`

	PrometheusSanitize bool `default:"true" help:"if true, sanitize prometheus metric names and labels" hidden:"true"`

	Crawlspace bool `help:"if true, enable crawlspace on debug port" default:"false" hidden:"true"`
}

// Server provides endpoints for debugging.
type Server struct {
	log    *slog.Logger
	config Config

	listener net.Listener
	server   http.Server
	mux      http.ServeMux

	Panel *Panel

	PrometheusEndpoint *PrometheusEndpoint
	requestCounter     *requestCounter
}

// Extension is a custom endpoint, added to the debug endpoint.
type Extension interface {
	// Description is a display name for the UI.
	Description() string

	// Path is the unique HTTP path fragment.
	Path() string

	// Handler is the HTTP handler for the path.
	Handler(http.ResponseWriter, *http.Request)
}

// NewServer returns a new debug.Server.
func NewServer(log *slog.Logger, listener net.Listener, registry *monkit.Registry, config Config, extensions ...Extension) *Server {
	return NewServerWithLevel(log, listener, registry, config, nil, extensions...)
}

// NewServerWithLevel returns a new debug.Server with a dynamic log-level endpoint at /logging.
// Pass a *slog.LevelVar that controls your root slog handler's level.
func NewServerWithLevel(log *slog.Logger, listener net.Listener, registry *monkit.Registry, config Config, logLevel *slog.LevelVar, extensions ...Extension) *Server {
	server := &Server{
		log:      log,
		listener: listener,
		PrometheusEndpoint: NewPrometheusEndpointFromConfig(registry, PrometheusEndpointConfig{
			SanitizeValues: config.PrometheusSanitize,
		}),
		requestCounter: newRequestCounter(),
		config:         config,
	}

	server.server.Handler = server.requestCounter.middleware(&server.mux)

	server.Panel = NewPanel(log.With("component", "control"), "/control", config.ControlTitle)
	if config.Control {
		server.mux.Handle("/control/", server.Panel)
	}

	server.mux.Handle("/version/", http.StripPrefix("/version", newVersionHandler(log.With("component", "version"))))

	server.mux.HandleFunc("/debug/pprof/", pprof.Index)
	server.mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	server.mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	server.mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	server.mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	server.mux.HandleFunc("/top", server.requestCounter.ServeHTTP)

	server.mux.HandleFunc("/debug/run/trace/db", server.collectTraces)

	server.mux.Handle("/mon/", http.StripPrefix("/mon", http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			present.HTTP(server.PrometheusEndpoint.registryForRequest(r)).ServeHTTP(w, r)
		})))
	server.mux.HandleFunc("/metrics", server.PrometheusEndpoint.PrometheusMetrics)

	server.mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "OK")
	})

	if logLevel != nil {
		server.mux.HandleFunc("/logging", levelHandler(logLevel))
	}
	for _, ext := range extensions {
		server.mux.HandleFunc(ext.Path(), ext.Handler)
	}
	server.mux.HandleFunc("/", indexPage(config.Control, logLevel != nil, extensions))

	return server
}

func htmlLink(link string) string {
	return fmt.Sprintf("<a href=\"%s\">%s</a>", link, link)
}

func indexPage(writeControl, writeLogging bool, extensions []Extension) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		line := func(args ...string) {
			_, _ = w.Write([]byte(strings.Join(args, "") + "\n"))
		}

		w.Header().Set("Content-Type", "text/html")
		line("<html><head><title>AIOZ debug server</title></head><body>")
		defer line("</body></html>")

		line("<dl>")
		if writeControl {
			line("<dt>", htmlLink("/control/"), "</dt>")
		}
		line("<dt>", htmlLink("/version/"), "</dt>")
		line("<dd>JSON representation of the current version information for the binary</dd>")
		line("<dt>", htmlLink("/debug/pprof/"), "</dt>")
		line("<dd>HTML page listing the available Go pprof profiles</dd>")
		line("<dt>", htmlLink("/debug/pprof/symbol"), "</dt>")
		line("<dd>Looks up the program counters listed in the request, responding with a table mapping program counters to function names</dd>")
		line("<dt>", htmlLink("/debug/run/trace/db"), "</dt>")
		line("<dd>Collects traces until request is canceled</dd>")
		line("<dt>", htmlLink("/mon/"), "</dt>")
		line("<dd>HTML page listing the available Monkit metrics</dd>")
		line("<dt>", htmlLink("/metrics"), "</dt>")
		line("<dd>Monkit metrics in Prometheus format</dd>")
		line("<dt>", htmlLink("/health"), "</dt>")
		line("<dd>debugServer server health check</dd>")
		if writeLogging {
			line("<dt>", htmlLink("/logging"), "</dt>")
			line("<dd>JSON endpoint can report or change the current logging level (https://pkg.go.dev/go.uber.org/zap#AtomicLevel.ServeHTTP)</dd>")
		}
		for _, ext := range extensions {
			line("<dt>", htmlLink(ext.Path()), "</dt>")
			line("<dd>" + ext.Description() + "</dd>")
		}
		line("</dl>")
	}
}

// Run starts the debug endpoint.
func (server *Server) Run(ctx context.Context) error {
	if server.listener == nil {
		return nil
	}

	ctx, cancel := context.WithCancel(ctx)
	var group errgroup.Group

	group.Go(func() error {
		<-ctx.Done()
		return Error.Wrap(server.server.Shutdown(context.Background()))
	})
	group.Go(func() error {
		defer cancel()

		if addr := server.listener.Addr(); addr != nil {
			server.log.Info("debug server started", "addr", addr.String())
		}
		err := server.server.Serve(server.listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}

		return Error.Wrap(err)
	})

	return group.Wait()
}

// Close closes server and underlying listener.
func (server *Server) Close() error {
	return Error.Wrap(server.server.Close())
}

// collectTraces collects traces until request is canceled.
func (server *Server) collectTraces(w http.ResponseWriter, r *http.Request) {
	// TODO: Implement trace collection
	w.WriteHeader(http.StatusNotImplemented)
	w.Write([]byte("Trace collection not implemented"))
}

// sanitize formats val to be suitable for prometheus.
func sanitize(val string) string {
	// https://prometheus.io/docs/concepts/data_model/
	// specifies all metric names must match [a-zA-Z_:][a-zA-Z0-9_:]*
	// Note: The colons are reserved for user defined recording rules.
	// They should not be used by exporters or direct instrumentation.
	if val == "" {
		return ""
	}
	if '0' <= val[0] && val[0] <= '9' {
		val = "_" + val
	}
	return strings.Map(func(r rune) rune {
		switch {
		case 'a' <= r && r <= 'z':
			return r
		case 'A' <= r && r <= 'Z':
			return r
		case '0' <= r && r <= '9':
			return r
		default:
			return '_'
		}
	}, val)
}

type versionHandler struct {
	log *slog.Logger
}

func newVersionHandler(log *slog.Logger) *versionHandler {
	return &versionHandler{log}
}

func (handler *versionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	j, err := version.Build.Marshal()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	_, err = w.Write(append(j, '\n'))
	if err != nil {
		handler.log.Error("error writing version to client", "err", err)
	}
}

// levelHandler returns an HTTP handler for reading and changing the log level at runtime.
//
//	GET /logging       → returns current level (e.g. "INFO")
//	PUT /logging       → sets level from plain-text body (e.g. "DEBUG", "WARN", "ERROR")
func levelHandler(lv *slog.LevelVar) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			fmt.Fprintln(w, lv.Level())
		case http.MethodPut, http.MethodPost:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			var level slog.Level
			if err := level.UnmarshalText(bytes.TrimSpace(body)); err != nil {
				http.Error(w, "unknown level "+string(bytes.TrimSpace(body)), http.StatusBadRequest)
				return
			}
			lv.Set(level)
			fmt.Fprintln(w, lv.Level())
		default:
			http.Error(w, "use GET or PUT", http.StatusMethodNotAllowed)
		}
	}
}
