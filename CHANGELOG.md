# Changelog

## Unreleased

### Added

**`depin.Storage` gains `DownloadRange`, `CreateLocalTicket` and `Account`.**
`DownloadRange(ctx, fileID, w, offset, length)` writes the plaintext bytes
`[offset, offset+length)`, so a caller that packs thumbnail sizes, HLS
segments, captions and chapters into one uploaded file reads one back without
fetching the whole pack. A negative offset or a length of zero or less is
`invalid` here rather than an `OutOfRange` a round trip later; a whole file
stays `Download`'s job.

`CreateLocalTicket(fileID, expiresAt)` signs a ticket with the identity's own
CA key and makes no coordinator call, which is what a playlist minting one link
per segment needs - an RPC per link is what pushed playlist loading past client
timeouts. It takes no context because nothing about it blocks. An identity
directory with no `ca.cert`/`ca.key` pair reports `auth`, a distinct code so
the fallback to `CreateTicket` is a check on `Code` and not a match on a
message. `Ticket.ExpiresAt` comes back truncated to the second, because the
token carries whole Unix seconds and that is what the coordinator enforces.

`Account(ctx)` returns this package's own `Account` - `ID`, `Address` (the
deposit address, EVM-style hex), `Email`, `Status`, `CreatedAt` - rather than
the SDK's protobuf record, whose id and address types are declared in
`go-sdk/internal/vo` and so cannot be named outside that module. It is a short
idempotent RPC, so it retries with the other coordinator calls. The
coordinator's `NotFound` for an identity it has no record of means an identity
that was never registered, which every caller has to treat as an auth failure,
so this one call reports it as `auth`.

`depintest.Recorder` serves ranges from `Files`, clipped to the recorded bytes,
and records them in `Ranges()`; local tickets are recorded apart from
coordinator ones in `LocallyTicketed()`, and `AccountRecord` is what `Account`
returns. The `log` provider reports a range as `not_found` like its `Download`,
mints the same stand-in token for a local ticket as for a coordinator one, and
answers `Account` with a stand-in whose address is the zero address, so a
service that reads its deposit address at startup still boots with no identity
and no coordinator.

**The go-sdk pin moves to `ef2115da4a3b`**, which is where
`DownloadFileRange`, `CreateDownloadTicketLocal` and `GetAccount` live.

**`lrucache.Options.EarlyExpiry` and `AddUntil`: a value that carries its own
expiry.** A signed URL, a download ticket or an access token expires on its own
schedule, whatever the cache's TTL says, and handing one out a second before it
dies means the caller gets it refused. `AddUntil(ctx, key, value, expiresAt)`
stores the value's own expiry and `EarlyExpiry` is how long before it the cache
stops serving the entry. The margin is exclusive, so at exactly `EarlyExpiry`
before the expiry the entry is already gone; a value that is already inside its
margin is not stored, and it still invalidates whatever it replaced, because it
is the current value and it is dead. `Expiration` still applies on top,
whichever runs out first, and `Get` reads the same predicate as `GetCached`, so
neither path serves an entry the other would refuse.

`EarlyExpiry` does nothing for an `Add` entry: `Expiration` is measured from
insertion, so subtracting a margin from it would only be a shorter
`Expiration`.

**`postgres.Config.LogQueries` and `SlowQuery`: a query log.** `db.log-queries`
logs every statement at debug; `db.slow-query` logs anything at or over a
duration at warn. A failure is an error whatever the threshold says, and a slow
statement is a warning rather than a warning and a debug line. Both are off by
default, so a pool nobody configured logs nothing. The tracer is now installed
for logging as well as for metrics, so `metrics: false` no longer turns the
query log off with it.

**Bind parameters are never logged.** pgx sends them separately from the
statement, so what is logged is the parameterized form - `WHERE email = $1` -
and the value that would identify a person stays out of the log.
`TraceQueryStart` drops `data.Args` rather than carrying them to the logger.
The statement is collapsed onto one line and capped at 2048 characters with a
`...[truncated]` marker.

**A db record is mapped to `db.*` in Elasticsearch.** A record carrying
`group=db` has its `statement`, `operation`, `rows`, pool `name` and `duration`
filed as `db.statement`, `db.operation`, `db.rows_affected`,
`db.instance.name` and `event.duration`, the way an api-group record is already
split into `http.*` and `url.*`. Left in `log.extra` they would be stored but
not mapped, so "every UPDATE over 200ms" would not be a query anyone could
write - which is the reason the statement is logged at all. The names follow
OpenTelemetry's db semantic conventions; ECS has no database fieldset. **The
matching `aioz-monitor` index template gains `db.*` and
`log.origin.stack_trace`; the two changes only work as a pair.**

**`logger.Config.StacktraceLevel` / `log.stacktrace-level`: the call stack of
the log statement.** Every record at or above the configured level carries
`callstack` - how the goroutine reached the log statement - the way zap's
`AddStacktrace` does. Blank, the default, disables it and costs nothing.

This is not `stacktrace`. That one says where a failure *began*, comes from the
error, and only exists when something in its chain is a zeebo/errs error;
`callstack` says how the process got to the line that reported it, and exists
for any record. The two answer different questions, so neither displaces the
other: both can appear on one record, each with its own console block, and
Elasticsearch files `stacktrace` under ECS `error.stack_trace` and `callstack`
under `log.origin.stack_trace`, beside the `log.origin.file.name` that
`AddSource` produces. A record that already carries a `callstack` keeps it.

