# s3client

Objects stored in an S3-compatible bucket, ours included.

Our own service at `s3-storage.attoaioz.cyou` is behind real S3, and the
aws-sdk-go-v2 defaults it needs changed are not the kind you find from a stack
trace. They were rediscovered three times - `aioz-stream`, `aioz-map` and
`crawler-service` each grew their own client - and only the newest copy has all
of them. This is the one copy.

It is a thin wrapper, not an abstraction. `Client` embeds the vendor's
`*s3.Client`, so an operation this package does not wrap is still reachable and
still carries the endpoint, the path style and the checksum settings. The
wrapped operations are the ones where the raw call has a trap: a body that has
to be closed, an absence that is not an error, a batch limit, a header the
service needs.

## Usage

```go
store, err := s3client.New(log, config.S3)
if err != nil {
    return err
}
defer func() { _ = store.Close() }()

_, err = store.Put(ctx, key, r, s3client.PutParams{ContentType: "video/mp4"})

obj, err := store.Get(ctx, key)
if s3client.IsNotFound(err) {
    return nil // it is already gone
}
defer func() { _ = obj.Close() }()
io.Copy(w, obj.Body)

url, err := store.PresignGet(ctx, key, 15*time.Minute)
```

Build one and keep it. It is a `lifecycle.Item` with a `Close` and no `Run`: it
pools connections and its uploader holds part buffers, both worth reusing.

## The SDK's defaults do not work against our own S3

Since aws-sdk-go-v2 v1.72 and service/s3 v1.73, `RequestChecksumCalculation`
defaults to *when supported*. Every request then carries
`x-amz-checksum-crc32` and `x-amz-sdk-checksum-algorithm`, and a streaming body
switches to `aws-chunked` with a trailing checksum. Our service rejects all
three. It also has no per-bucket DNS, so addressing has to be path style.

`Config.Compat` names the server. `aioz`, the default, sets checksums to *when
required* on the request and the response, forces path style, and installs the
`DeleteObjects` middleware below. `aws` takes the SDK's own defaults.

`aws` sets those defaults explicitly rather than leaving them alone, and that
is not decoration: both settings are enums whose zero value is `Unset`, and
only `config.LoadDefaultConfig` turns `Unset` into the documented default. This
package builds its `aws.Config` by hand, so unset would have meant neither
profile.

## The config is built by hand, not loaded from the environment

`config.LoadDefaultConfig` reads `~/.aws/config`, the `AWS_*` environment, SSO
and the EC2 metadata service. None of those should be able to change what a
service configured from this repo's `config` package talks to, and the metadata
probe costs a timeout on a machine that is not on EC2. Skipping it also keeps
`sso`, `ssooidc`, `sts` and `feature/ec2/imds` out of this module's
dependencies.

## The uploader has its own checksum setting, and it wins

Uploads go through `feature/s3/transfermanager`, which has its own
`RequestChecksumCalculation`. Setting it on the `*s3.Client` alone is not
enough, and this is the one place where the newer uploader is *worse* than the
`feature/s3/manager` it replaces: `manager.Uploader` merely failed to inherit
the client's setting, while transfermanager's **takes precedence over** it
(`options.go:39-41`). A correctly configured client is overridden rather than
just unhelped. So the setting appears twice in `New`, with a comment at each
site.

Switching uploaders fixed nothing here. transfermanager has the same knob and
the same `WhenSupported` default; it was adopted because `feature/s3/manager`
is deprecated, not because it makes the workaround unnecessary.

## The uploader is built once, not once per upload

`aioz-stream` called `manager.NewUploader` inside every `Upload`, with a part
size of 100 MB and a concurrency of 5 - a 500 MB in-flight ceiling per object,
for HLS segments a few megabytes long. Here the uploader is built in the
constructor. The defaults are 8 MiB and 4, so 32 MiB per upload, and a segment
that fits in one part never becomes a multipart upload at all.

The multipart threshold is pinned to the part size rather than left at
transfermanager's own 16 MiB default, so "over the part size" and "becomes
multipart" mean the same thing. `TestDefaultPartSizeSplitsAtEightMiB` asserts
both sides of that from the requests the server sees, because transfermanager
keeps its options unexported and there is nothing to read back.

## DeleteObjects is signed with Content-MD5

