# telemetry

Pushes this process's metrics to a Prometheus remote_write receiver.

It is the counterpart to [`stats/debug`](../debug)'s `/metrics`: that endpoint
waits to be scraped, which needs the debug port reachable from wherever
Prometheus runs. A service behind NAT, on a laptop, or on a VPS that only dials
outward is not reachable, so it pushes instead. Both can run at once.

```go
client, err := telemetry.NewClient(log, cfg.Metrics, monkit.Default)
if err != nil {
    return err
}
services.Add(lifecycle.Item{Name: "metrics", Run: client.Run, Close: client.Close})
```

An empty `metrics.url` disables the client, so a service that has not been
onboarded runs unchanged.

## Configuration

| key | default | what |
| --- | --- | --- |
| `metrics.url` | `""` | remote_write endpoint; empty disables everything |
| `metrics.app` | `""` | the `app` label |
| `metrics.instance` | hostname | the `instance` label |
| `metrics.role` | `""` | the `role` label |
| `metrics.interval` | `15s` | how often the registry is walked |
| `metrics.bearer-token` | `""` | `Authorization: Bearer`, wins over basic auth |
| `metrics.username` / `.password` | `""` | HTTP basic auth |
| `metrics.include-prefixes` | `""` | only push measurements with these prefixes |
| `metrics.exclude-prefixes` | `""` | drop these, applied after the include list |
| `metrics.drop-quantiles` | `true` | drop monkit's r10/r50/r90/r99/rmin/rmax fields |
| `metrics.include-default-gatherer` | `false` | also push `prometheus/client_golang`'s default registry |

A label with an empty value is not sent: Prometheus treats it as absent anyway.

## What gets pushed

Everything in the monkit registry, plus the process and runtime stats that
`NewClient` attaches through [`stats/monitor`](../monitor) - goroutines, memory,
GC, rusage, fds and uptime - so a freshly started service reports something
before it has counted anything of its own.

A monkit sample becomes one series per `(measurement, field, tags)`, with
`field` as a label, matching how `stats/debug` renders the same registry.

`drop-quantiles` is on by default because the reservoir fields are tagged per
call site and are by far the largest source of cardinality in a registry full
of `mon.Task` calls. The exact aggregates (`count`, `sum`, `min`, `max`,
`recent`) survive.

## client_golang alongside monkit

Set `metrics.include-default-gatherer`, or pass `WithGatherer` for a registry
of your own, and the client also pushes what `prometheus/client_golang` has
collected - which is how a service with gin HTTP middleware gets
`http_requests_total` into the same place as its monkit series. Histograms and
summaries are flattened exactly as their text exposition would be, `+Inf`
bucket included.

## Notes

- **No deltas.** Unlike `stats/debug`'s endpoint, which hands out
  since-last-scrape deltas, this reads raw cumulative values: remote_write
  receivers expect monotonic counters and compute `rate()` themselves.
- **No retries.** A failed push is logged and dropped rather than buffered.
  The next interval sends the current cumulative value anyway, so the only
  thing lost is resolution, and an unbounded buffer for a receiver that is
  down would be a far worse failure.
- **No generated protobuf.** The four remote_write messages are written to the
  wire by hand in `remotewrite.go`, so importing this package does not drag in
  protoc or the gogo/protobuf fork. `remotewrite_test.go` checks the encoding
  against an independent decoder.