The capture happens in the outermost handler, the last point still running on
the caller's goroutine - `asyncHandler` hands the record to a consumer whose
stack is its own loop. It is anchored on the record's own program counter, so
slog's dispatch and this package's handlers are dropped without encoding how
deep either is.

**`errors.Unwrap` on `depin`, `slack`, `s3client` and `mail` errors.** Each
`*Error` gains an `Unwrap`, so `errors.Is` reaches a `context.Canceled`, an
`io` error or the SDK's typed error through a failure, instead of finding it
flattened into the message string. The `*Error` is still what a caller matches
with `errors.As`.

### Changed

**`depin.Storage` is three methods wider.** Anything outside this repo
implementing it needs `DownloadRange`, `CreateLocalTicket` and `Account`;
`depintest.Recorder` and the `log` provider already have them.

**`Critical` attaches its stack as `callstack`, not `stacktrace`,** and
`logger.Stack()` returns the same trimmed `function:line` shape as every other
stack this package prints instead of its own `function\n\tfile:line` one.
Before, a `Critical` about an error overwrote that error's `stacktrace` with
its own call stack, so the failure's origin was lost exactly where it was most
wanted, and a `Critical` with no error filed a call stack as
`error.stack_trace`. **Anything querying `error.stack_trace` for Critical lines
should move to `log.origin.stack_trace`.**

### Fixed

**`lrucache.Add` grew the cache without bound and broke `Capacity`.** When it
evicted, it deleted the map entry but left the list element in place, so the
order list grew for every insert and every later eviction picked a key that was
already gone - which meant nothing was actually evicted and the map grew too.
A cache with `Capacity: 2` held 99 entries after 99 adds; `Get`'s own eviction
path was always correct, so only the `Add`/`GetCached` pair leaked. Eviction
now takes the key out of both halves and repeats until there is room, and
replacing an existing key no longer evicts a different one to make space for
it.

**`lrucache.Get` overwrote a value that `Add` had just seeded.** An entry
inserted by `Add` carried an unused `sync.Once`, so the first `Get` on that key
ran its fill function and replaced the value - and for an `AddUntil` entry
discarded the expiry it was added with, which is exactly the pattern the README
now recommends. The insert spends the once and marks the value loaded, so `Get`
serves it and fills only once the entry has really expired.

**`lrucache.Add` panicked when caching was disabled.** A non-positive
`Capacity` disables the cache, which `Get` has always honored by calling
straight through, but `Add` read the back of an empty list and dereferenced
nil. It now stores nothing and reports `replaced` as false.

**`postgres`'s bind-parameter leak test failed on the clock.** It asserted the
formatted records did not contain `42`, the non-string parameter, and two
digits turn up inside a measured duration (`1.000421ms`) or a timestamp
(`17:58:00.260342962`) often enough to fail a clean run. The parameter is now
long and distinctive and the assertion reads the message and attributes
without the wall-clock time.

**The call site reaches the sinks, so `log.origin.file.name` is no longer
always absent.** With `AddSource`, the console handler computed the caller's
`file:line` and passed it to the console writer alone; the attribute map handed
to the file, Graylog and Elasticsearch sinks never got it, so the
Elasticsearch sink's `case "source":` branch was dead in production and
`logger/README.md`'s promise of `log.origin.file.name` was not kept. A record
that logs its own `source` - the source of a payment, a feed - keeps it.


### Added

**`logger/logtest`: loggers for tests.** `New(t)` writes every record into the
test's own log, so a failing test prints what the code under test logged and a
passing one prints nothing. `NewRecorder` keeps every record - message, level
and attributes flattened to dotted keys, errors kept as errors - so a test can
assert that a line was written and with what, which is what zap's
`zaptest/observer` was used for.

**Per-component levels: `logger.Config.ComponentLevels` and
`logging.Config.CustomLevel`.** A record whose `component` attribute - the key
`lifecycle` and `stats/debug` already use - names a listed component is written
only at or above that component's level, so one noisy part of a service can be
quietened without losing the rest of its debug output. The innermost component
wins, as with a nested zap `Named`. Configured as `custom-level:
audit=warn,relay=error`.

**`logging.Config.Output`: stdout, stderr or a file path** for the console
format, and **`logger.Config.Output` / `logging.WithOutput`** to hand it a
writer directly.

**`stats/debug.NewServerWithLevelHandler`** serves `/logging` from any
`http.Handler`, for a process whose level does not live in an `*slog.LevelVar`
or that wants its own `/logging` protocol.

**`config.FindConfigDirParam`, `FindIdentityDirParam` and `FindDefaultsParam`.**
The three flags a service reads out of `os.Args` before flag parsing has
happened, so a default that expands `$CONFDIR` or a choice of default set can
be made while the flags are still being declared. Each is `FindFlagEarly` with
the name filled in.

### Changed

**`depin` hands the go-sdk the caller's `*slog.Logger` directly.** The go-sdk
now logs through `log/slog`, so the zap core that bridged its lines into the
caller's handler is gone, and this module no longer imports zap anywhere. It
stays in `go.mod` only as an indirect dependency of libp2p.

**The text console names a source as `pkg/file.go:line`**, not by the absolute
path of whatever checkout built the binary. JSON output and the sinks keep the
full path.

**`logging.Config.Validate` rejects an Elasticsearch URL with no scheme.** It
used to bind, build a sink, and then fail once per flush at the transport, with
nothing pointing back at the configuration.