`DeleteObjects` predates the flexible checksums and wants a `Content-MD5`. The
middleware that computes it **swaps** the SDK's checksum middleware rather than
adding one, so it inherits that middleware's position in the Finalize step,
which is before SigV4 signs. Added instead of swapped, the header would not be
in `SignedHeaders` and the service would reject the signature. `checksum_test.go`
asserts exactly that, and is the reason the test is worth keeping rather than
trusting the header alone.

The middleware is registered on the client and guarded by the operation name,
so a `DeleteObjects` made through the embedded `*s3.Client` is covered too and
every other operation keeps the SDK's own checksum behavior.

## Uploads moved to transfermanager; downloads did not

`feature/s3/manager` is deprecated in favour of `feature/s3/transfermanager`,
so uploads use the latter. Downloads stayed on the plain `*s3.Client`, and that
is deliberate.

transfermanager's `GetObject` always builds a `concurrentReader` that issues
ranged GETs from a pool of goroutines, does a `HeadObject` first, and hands
back a `Body io.Reader` with **no `Close`** on it or on the reader behind it
(`api_op_GetObject.go:265-270`). That is the connection-leak shape this package
removed from the old client, plus goroutines that only a cancelled context
stops, plus a dependency on ranged and part-numbered GETs that our own service
has never been tested against. None of that is worth it to read an HLS segment.

The consequence is that transfermanager is still v0.x - free to break its API -
and it is now in this module's dependency graph. It is confined to `Put`.

## A missing object is a code, not a false

An "exists" call returning a bool cannot tell a missing object from an expired
credential. `aioz-stream`'s did not try: it read every error as `false`, so a
cleanup job deleted nothing and reported success. Here absence is
`CodeNotFound` and `IsNotFound(err)` is the one-line form. Both `NoSuchKey` and
`NotFound` mean it, because our service uses one where AWS uses the other, and
a bare 404 with a body the SDK cannot parse means it too.

## A body you are handed is a connection you hold

`Get` returns an `*Object` whose `Body` is an `io.ReadCloser`. Returning a bare
`io.Reader` off `resp.Body`, as the old client did, leaves the caller unable to
close what it was given and the transport holding the connection until the
garbage collector takes it. `Info` is filled in before the body is read, so an
ETag is available for a later conditional delete without draining the object.

## The SDK retries, and nothing here retries on top of it

`slack` and `depin` drive `retry` themselves. This package does not, because an
outer loop would wrap only the methods here - a call through the embedded
`*s3.Client` would get different behavior, silently. One layer that covers
every call is the honest shape, and it keeps two budgets from multiplying.
`Config.MaxAttempts` and `Config.RetryWaitMax` configure the SDK's standard
retryer.

A `Put` is not retried as a whole: an `io.Reader` cannot be rewound. The
uploader does retry an individual part, which it has already buffered, and that
is the retry the SDK retryer is left enabled for.

## The response cap and the request timeout are turned off

`httpclient` defaults to a 32 MiB cap on a response body and a 30-second
backstop on a whole request. Both are right for an API and wrong for an object
store: the first truncates a download, the second kills an upload that is
making progress. This package sets both to zero and lets the caller's context
be the bound. Everything else in `Config.Client` is the caller's.

## Nothing here logs an object key

Keys carry user ids and original filenames. An `*Error` holds the operation,
the service's code and the HTTP status, never the key, and no log line prints
one. This costs something in one place and it is not hidden: a partial
`DeleteObjects` failure reports how many keys were refused, not which.

## Why this embeds *s3.Client instead of defining an interface

For the reason the root README already gives for `redis`, `rabbitmq` and
`postgres`: the moment there is an interface, every feature the underlying
library grows has to be re-exported through it. `*s3.Client` has around ninety
methods and we use nine. An interface over it would either lock the other
eighty-one away or grow forever, and a caller would type-assert back to the
concrete type the first time it needed a ranged `GetObject`.

This is also why `depin.Storage` is not reused. That interface is a capability
model - opaque file ids the coordinator mints, tickets, contracts, placements.
S3 is a namespace model: a key the caller chose, prefix listings, ETags,
conditional deletes. The intersection is `Put`, `Get` and `Delete`, and
everything else on both sides falls outside it. The vocabulary is shared
instead: the same `Error` shape and the same code names, so a service moving
between them gets a mechanical diff.

## What this package does not do

- **No bucket lifecycle.** No create, delete, policy, lifecycle rule or
  versioning. A bucket is infrastructure, not something a service makes.
- **No SSE or KMS**, no tagging, no object lock. None is used today, and an
  unused method is one nobody notices breaking.
