# depin

Stores and fetches files through the depin network, behind a provider-neutral
`Storage`.

The depin go-sdk is the only implementation. It is wrapped rather than used
directly because three things about it do not fit a service in this repo. Its
identifiers are declared in `go-sdk/internal/vo`, which Go's internal rule puts
out of reach of every other module, so a service using it directly ends up
passing raw `[]byte` around. It logs through a `*zap.Logger`, and everything
here logs through `log/slog`. And it reports every failure as a formatted
string, so deciding whether to retry would mean matching on error text.

## Usage

```go
storage, err := depin.New(log, config.Depin)
if err != nil {
    return err
}
defer func() { _ = storage.Close() }()

receipt, err := storage.Upload(ctx, body, depin.UploadParams{
    Filename:   "invoice.pdf",
    Size:       size,          // required: the SDK plans segments up front
    ContractID: contractID,    // how usage is attributed and bulk deletes work
})
if err != nil {
    return err
}

err = storage.Download(ctx, receipt.FileID, w)
```

A `Storage` is meant to live as long as the process and be registered with
[`lifecycle`](../lifecycle/README.md). `Close` releases the libp2p host and the
connection pool, and building one per request would leak both.

## The SDK's identifiers do not leave this package

`vo.UUID` is `type UUID []byte`: a defined type whose underlying type is the
type literal `[]byte`. A defined type is assignable to and from its underlying
type when one side is not itself defined, so an id crosses the boundary as a
`[]byte` and becomes a [`uuid.UUID`](../uuid/README.md) here. That is the whole
reason this package can exist outside the go-sdk module, and why nothing in it
names `vo`.

The conversion is checked, not assumed: an id the coordinator returns that is
not 16 bytes is reported as `unavailable` rather than silently truncated.

## The provider is named, never inferred

`Provider` is `depin` or `log`. The `log` provider validates a call, stores
nothing, and logs that it stored nothing, so a developer can run a service with
no identity directory and no reachable coordinator.

An empty identity directory with the `depin` provider is a startup error, not a
quiet switch to storing nothing, because a production deploy that forgot it has
to fail where someone sees it. A download from the `log` provider is
`not_found` rather than an empty success, for the same reason: an empty file
looks like a working system.

## Only the short coordinator calls are retried

`CreateTicket`, `DeleteFile`, `ListPlacements` and `CreateContract` are
idempotent and cheap, so they retry with the [`retry`](../retry/README.md)
package's backoff and stop at the first permanent failure. A `not_found` costs
one round trip, not four.

`Upload` is not retried: its bytes are consumed as they are sent, and this
package cannot rewind an arbitrary `io.Reader`. `Download` is not retried
either, because the SDK already retries its own pieces and re-resolves the
manifest. A download error here means it has already given up.

## A failure says whether trying again could help

Every failure that reached the coordinator arrives as an `*Error` carrying a
`Code` from a closed set, the gRPC status it came from, and `Temporary`.
Classification reads the gRPC status rather than the message, because the SDK
wraps everything as `fmt.Errorf("sdk: <op>: %w", err)` and the strings are not
an interface. A failure that never reached the coordinator is `transport` and
temporary.

The codes are `invalid`, `auth`, `permission`, `not_found`, `conflict`,
`exhausted`, `rate_limited`, `unavailable` and `transport`. The set is closed,
so a metric can be tagged with one.

## The SDK's logs reach the caller's handler

`WithLogger` takes the caller's `*slog.Logger`, so the SDK's retry warnings,
stalled-piece reports and per-transfer summary reach the same handler, level
and redaction as the rest of the process, and the debug server's `/logging`
endpoint turns them on and off with everything else.

## Nothing here logs a filename

Not a filename, not user metadata, not file contents. A filename is often the
only description a private file has, and a log line is where it would leak.
Sizes, ids and placement names are logged; the tests assert the rest is not.

## What this package does not do

- It does not expose the SDK's protobuf types. `ListPlacements` returns this
  package's `Placement`, not `*placementpb.Placement`.
- It does not wrap account registration, usage reporting, byte-range downloads,
  ticket resolution or contract deletion. They are reachable on the SDK and can
  be added here when a service needs them.
- It does not return a reader. Downloads write to an `io.Writer` as the data
  arrives, because that is the shape the SDK offers; bridge it with `io.Pipe`.
- It does not retry uploads or downloads. See above.
- It does not manage the identity directory. The SDK loads `identity.cert`,
  `identity.key` and `piece_key.json` itself; this package carries only paths.

## API

```go
type Storage interface {
    Upload(ctx context.Context, r io.Reader, p UploadParams) (Receipt, error)
    Download(ctx context.Context, fileID uuid.UUID, w io.Writer) error
    DownloadByTicket(ctx context.Context, ticket string, w io.Writer) error
    CreateTicket(ctx context.Context, fileID uuid.UUID, expiresAt time.Time) (Ticket, error)
    DeleteFile(ctx context.Context, fileID uuid.UUID) error
    ListPlacements(ctx context.Context) ([]Placement, error)
    CreateContract(ctx context.Context, region string, placementID int32, tags map[string]string) (uuid.UUID, error)
    Close() error
}

func New(log *slog.Logger, config Config) (Storage, error)
func NewClient(log *slog.Logger, config ClientConfig) (*Client, error)
func NewLog(log *slog.Logger) Storage

type Config struct {
    Provider string
    Client   ClientConfig
}

func (c Config) Validate() error

type UploadParams struct {
    Filename      string
    Size          int64
    ContractID    uuid.UUID
    Placement     []byte
    SystemHeaders map[string]string
    UserMetadata  map[string]string
}

type Receipt struct {
    FileID        uuid.UUID
    EncryptedSize int64
}

type Ticket struct {
    Token     string
    ExpiresAt time.Time
}

type Placement struct {
    ID          int32
    Name        string
    Description string
}

type Error struct {
    Code      string
    Op        string
    GRPC      string
    Temporary bool
}

func (e *Error) Error() string
```

`depin/depintest` holds `Recorder`, a recording `Storage` for tests, with a
`Fail` hook for making one operation fail and a `Files` map for serving
downloads.

## Tests

```sh
go test ./depin/...
```

They need no network, no identity directory and no coordinator: the error
classification, the zap bridge, the configuration and the `log` provider are
all exercised in process. There is no unit test of a real upload, because the
SDK's client is a concrete type with no seam to fake; that path is covered by
running a service against a coordinator.