**`errs2.Frames` reads a zeebo/errs v1 error too.** It matched the v2 type
alone, so a service part-way through the move between them - or one embedding a
library still on v1 - got no frames from half its errors, and with no frames
`logger.ErrorStack` has nothing to log and `error.stack_trace` is empty. Both
versions name the type the same and print the same stack, so both are matched.

**`logging.ElasticsearchConfig`'s `url` and `bearer-token` are no longer
flags.** They are `noflag:"true"`, so the endpoint and its credential come from
the config file or the environment. A bearer token passed on a command line is
readable by every process on the host.


### Changed

**A service now declares which log attributes Elasticsearch indexes, and
`log.elasticsearch.index-fields` no longer drops the rest.** The sink flattens
a record's attributes into key-value pairs, as it did, but it now writes them
to one of two places. A declared attribute goes where Elasticsearch maps it,
`labels.<name>` or an ECS field; every other attribute goes to `log.extra`,
which the index template maps with `"enabled": false`. Both are shipped and
both are in `_source`, so nothing is lost from Discover or from an API
response - the undeclared half simply adds no field to the mapping and cannot
be filtered or aggregated on. That is the point: a mapped field stays in the
mapping for ever, and an index has a field limit, a thousand by default, that a
service inventing attribute names at runtime reaches on its own.

`IndexFields` empty now takes `logger.DefaultIndexFields`, `service` and
`role`, where it used to mean "index everything". A dashboard that filtered on
an attribute nobody declared will stop matching until that attribute is listed,
and the attribute is still visible in the document meanwhile.
`GraylogConfig.IndexFields` is unchanged and still drops what it does not list.

**`log.elasticsearch.role`** is stamped on every document as `service.role`:
what this process does, such as `api`, `worker` or `job-runner`, so a service
deployed as several of them can be told apart. It is process configuration
rather than a per-record attribute, because a value that can be forgotten at a
call site will be. `Service`, `Role` and `Host` are all overridden by a record
attribute of the same name.

`source`, `error`, the stack trace and `trace_id` are always written where ECS
puts them whether or not they were declared: they describe the event rather
than a dimension of it, and a stack trace under an unmapped object is a stack
trace nobody can search for.

The matching `aioz-monitor` index template gains `log.extra`
(`"enabled": false`) and `service.role`. The two changes only work as a pair:
without the template, `log.extra` is dynamically mapped like anything else and
the field count is no more bounded than before.

### Fixed

**`logger` logs an error as its message, with a stack you can read.** The
console handler wrote an error attribute with `%v`, which dropped the
zeebo/errs stack. `json.Marshal`, used by the JSON console, the file sink and
Graylog, wrote it as `{}`, because zeebo/errs's error has no exported fields.
Now `err` is the message, and the stack of where the error began is
`stacktrace`, one trimmed `function:line` per line. The cause of an `apierr`
counts. `Critical`'s call-site stack still wins. On the console every stack
block, `Critical`'s included, is now labeled `stack:` and indented under the
line. See "Errors and their stacks" in [`logger/README.md`](logger/README.md).

### Added

**`s3client`** - objects stored in an S3-compatible bucket, ours included,
replacing the three divergent copies in `aioz-stream`, `aioz-map` and
`crawler-service`. Our own service is not AWS, and since aws-sdk-go-v2 v1.72
the SDK computes a CRC32 for every request by default: it sends
`x-amz-checksum-crc32` and `x-amz-sdk-checksum-algorithm`, and it turns a
streaming body into `aws-chunked` with a trailing checksum. Our service rejects
all of it. So request and response checksums are set to *when required*, path
style is forced, and `DeleteObjects` is given the `Content-MD5` its protocol
defines by a middleware that **swaps** the SDK's checksum middleware rather
than adding one - swapped, it inherits that middleware's position before SigV4
signs, so the header is in `SignedHeaders`; added, it would not be and the
signature would be refused. The setting is applied to the uploader as well as
to the client, which is the half of the fix `crawler-service` has and nothing
else did.

Uploads go through `feature/s3/transfermanager`, because the SDK has deprecated
`feature/s3/manager`. That changes nothing about the workaround: transfermanager
carries the same checksum setting with the same `WhenSupported` default, and its
setting **takes precedence over** the S3 client's rather than merely failing to
inherit it, so getting it wrong there is worse than before. Downloads stay on
the plain `*s3.Client`: transfermanager's `GetObject` always issues ranged GETs
from a goroutine pool and returns a body with no `Close`, which is the
connection leak this package exists to remove. transfermanager is still v0.x,
and it is confined to `Put`.

`Client` embeds the vendor's `*s3.Client` rather than hiding it behind an
interface, for the reason `redis` and `postgres` define none: `*s3.Client` has
around ninety methods and we use nine, so an interface would either lock the
rest away or grow forever. `Config.Compat` names the server, `aioz` by default
or `aws` for anything current. `aws` sets the SDK's defaults explicitly rather
than leaving them alone: both are enums whose zero value is `Unset`, and only
`config.LoadDefaultConfig` - which this package deliberately does not call, so
that `~/.aws`, the `AWS_*` environment, SSO and the EC2 metadata service cannot
change what a service talks to - resolves `Unset` into a default.

