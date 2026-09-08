# aioz-stats

A shared debug and observability package for AIOZ backend services. It bundles a single HTTP debug server that exposes:

- **Monkit metrics** (p50 / p95 / p99 latencies, call counts, error rates) at `/metrics` in Prometheus format
- **pprof** CPU, heap, goroutine, and trace profiles at `/debug/pprof/`
- **Request frequency** ranked by URI at `/top`
- **Build version** JSON at `/version/`
- **Health check** at `/health`
- **Dynamic log level** at `/logging` (optional)
- **Interactive monkit viewer** at `/mon/`

---

## Installation

```bash
go get gitlab.internal/tuan.quang.tran/aioz-common
```

---

## Instrumenting your code

Use [monkit](https://github.com/spacemonkeygo/monkit) to annotate functions. One line per function is enough — monkit automatically tracks call count, error count, and latency percentiles.

```go
import "github.com/spacemonkeygo/monkit/v3"

var mon = monkit.Package() // one per package, named after the calling package

func FetchUser(ctx context.Context, id string) (_ *User, err error) {
    defer mon.Task()(&ctx)(&err) // instruments this function
    // ... your logic ...
}
```

`mon.Task()` records for every call:

| Metric field | Meaning |
|---|---|
| `success_count` | number of error-free calls |
| `error_count` | number of calls that returned a non-nil error |
| `panics` | number of panics recovered |
| `success_times` p50/p95/p99/max | latency distribution for successful calls |
| `failure_times` p50/p95/p99/max | latency distribution for failed calls |

---

## Starting the debug server

```go
package main

import (
    "context"
    "log/slog"
    "net"
    "os"

    "github.com/spacemonkeygo/monkit/v3"

    "gitlab.internal/tuan.quang.tran/aioz-common/stats/debug"
)

func main() {
    ctx := context.Background()
    log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

    ln, err := net.Listen("tcp", "0.0.0.0:6060")
    if err != nil {
        log.Error("listen failed", "err", err)
        os.Exit(1)
    }

    srv := debug.NewServer(log, ln, monkit.Default, debug.Config{
        Addr: "0.0.0.0:6060",
    })

    // Run alongside your main server
    go func() {
        if err := srv.Run(ctx); err != nil {
            log.Error("debug server", "err", err)
        }
    }()

    // ... start your main service ...
}
```

### With dynamic log level

Use `NewServerWithLevel` and pass a `*slog.LevelVar` that controls your handler's level.
The `/logging` endpoint then lets you change it at runtime without restarting.

```go
logLevel := &slog.LevelVar{} // defaults to INFO
log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))

srv := debug.NewServerWithLevel(log, ln, monkit.Default, cfg, logLevel)
```

Change the level at runtime:

```bash
# Read current level
curl http://localhost:6060/logging

# Switch to DEBUG
curl -X PUT http://localhost:6060/logging -d "DEBUG"

# Switch back to INFO
curl -X PUT http://localhost:6060/logging -d "INFO"
```

### With custom extensions

```go
type myExt struct{}

func (myExt) Description() string                          { return "My custom endpoint" }
func (myExt) Path() string                                 { return "/custom" }
func (myExt) Handler(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "hello") }

srv := debug.NewServer(log, ln, monkit.Default, cfg, myExt{})
```

### Embedding a Control Panel button

Use `debug.Cycle` to add pause/trigger/resume buttons for any background worker that implements `CycleController`:

```go
srv.Panel.Add(debug.Cycle("GC Worker", gcWorker))
```

`CycleController` requires `TriggerWait()`, `Pause()`, and `Restart()` — `sync2.Cycle` from `aioz-depin` satisfies this interface.

### Setting build version info

Pass version details at link time:

```makefile
go build -ldflags "\
  -X gitlab.internal/tuan.quang.tran/aioz-common/stats/version.Build.Release=v1.2.3 \
  -X gitlab.internal/tuan.quang.tran/aioz-common/stats/version.Build.SHA=$(git rev-parse HEAD) \
  -X gitlab.internal/tuan.quang.tran/aioz-common/stats/version.Build.Build=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
```

---

## Endpoints reference

| Endpoint | Description |
|---|---|
| `GET /` | HTML index of all endpoints |
| `GET /health` | Returns `OK` — use as liveness probe |
| `GET /version/` | Build version JSON |
| `GET /metrics` | Monkit stats in Prometheus exposition format |
| `GET /mon/` | Interactive monkit stats browser |
| `GET /top` | HTTP request counts per path, ranked by frequency |
| `GET /debug/pprof/` | pprof index |
| `GET /debug/pprof/profile?seconds=30` | CPU profile |
| `GET /debug/pprof/heap` | Heap snapshot |
| `GET /debug/pprof/goroutine` | Goroutine dump |
| `GET /debug/pprof/trace?seconds=5` | Execution trace |
| `GET /logging` | Read current log level |
| `PUT /logging` | Change log level at runtime |

---

## Prometheus integration

Point Prometheus at the `/metrics` endpoint. Example `prometheus.yml` scrape config:

```yaml
scrape_configs:
  - job_name: my-service
    static_configs:
      - targets:
          - my-service-host:6060   # debug server address
    metrics_path: /metrics
    scrape_interval: 15s
```

The metrics use the Prometheus gauge type. A typical function produces labels like:

```
# TYPE mypackage_myfunc gauge
mypackage_myfunc{scope="mypackage",kind="success_times",field="p99"} 0.0034
mypackage_myfunc{scope="mypackage",kind="success_times",field="p95"} 0.0021
mypackage_myfunc{scope="mypackage",kind="success_times",field="p50"} 0.0009
mypackage_myfunc{scope="mypackage",kind="success_count",field="count"} 4821
mypackage_myfunc{scope="mypackage",kind="error_count",field="count"} 3
```

> **Delta mode** — `/metrics` uses `DeltaTransformer`, so each scrape returns the change since the last scrape, not cumulative totals. This matches the Prometheus counter model. Use `rate()` and `increase()` in PromQL rather than raw values.

---

## Grafana integration

### 1. Add Prometheus as a data source

In Grafana → **Connections → Data sources → Add data source → Prometheus**.  
Set the URL to your Prometheus server (e.g. `http://prometheus:9090`).

### 2. Useful PromQL queries

**p99 latency for a function (seconds):**
```promql
mypackage_myfunc{field="p99"}
```

**p95 latency across all functions in a package:**
```promql
{scope="mypackage", field="p95"}
```

**Error rate (errors per second):**
```promql
rate(mypackage_myfunc{kind="error_count"}[1m])
```

**Throughput (calls per second):**
```promql
rate(mypackage_myfunc{kind="success_count"}[1m])
```

**Top slowest p99 functions:**
```promql
topk(10, {field="p99"})
```

### 3. Recommended dashboard panels

| Panel type | Query | Title |
|---|---|---|
| Time series | `{scope="mypackage", field="p99"}` | p99 latency |
| Time series | `{scope="mypackage", field="p95"}` | p95 latency |
| Stat | `rate(mypackage_myfunc{kind="error_count"}[5m])` | Error rate |
| Bar gauge | `topk(10, {field="p99"})` | Slowest functions |
| Time series | `rate({kind="success_count"}[1m])` | Throughput (req/s) |

### 4. Alerting example

Alert when p99 latency exceeds 500 ms:

```yaml
groups:
  - name: latency
    rules:
      - alert: HighP99Latency
        expr: '{field="p99"} > 0.5'
        for: 2m
        labels:
          severity: warning
        annotations:
          summary: "p99 latency above 500ms for {{ $labels.scope }}"
```

---

## pprof profiling

Capture a 30-second CPU profile directly from a running service:

```bash
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30
```

Heap snapshot:

```bash
go tool pprof http://localhost:6060/debug/pprof/heap
```

Goroutine dump (useful for detecting leaks):

```bash
curl http://localhost:6060/debug/pprof/goroutine?debug=1
```

View an execution trace in the browser:

```bash
curl -o trace.out "http://localhost:6060/debug/pprof/trace?seconds=5"
go tool trace trace.out
```
