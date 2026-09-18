# slack

Posts messages to Slack through a provider-neutral `Notifier`.

Services that want to say something to a channel were each about to grow their
own copy of the same call. This package is the one copy: a `Notifier`, `Slack`
as the only real implementation, a `log` provider for development, and a
recording fake in `slack/slacktest`.

## Usage

```go
notifier, err := slack.New(log, config.Slack)
if err != nil {
    return err
}

_, err = notifier.Post(ctx, slack.Message{
    Text: "deploy finished: v0.3.3",
})
```

A message that names no channel goes to `DefaultChannel`, so most callers never
carry a channel id. A post that should not hold up a request still needs a
context with a deadline and its error still needs reporting; the `Notifier`
doc has the shape.

## A 200 is not a success

This is the thing the Web API gets wrong relative to every other HTTP API, and
the reason this package exists rather than a five-line helper.
`chat.postMessage` answers **200 with `{"ok": false, "error": "..."}`** for
almost every failure: a channel that does not exist, a revoked token, a message
over the length limit. A client that checks the status code reports all of
those as delivered.

So the body decides first and the status second. `ok` must be true, and the
`error` string is what chooses the code. The status is only consulted for the
answers that carry no useful body: 429, and the 5xx that come from Slack's edge
rather than the API.

## A failure says whether trying again could help

Every failure arrives as an `*Error` carrying a `Code` from a closed set,
Slack's own error string in `Name`, and `Temporary`. The codes are `invalid`,
`auth`, `channel`, `rate_limited`, `unavailable`, `transport` and `rejected`.
The set is closed, so a metric can be tagged with one.

`channel` is separate from `invalid` on purpose: `channel_not_found` and
`not_in_channel` are the two failures an operator fixes by inviting the bot,
and they should not be buried with a malformed payload.

## Retries stop where duplicates would start

A rate limit, a 5xx and a dropped connection are retried with the
[`retry`](../retry/README.md) package's backoff. A `Retry-After` is honored,
and one longer than `httpclient.MaxRetryAfter` is refused rather than obeyed,
because waiting as long as a remote asks is a way for whoever runs it to hold
this service's goroutines.

Slack offers no idempotency key for `chat.postMessage`, so a retry after a
response that was sent but not received would post the message twice. Only
failures known not to have posted are retried, which is why a 200 whose body
could not be parsed is reported rather than repeated.

## The token is a secret.Token

The bot token is wrapped in [`secret`](../secret/README.md)'s `Token` as soon
as it arrives, so it redacts itself in fmt, slog, JSON and SQL, and `Reveal` is
called once, to build the `Authorization` header. The tests assert it appears
in no log line and no error string.

## Nothing here logs message text

Not the text, not the blocks. An alert usually quotes whatever went wrong, and
a development message is the same message production sends, so copying it into
a second system is how a payload leaks. The channel, the sizes and whether the
message was threaded are logged.

## What this package does not do

- No vendor SDK. `slack-go` carries socket mode, the events API and a
  websocket client; `chat.postMessage` is one POST with a bearer token.
- No incoming webhooks. Those need no token and answer plain text, so they are
  a different client; this one is the Web API.
- No typed Block Kit. `Blocks` is raw JSON, because Slack adds block and
  element types faster than a wrapper follows.
- No reading. No `conversations.history`, no updating or deleting a message,
  no file uploads. `Receipt.TS` is kept so those can be added when something
  needs them.
- No rate-limit budget of its own. Slack's per-method tiers are not modeled;
  the client reacts to a 429 rather than predicting it.

## API

```go
type Notifier interface {
    Post(ctx context.Context, msg Message) (Receipt, error)
}

func New(log *slog.Logger, config Config) (Notifier, error)
func NewSlack(log *slog.Logger, defaultChannel string, config SlackConfig) (*Slack, error)
func NewLog(log *slog.Logger, defaultChannel string) Notifier

type Config struct {
    Provider       string
    DefaultChannel string
    Slack          SlackConfig
}

func (c Config) Validate() error

type Message struct {
    Channel        string
    Text           string
    Blocks         json.RawMessage
    ThreadTS       string
    ReplyBroadcast bool
    Username       string
    IconEmoji      string
}

type Receipt struct {
    Channel string
    TS      string
}

type Error struct {
    Code       string
    Status     int
    Name       string
    Temporary  bool
    RetryAfter time.Duration
}

func (e *Error) Error() string

const (
    MaxTextLen = 40000
    MaxBlocks  = 50
)
```

`slack/slacktest` holds `Recorder`, a recording `Notifier` for tests, with a
`Fail` hook for making one message fail and `Posted`/`PostedTo` for asserting
what was sent.

## Tests

```sh
go test ./slack/...
```

They need no network and no Slack workspace: an `httptest` server plays
`chat.postMessage` from a script, including the 200-with-`ok:false` answers,
the 429 with a `Retry-After`, and a connection that hangs up.