The bugs the old copies shared are closed rather than carried. The uploader is
built once in the constructor instead of once per upload, at 8MiB across four
parts rather than 100MB across five. `Get` returns an `*Object` whose `Body` is
an `io.ReadCloser`, because a bare `io.Reader` off `resp.Body` leaves the caller
unable to close the connection it was handed. A missing object is
`CodeNotFound`, which `IsNotFound` reports, rather than a `false` that also
swallowed an expired credential. The startup check is `HeadBucket`, not the
`ListBuckets` scan a bucket-scoped key is not allowed to make. Every method
takes the caller's context, in place of `context.Background()` with a hardcoded
90-second timeout. And `httpclient`'s 32MiB response cap and 30-second request
backstop are turned off here, because both are right for an API and would
truncate a download or kill a working upload.

Failures arrive as an `*Error` whose `Code` comes from a closed set and whose
`Temporary` says whether trying again could help, sharing `depin`'s and
`slack`'s vocabulary. Retries are the SDK's own, configured from `Config`, and
there is no second layer on top: an outer loop would cover only the wrapped
methods, so a call through the embedded client would behave differently without
saying so. Credentials are `secret.Token`s and nothing logs an object key. See
[`s3client/README.md`](s3client/README.md).


**`errs2.Origin` and `errs2.Frames`** - the innermost zeebo/errs error in a
chain, and its stack as a `[]string`, through `fmt.Errorf("%w")` and
`apierr`, and for untagged `errs.Errorf` too. See
[`errs2/README.md`](errs2/README.md).

**`logger.ErrorStack`, `logger.FormatStack` and `logger.ErrorAttr`** - the
same pieces for a handler built on `slog.TextHandler`, which prints an error
with `%+v` and so quotes its whole stack onto one line with every newline
escaped. `ErrorAttr` is a `ReplaceAttr` that logs the message. `ErrorStack`
trims the frames: host and owner dropped, runtime, errgroup and lifecycle
frames left out. `FormatStack` renders the block to print under the line.

**`depin`** - files stored and fetched through the depin network, behind a
provider-neutral `Storage`. The go-sdk is wrapped rather than used directly for
three reasons: its identifiers are declared in `go-sdk/internal/vo`, which Go's
internal rule puts out of reach of every other module, so ids cross the
boundary as `[]byte` and become `uuid.UUID`; it logs through a `*zap.Logger`,
which is bridged into the caller's `*slog.Logger` so the SDK's retry and
transfer lines reach the same handler; and it reports every failure as a
formatted string, so failures are classified from the gRPC status underneath
into an `*Error` whose `Code` and `Temporary` say whether trying again could
help. A `log` provider stores nothing, for development and CI, and
`depin/depintest` holds a recording fake. Only the short coordinator calls are
retried: an upload's reader cannot be rewound, and the SDK already retries its
own download pieces. See [`depin/README.md`](depin/README.md).

**Importing `depin` requires a `replace` directive.** A consuming module needs
`replace github.com/gogo/protobuf => github.com/regen-network/protobuf
v1.3.3-alpha.regen.1` in its own go.mod. Go ignores a `replace` declared by a
dependency, and the go-sdk requires one for a `gogo/protobuf` version that does
not exist upstream, so without it the build fails with `unknown revision
v1.3.3` even in a service that never touches this package.

That replacement in turn requires a pre-split `google.golang.org/genproto`,
whose `googleapis/api` packages are also in the split-out
`genproto/googleapis/api` that newer grpc requires, so `go mod tidy` stops with
`ambiguous import` wherever something pulls in grpc-gateway, as OpenTelemetry
does. This module now requires `google.golang.org/genproto` at a post-split
version, where those packages are gone and the ambiguity with them. A consumer
that hits the same error fixes it the same way.

**`slack`** - messages posted to Slack's Web API through a provider-neutral
`Notifier`, with a `log` provider for development and a recording fake in
`slack/slacktest`. `chat.postMessage` answers 200 with `{"ok": false, "error":
"..."}` for almost every failure, including a missing channel and a revoked
token, so a client that reads the status code reports those as delivered. Here
the body decides first and the status second, and a failure arrives as an
`*Error` whose `Code`, taken from a closed set, says whether trying again could
help. A rate limit's `Retry-After` is honored unless it is longer than
`httpclient.MaxRetryAfter`. There is no vendor SDK and no idempotency key: only
failures known not to have posted are retried, because Slack offers no way to
make a repeat harmless. The bot token is a `secret.Token`, and no message text
or block reaches a log line. See [`slack/README.md`](slack/README.md).

## v0.3.2 (2026-09-14)

Release note: [`.gitlab/releases/v0.3.2.md`](.gitlab/releases/v0.3.2.md).

Two `jwt` additions: a key ring loaded from a directory of PEM files, so
rotating a key is a file change on each replica, and the `typ` header, so an
access token cannot be confused with another token from the same issuer.

And the server-side controls from the OWASP cheat sheets that every service
was missing: the real client address behind a proxy, a rate limiter shared by
every replica, one way to store passwords, and security headers with a CORS
policy that is off until it is configured.

Also `mail` and `random`, a display hint for `secret` keys, and the two
`httpclient` fixes the v0.3.1 review asked for.

### Added

**`clientip`** - the address a request really came from. `X-Forwarded-For` is
walked from the right past a configured list of trusted proxies, so a client
cannot choose the address it is limited and logged by; with no trusted proxies
no header is read. `X-Real-IP` is used only when there is exactly one. `New`
refuses `0.0.0.0/0`, `::/0` and the RFC 7239 `Forwarded` header. The README
wires it into gin and echo. See [`clientip/README.md`](clientip/README.md).