- **No explicit multipart API.** `Put` uses multipart when it has to. A caller
  that needs to resume an interrupted upload wants the SDK directly.
- **No `PresignPost`**, the browser form-upload flavour. `PresignPut` covers
  what we do.
- **No cross-bucket operations.** One `Client` is one bucket; `Copy` copies
  within it. Two buckets means two clients, which can share an
  `httpclient.Config`.
- **`Copy` is unproven against our own service.** No repo uses `CopyObject`
  today, so if the service does not implement it this fails at the first call.
- **No CDN invalidation.** Overwriting an object does not expire a cached copy
  of it.

## API

```go
const (
    CompatAIOZ = "aioz"
    CompatAWS  = "aws"
)

type Config struct {
    Compat, Endpoint, Region, Bucket   string
    AccessKeyID, SecretAccessKey       string
    PublicURL                          string
    PartSize                           memory.Size
    Concurrency                        int
    PresignExpiry                      time.Duration
    VerifyBucket                       bool
    MaxAttempts                        int
    RetryWaitMax                       time.Duration
    Client                             httpclient.Config
}
func (config Config) Validate() error

func New(log *slog.Logger, config Config) (*Client, error)

type Client struct {
    *s3.Client
    Uploader *transfermanager.Client
    Presign  *s3.PresignClient
}

func (c *Client) Put(ctx context.Context, key string, r io.Reader, p PutParams) (ObjectInfo, error)
func (c *Client) Get(ctx context.Context, key string) (*Object, error)
func (c *Client) Stat(ctx context.Context, key string) (ObjectInfo, error)
func (c *Client) List(ctx context.Context, p ListParams) (Page, error)
func (c *Client) Delete(ctx context.Context, key string, p DeleteParams) error
func (c *Client) DeleteMany(ctx context.Context, keys []string) (int, error)
func (c *Client) DeletePrefix(ctx context.Context, prefix string) (int, error)
func (c *Client) Copy(ctx context.Context, srcKey, dstKey string, p CopyParams) (ObjectInfo, error)
func (c *Client) PresignGet(ctx context.Context, key string, expires time.Duration) (string, error)
func (c *Client) PresignPut(ctx context.Context, key string, expires time.Duration, p PutParams) (string, error)
func (c *Client) Bucket() string
func (c *Client) Close() error

func All(ctx context.Context, c *Client, p ListParams) iter.Seq2[ObjectInfo, error]

type ObjectInfo struct {
    Key          string
    Size         int64 // -1 when the service reported no length
    ETag         string
    LastModified time.Time
    ContentType  string
    CacheControl string
    Metadata     map[string]string
}
type Object     struct{ Body io.ReadCloser; Info ObjectInfo }
func (o *Object) Close() error

type PutParams    struct{ ContentType, CacheControl string; ContentLength int64; Metadata map[string]string }
type ListParams   struct{ Prefix, Delimiter string; MaxKeys int32; Cursor string }
type Page         struct{ Objects []ObjectInfo; Prefixes []string; Cursor string }
type DeleteParams struct{ IfMatch string }
type CopyParams   struct{ ReplaceMetadata bool; ContentType, CacheControl string; Metadata map[string]string }

const (
    CodeInvalid, CodeAuth, CodePermission, CodeNotFound, CodePrecondition,
    CodeTooLarge, CodeRateLimited, CodeUnavailable, CodeTransport = ...
)

type Error struct {
    Code, Op, AWS string
    Status        int
    Temporary     bool
}
func Code(err error) string
func IsNotFound(err error) bool
func IsPrecondition(err error) bool
func IsTemporary(err error) bool

const MaxKeyLen = 1024
const MaxDeleteBatch = 1000
```

## Tests

```
go test ./s3client/...
```

No Docker and no credentials. The tests run against a scripted `httptest`
server that speaks enough S3 to answer the operations here, which is the right
shape for this package: what the workarounds are about is the bytes on the
wire, and a real server would prove less about them than asserting on the
request does.

Two assertions run on **every** request any test makes - no flexible checksum
headers, and path style - so an SDK bump that reintroduces one fails the whole
suite rather than a single test. The multipart path is covered by a body larger
than the part size, which is the case the client-level checksum fix does not
reach.

What the tests cannot prove is that our own service accepts what they assert.
They pin the wire format against the behavior the service was observed to need;
confirming the service still needs it takes a request to a scratch bucket on
the real endpoint.