**`ratelimit`** - GCRA windows in Redis (`ratelimit.WithRedis`), checked
atomically by one Lua script, or in memory with no option, for single replicas
and tests. On a Redis shared by several services, each sets its own `Prefix`,
and the server must run `noeviction`. A denied request
consumes nothing, so hammering a victim's key cannot lock them out; a long
window behind a short one is the progressive delay. `Result.RetryAfter` is
exact. Keys are hashed (no email or address in Redis), and an IPv6 client is
keyed by its /64. Every Redis call is bounded by `Timeout`, even against a
server that never answers, and a failure applies `OnError`: `local` (memory,
the default), `open` or `closed`. The README has the login, forgot-password
and OTP recipe. See [`ratelimit/README.md`](ratelimit/README.md).

**`password`** - argon2id password storage to OWASP's Password Storage cheat
sheet and NIST SP 800-63B. `New` refuses parameters below OWASP's argon2id
floor (19 MiB at 2 iterations is the default); hashes are PHC strings, decoded
strictly. `Verify` costs one derivation on every path, including an unknown
account, which is checked against a dummy hash, and compares in constant time.
Legacy bcrypt hashes verify against the raw input and, like argon2id hashes
with other parameters, report `NeedsRehash`. Passwords are NFC-normalized, and
`Check` counts characters after NFC with a minimum of 15. At most
`MaxConcurrent` derivations hold memory at once. See
[`password/README.md`](password/README.md).

**`httpsec`** - the OWASP HTTP Headers, HSTS and CORS cheat sheets, with no
framework. `Headers` sets the OWASP set (a no-HTML CSP, HSTS for two years
with subdomains, nosniff, `DENY` framing, a strict referrer policy, COOP,
CORP, a permissions policy, `no-store`) before the handler runs, so a handler
can still override one. `CORS` is off when no origins are configured; exact
origins and `https://*.example.com` subdomain wildcards match strictly, and
`NewCORS` refuses `*` with credentials, `null`, paths and TLD wildcards.
`Vary: Origin` is added, never set; configured lists are sent, never the
request's echoed; every preflight gets a 204 without reaching a handler. Both
have a stdlib `Wrap`; the README has the gin and echo middleware. See
[`httpsec/README.md`](httpsec/README.md).

**`postgres.SortColumns`** - an allowlist from the sort keys an endpoint
documents to the columns they order by, since `ORDER BY` cannot take a bind
parameter. `OrderBy(key, direction)` matches the key exactly, accepts `asc` or
`desc`, quotes the column as an identifier, and returns
`postgres.ErrInvalidSort` (a 400) for anything else. See "A sort chosen by the
request goes through an allowlist" in [`postgres/README.md`](postgres/README.md).

**Log redaction** - `logging.New` and `logging.Bootstrap` now log the value of
a sensitive attribute as `[redacted]`: `password`, `secret`, `token`,
`authorization`, `cookie`, `api_key` and their relatives, matched exactly
after lowercasing and turning `-` into `_`, and everything under a group of
that name. `logging.Config.Redact` adds keys; `logging.RedactAttr` and
`logging.RedactKeys` are exported for handlers built by hand. Underneath,
`logger.Config.ReplaceAttr` is new and is applied inside the handler to
attributes from `With`, from the context and from the record alike, before
stdout, file or Graylog. It is a safety net: values inside headers, structs
and bodies are not inspected, and `secret.Token` remains the control.

**`upload`** - the OWASP File Upload cheat sheet for a form file. A `Policy`
accepts a file only when its extension and its sniffed content name the same
allowed `Type` (`PNG`, `JPEG`, `JPG`, `GIF`, `WebP`); the client's
`Content-Type` is never read, empty files are refused, and SVG, HTML and other
active content can never be allowed. The returned `File` replays the sniffed
bytes and fails with `ErrTooLarge` past `MaxBytes` instead of truncating.
`DisplayName` reduces a client's filename to one safe to show and log, and
`LimitRequest` caps the body before anything parses it. See
[`upload/README.md`](upload/README.md).

**`debug.HealthCommand`** - a `healthcheck` subcommand that GETs the debug
server's `/health` and exits non-zero unless it answers 200, for a container
`HEALTHCHECK` in a distroless image that has no shell or `curl`. It ignores
proxy settings, follows no redirects and gives up after `--timeout`;
`debug.CheckHealth` is the same check as a function. The debug address must
be pinned for it to find: `debug.addr` defaults to a random port. See
"Container health check" in [`stats/README.md`](stats/README.md).

**`jwt.LoadKeyRing`** - reads every `<name>.pem` (private) and
`<name>.pub.pem` (public only) in a directory and returns a `KeyRing`: a
`Signer` for the named active key and a `Ring` over every key. The kid is the
key's RFC 7638 thumbprint (`jwt.Thumbprint`), so replicas agree on it with no
coordination and the same key under two names is one ring member. Keys are
Ed25519 or RSA of at least 2048 bits, and every load error names the file.
Dotfiles, including a Kubernetes mount's `..data`, are ignored; other unknown
files are reported by `Skipped`, and a private key readable by group or other
is flagged in `KeyInfo.LoosePermissions`. `DeriveSecret` derives a symmetric
secret from the active key with HKDF-SHA256. `ParsePrivateKeyPEM`,
`ParsePublicKeyPEM` and `AlgorithmFor` are exported for callers that hold keys
elsewhere. See "Rotating a key" in [`jwt/README.md`](jwt/README.md).

**`jwt.GenerateKey`, `WritePrivateKey`, `WritePublicKey`** - the pieces of a
service's `genkey` command. Files are written with modes 0600 and 0644, never
over an existing file, and only under a name `LoadKeyRing` reads back.

**`jwt.WithType` and `jwt.RequireType`** - stamp and require the `typ` header,
with `jwt.TypeAccessToken` (`at+jwt`, RFC 9068). A verifier with `RequireType`
refuses a token whose type is missing or different, which stops an ID token or
any other token signed by the same key from passing as an access token (RFC
8725 §3.11). The comparison ignores case and an `application/` prefix.

**`mail`** - one `Sender` interface in place of each service's own Resend
helper. The `resend` provider retries rate limits, an idempotent request still
in flight, 5xx responses and transport errors with exponential backoff, and
sends the same `Idempotency-Key` on every attempt, so a message goes out once;
quota exhaustion, any other 4xx and a `Retry-After` over 30s are not retried.
An empty API key is a startup error. The `log` provider, the dev and test
default, records that a mail was not sent. `ParseAddress` validates an address
(the HTML email pattern, tightened per OWASP) and gives a `Canonical` form for
dedupe; `ParseTemplates` parses `html/template` bodies from the service's
embedded files or an override directory, which may be a ConfigMap mount;
`mail/mailtest.Recorder` captures messages in tests. Nothing logs a subject,
body, address or key.

**`random`** - unbiased CSPRNG helpers: `Bytes`, `IntN`, `String(n, Alphabet)`
and `Digits(n)`, with the `Decimal`, `Base62` and `Crockford` alphabets. They
use rejection sampling and never return an error; `Digits` panics below
`MinDigits` (6, SP 800-63B). A new depguard rule, `no-math-rand`, keeps
`math/rand` to tests and retry jitter.

**`secret.Key.Hint`** - the key's prefix and last four characters
(`aioz_...w2Qe`), for a dashboard to show which key is which. `KeyRef` carries
it as `Hint`. The stored hash cannot give it back, so store the hint when the
key is minted, and never look a key up by it.

### Changed

`golang.org/x/crypto` v0.43.0 to v0.55.0, now a direct dependency (`password`
uses argon2 and bcrypt). v0.43.0 carries ten HIGH advisories that trivy flags,
fixed by v0.52.0 and v0.55.0; v0.55.0 is the newest that still builds with Go
1.25. `golang.org/x/text` moves with it to v0.41.0.

The `secret` package doc now points at `password` for passwords, and its random
generation goes through `random`, with output format and distribution
unchanged.

`secret.KeyRef` gains a `Hint` field, serialized as `"hint"`: anything that
encodes a `KeyRef` to JSON emits one more key.

`github.com/resend/resend-go/v3` v3.10.1 is a new dependency, for `mail`.

### Fixed

**`httpclient.Retry.Do`** sets the first attempt's `Idempotency-Key` on every
retry, overriding what `newRequest` builds. A key generated inside
`newRequest` used to change on every attempt, so a `POST` that timed out after
the server processed it could be taken as a second request.

**`httpclient` private-network guard** - an `allowed-networks` IPv4 prefix now
also allows that address in its NAT64 and 6to4 IPv6 forms. The block check
already looked through those forms and the allowlist did not, so an IPv6-only
host behind NAT64 was refused.

**`httpclient.ParseRetryAfter`** - a `Retry-After` of the largest integer
wrapped to -1s, turning "wait forever" into "retry now". It saturates, so
`DefaultRetryable` sees it as too long and stops.

## v0.3.1 (2026-09-11)

Release note: [`.gitlab/releases/v0.3.1.md`](.gitlab/releases/v0.3.1.md).

Two `httpclient` changes, both visible to a caller: a guard against server-side
request forgery, and retry moved out of the client to the call site.

### Changed

**`httpclient` no longer retries inside the client** (breaking). The retry
`RoundTripper`, `WithRetry`, `WithoutRetry` and the `retry`, `retry-max`,
`retry-wait-min`, `retry-wait-max` and `retry-non-idempotent` config keys are
gone. The per-call context flag failed silently - `WithRetry` on a `POST`, or on
a body that could not be rewound, did nothing - and it leaked into every call
made further down with the same context. Every call is now one attempt, and a
call site that should retry says so with `httpclient.Retry`.

To migrate: replace `client.Do(req.WithContext(httpclient.WithRetry(ctx)))`
with `Retry.Do`, building the request inside its `newRequest` function; delete
`WithoutRetry`, since the client no longer retries anyway; and remove the
`retry*` keys from config files.

### Added

**`httpclient.Retry`** - an explicit retry policy for one call site.
`Retryable(resp, err) bool` decides whether an attempt is worth another and
`Wait(attempt, resp, err) time.Duration` how long to sleep before the next, with
`DefaultRetryable`, `RetryableWithin`, `Exponential`, `Constant` and
`RetryAfter` provided. `Do` builds a fresh request per attempt, drains the
responses it discards, and returns the last response intact when the attempts
run out. See [`httpclient/README.md`](httpclient/README.md).

**`httpclient` private-network guard** - `NewOneShot`, the client for URLs
someone else supplied, refuses to connect to loopback, private, link-local and
other internal addresses, the cloud metadata endpoint among them, which closes
server-side request forgery through webhooks and callbacks. The check is on the
resolved address at dial time, so DNS rebinding and redirects into the network
are refused too, and a refusal wraps `ErrBlockedAddress` and is never retried.
`New` allows internal addresses unless `private-networks: block` is set;
`allowed-networks` exempts prefixes. A guarded transport ignores `HTTP_PROXY`.

### Migration

| Old | New |
| --- | --- |
| `client.Do(req.WithContext(httpclient.WithRetry(ctx)))` | `httpclient.Retry{...}.Do(ctx, client, newRequest)` |
| `httpclient.WithoutRetry(ctx)` | delete it; the client no longer retries |
| `retry`, `retry-max`, `retry-wait-*`, `retry-non-idempotent` config keys | delete them |
| `httpclient.NewOneShot` to an internal address | add its prefix to `allowed-networks`, or set `private-networks: allow` |

## v0.3.0 (2026-09-10)

Release note: [`.gitlab/releases/v0.3.0.md`](.gitlab/releases/v0.3.0.md).

Twelve utility packages imported from `depin`, eight new packages, and one
security rule added to `config`. The new ones cover the three dependencies every
service actually has - Postgres, outbound HTTP, and the backoff between
attempts at either. Everything is additive except the move of
`stats/version`, which leaves aliases behind.

### Added

**Twelve packages imported from depin**, with their commit history, keeping
their original package names so a later migration off depin's copies is an
import path rewrite rather than an API rewrite:

`base58`, `context2`, `date`, `errs2`, `leak`, `lrucache`, `memory`, `period`,
`readcloser`, `signal`, `slices2`, `time2`.

**`secret`** - bearer secrets that are never at rest in a replayable form.
`Token` is plaintext and redacts itself through fmt, slog, JSON and SQL;
`Hash` is what you store. Prefixed API keys carry a public lookup id, so
verification is one indexed row rather than a scan over every stored hash.
Recovery codes use Crockford base32, which folds the characters people
mistype. See [`secret/README.md`](secret/README.md).

**`jwt`** - signed tokens for a verifier that cannot reach the issuer's
database, with a shared secret or a key pair. The algorithm is pinned at
construction and the header's `alg` is never trusted, closing algorithm
confusion and `alg: none`. An expiry is required on both sides. `Ring`
rotates keys on `kid` without invalidating tokens in flight. See
[`jwt/README.md`](jwt/README.md).

**`version.SemVer`** - a comparable semantic version, tolerant on input
because internal builds disagree about the leading `v`. An untagged build now
derives `v0.0.0-dev.<unix>.g<commit>`, which sorts below every real tag.

**`version.Register`** - the build stamp as a monkit `StatSource`, so
`/metrics` carries a `version_info` series.

**`config` secret handling** - `secret:"true"` / `secret:"optional"` on a
field, and `config.CheckSecrets` at startup. See below.

**`redis`** - a configured, observable go-redis client. `Open` dials and pings,
so an address that is wrong, a password that expired or a firewall in the way
fails at startup next to the configuration that caused it rather than on the
first business request. The pool carries a monkit hook that counts a
`redis.Nil` as a miss instead of an error, timeouts and pool limits are config
fields, and `Client` embeds `*redis.Client` so no command is wrapped. Across
the estate today, three options are ever set, nothing configures a timeout,
nothing has a redis metric and nothing calls `Close`. See
[`redis/README.md`](redis/README.md).

**`rabbitmq`** - the AMQP client, replacing eight hand-rolled wrappers, three
of them stale forks of each other. `Conn` supervises a connection with
exponential backoff and reconnects indefinitely; `Topology` is re-declared on
every reconnect; `Publisher` correlates every confirm to its own delivery tag;
`Consumer` runs a bounded worker pool with a dead-letter policy and a drain
that loses nothing at shutdown. Three live bugs in the wrappers it replaces -
stolen publisher confirms, a self-deadlocking close, and a reconnect loop
watching a dead channel - cannot be written against this API. See
[`rabbitmq/README.md`](rabbitmq/README.md).

**`retry`** - exponential backoff with full jitter. The three retry loops in
`rabbitmq` were each written by hand and none of them waits a random amount of
time, so when a broker restarts every instance that was connected to it fails in
the same instant, waits the same second, and reconnects in the same instant -
and stays synchronized through every doubling. `Do` covers bounded work; `Loop`
covers a supervisor that starts its wait over after each success. What is worth
retrying is a classifier, so a bug is not retried four times; whether the caller
has given up is read from the context and never from the error, because an inner
per-attempt timeout and an outer deadline are the same
`context.DeadlineExceeded` and only the context tells them apart. The clock
comes from `time2`, so the tests advance time rather than spend it. The
`rabbitmq` loops are not yet migrated: full jitter halves their mean reconnect
wait, which is a behavior change that deserves its own. See
[`retry/README.md`](retry/README.md).

**`httpclient`** - the outbound HTTP client. `http.DefaultClient` has no
timeout, so a dependency that accepts a connection and never answers holds a
goroutine and a connection for the life of the process; `http.DefaultTransport`
keeps two idle connections per host, so at any real concurrency there is a fresh
TLS handshake on nearly every request. This is an `*http.Client` with both
fixed, plus per-request metrics whose tag set is bounded and carries no URL, a
redirect policy that refuses an https-to-http downgrade, a response body cap
that reports rather than truncating silently, and a User-Agent from the build
stamp. It is pure `net/http` - no server, no router, no framework - so it drops
into any vendored SDK that takes an `*http.Client`. Retry is a `RoundTripper`
and is **off by default**: a client that retries by default triples the load on
a dependency that is already failing, and a replayed POST corrupts data. A call
site opts in with `httpclient.WithRetry(ctx)`. See
[`httpclient/README.md`](httpclient/README.md).

**`postgres`** - a configured, observable pgx connection pool. `pgxpool.New`
returns a pool that has not connected to anything, and its idle-connection
priming is a goroutine with a discarded error, so a wrong host or an expired
password produces a process that starts cleanly and fails on the first query;
`Open` pings, so it fails next to the configuration that caused it.
`search_path`, `application_name` and the statement timeouts are sent in the
startup packet rather than issued per query, which is what stops one caller's
`SET` from leaking into the next borrower of a pooled connection. A pgx tracer
records query, batch and - the number that actually predicts a database outage -
connection-acquire latency, tagged by pool and never by SQL text; failures carry
the SQLSTATE. `DB` embeds `*pgxpool.Pool`, so no query method is wrapped and
`stdlib.OpenDBFromPool` is there for anyone who wants `database/sql`. Migrations
are deliberately out of scope. See [`postgres/README.md`](postgres/README.md).

**`uuid.UUID.Value` now writes the canonical string, not the raw bytes.** The
bytes were cheaper for a `uuid` column, which is what the column holds, and
wrong for every other one: sixteen arbitrary bytes are not valid UTF-8, so a
`text` or `varchar` column rejected the whole statement with `invalid byte
sequence for encoding "UTF8"`, and `lib/pq` refuses a `[]byte` for a `uuid`
column outright. `Value` cannot know which kind of column it is being sent to,
so it now sends the form that works for all of them. `Scan` still accepts both.
This changes what new rows contain in a `bytea` column, which nothing here
writes; a `uuid` column is unaffected, since both forms produce the same value.

**`period.Period` and `version.SemVer` can be stored.** Both are structs, and
`database/sql` has no converter for a struct, so both were unusable as columns -
a failure that arrived at the first query rather than at compile time. They now
implement `driver.Valuer` and `sql.Scanner` over their string forms, and their
zero values store as `NULL`: "no period" and "no version" are real states, and a
column that wrote them as `0000-00` or `v0.0.0` could not tell them from real
ones. `memory.Size` needed nothing - its underlying type is `int64` - and there
is now a test against a real column that says so.

**`version.Info.UserAgent`** - the build stamp as a User-Agent product token, so
every outbound integration identifies itself the same way. The commit hash is
deliberately not in it: a per-commit User-Agent fragments whatever the remote
counts by user-agent into noise.

**`stats/testcontext`** gained `Go`, `Wait` and `Cleanup` for tracking
background goroutines in a test.

### Changed

**`stats/version` moved to `version`.** It stopped being a stats concern once
it grew version comparison and a metrics source. `stats/version` remains as
type aliases with deprecation notices, so no import breaks.

**`Info.Version` is now a `SemVer`, not a `string`.** Rendering is unchanged -
it still marshals as a `v`-prefixed string - but it can now be compared. Code
that treated the field as a string needs `.String()`.

**A `secret`-tagged config field may no longer carry a `default` or
`releaseDefault`.** `Bind` panics if it does. This is a deliberate breaking
change for any struct that shipped a placeholder secret, which is the point:
a default reaches production, so a placeholder password in the source tree is
the password of every deployment that never overrode it.

To keep a local convenience value, pair `devDefault` with an explicitly empty
`releaseDefault`:

```go
Password string `secret:"true" devDefault:"dev-only" releaseDefault:""`
```

### Fixed

`stats/README.md` documented `-ldflags -X` targeting struct fields
(`...version.Build.Release=`). The linker only patches plain `string`
variables, so those flags were silently doing nothing. The corrected
invocation targets package-level variables in the new `version` package, and
uses `git describe --abbrev=0`, since bare `git describe` output sorts below
its own tag under semver rules.

### Dependencies

Three modules added, each with no dependencies of its own:
`blang/semver/v4`, `golang-jwt/jwt/v5`, and `zeebo/errs/v2` promoted to
direct use. `zeebo/errs` v1, which arrived with the imported packages, was
converted away rather than added.

The two client packages add two build dependencies and one test dependency:

| Module | Version | Build-graph cost |
| --- | --- | --- |
| `github.com/rabbitmq/amqp091-go` | v1.14.0 | none. Its one requirement is its own test dependency and is pruned out of the build list. |
| `github.com/redis/go-redis/v9` | v9.22.0 | small: `cespare/xxhash/v2`, `go.uber.org/atomic`, and `golang.org/x/sys`, which was already here. |
| `github.com/testcontainers/testcontainers-go` | v0.40.0 | heavy, and test-only: the Docker SDK, containerd, OpenTelemetry, roughly fifty modules. |

testcontainers is the one worth arguing, and it is acceptable on two grounds.
A consumer never sees it: with module-graph pruning, a test-only dependency of
an imported package does not enter the importing module's build list, `go.sum`
or `go mod tidy` output. That is a different bill from the one `base58` was
vendored to avoid, which was a build dependency. And there is no alternative
for RabbitMQ: no credible fake broker exists, `miniredis` models no pool, no
pipelines and none of the failure modes that matter, and the value of both
packages is reconnect and redelivery behaviour that only a real server
restart exercises.

### Migration

See [`docs/ADOPTING.md`](docs/ADOPTING.md).

| Old | New |
| --- | --- |
| `aioz-common/stats/version` | `aioz-common/version` |
| `info.Version` (string) | `info.Version.String()` |
| `-X .../stats/version.Build.X=` | `-X .../version.buildVersion=` |

## v0.2.0

`uuid` and `currency`; the build stamp derived from Go build info; `apierr`
trimmed to its core.

## v0.1.0

The initial module, assembled from `aioz-config`, `aioz-logger`, `aioz-stats`
and `aioz-template`'s `internal/utils`.
