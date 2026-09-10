# rabbitmq

A supervised AMQP connection, a declared topology, a publisher and a consumer.

Eight hand-rolled RabbitMQ wrappers exist across our services, three of them
stale forks of each other, and together they are about three thousand lines
implementing the same seven things seven different ways. Every one of them
reconnects on a fixed delay with no backoff. None of them configures a
heartbeat, so a half-open TCP connection goes unnoticed. Three of them have
live bugs that this package cannot express: stolen publisher confirms, a
self-deadlocking close, a reconnect loop that watches a dead channel forever,
and a publisher that races its own reconnect while publishing transient
messages.

So this package does not stop at the connection. Stopping there would let each
service delete its dial block and keep two hundred lines of publisher and
consumer. It owns everything up to the message, which is exactly the set of
features that was written independently more than once.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/rabbitmq"
```

## Usage

```go
broker := rabbitmq.New(log, cfg.RabbitMQ)
monkit.Default.ScopeNamed("rabbitmq").Chain(broker)
group.Add(lifecycle.Item{Name: "rabbitmq", Run: broker.Run, Close: broker.Close})

topology := rabbitmq.Topology{
    Exchanges:  []rabbitmq.Exchange{{Name: "events", Kind: "topic", Durable: true}},
    Queues:     []rabbitmq.Queue{{Name: "orders", Durable: true}},
    Bindings:   []rabbitmq.Binding{{Exchange: "events", Queue: "orders", RoutingKey: "data.received"}},
    DeadLetter: rabbitmq.DeadLetter{Exchange: "events.dlx", Queue: "orders.dlq"},
}

pub := rabbitmq.NewPublisher(broker, log, cfg.Publisher, topology)
monkit.Default.ScopeNamed("rabbitmq").Chain(pub)
group.Add(lifecycle.Item{Name: "publisher", Run: pub.Run, Close: pub.Close})

sub := rabbitmq.NewConsumer(broker, log, cfg.Consumer, topology,
    func(ctx context.Context, msg rabbitmq.Delivery) error {
        var order Order
        if err := json.Unmarshal(msg.Body, &order); err != nil {
            return err // malformed: dead-letter it, a retry cannot help
        }
        if err := process(ctx, order); err != nil {
            return rabbitmq.Requeue(err) // transient: let someone else try
        }
        return nil
    })
monkit.Default.ScopeNamed("rabbitmq").Chain(sub)
group.Add(lifecycle.Item{Name: "orders-consumer", Run: sub.Run, Close: sub.Close})
```

Register the connection **before** the publisher and consumer. `lifecycle`
starts items together and closes them in reverse, so the connection closes
last.

## Return `Requeue(err)` for a transient failure and a bare error for a poison message

This is the only decision the package leaves to the handler, and it is the one
it cannot make: only the handler knows whether the failure was the message or
the world.

A bare error nacks without requeue, so the message goes to the queue's
dead-letter exchange - a malformed body, a row that will never exist, anything
a retry cannot fix. `Requeue(err)` nacks with requeue, so another consumer
tries again - a database that is down, a rate limit, a timeout. Getting this
backwards gives you either an infinite redelivery loop or a dead-letter queue
full of messages that would have worked on the second attempt.

## Your topology is re-declared on every reconnect

A new connection inherits nothing, and a broker that was re-created has
nothing, so `Declare` runs again every time. That is what makes a reconnect
produce a working service rather than a connected one.

It also means changing a queue's arguments - durability, a TTL, a dead-letter
exchange - against a broker that already has the old queue fails with
`PRECONDITION_FAILED`, which kills the channel, which triggers a reconnect,
which re-declares, which fails again. It looks exactly like a network fault.
The error from `Declare` says so explicitly; the fix is to delete the queue or
match the existing declaration.

## A NACK is not a retry

A nack is the broker rejecting the message. Publishing it again produces the
same rejection, so `Publish` returns an error naming the delivery tag rather
than retrying. A channel failure *is* retried, up to `Retries`, because a new
channel is a different situation.

## A channel does not outlive its connection

`Channel` hands back a raw channel that dies with the connection and is not
re-opened. Anything that has to survive a reconnect belongs in `Session`, which
runs your function again on every new connection, with a context that is
canceled when its channel dies. `Publisher` and `Consumer` are both `Session`
with a body.

## Publishing waits rather than failing

A publish issued while the broker is away blocks until it comes back, bounded
by the caller's context. A broker restart is then a pause, not a wave of
errors. If a caller would rather fail fast, that is a context with a deadline.

## What this package does not do

Each of these exists in exactly one service, and each is a policy that belongs
to that service rather than to the transport. A repo migrating onto this
package keeps its own:

- A disk spool that replays failed publishes.
- Fire-and-forget publishing: an in-memory queue and a pool of publish workers,
  so `Publish` returns before the broker has seen anything. `Publish` here is
  synchronous and returns the broker's answer. Buffering unsent messages in
  memory is a durability decision - they are gone if the process dies - and
  that belongs to the service making it, in a goroutine of its own.
- Application-level retry with attempt counting and a diagnostic DLQ entry
  recording the attempts and the error. The *broker's* dead-letter path is
  here; a second, application-level one is not.
- The `event_id` correlation header contract between two specific services.
  `Message.Headers` and `Message.MessageID` carry it; the package does not
  define it.
- Watermill topology building, consumer-group queue naming, and treating
  `ErrAlreadyExists` as an ack.
- Named multi-consumer fan-out with per-name cancellation over non-durable
  queues.
- A request/response queue pair that hands the caller an ack function to invoke
  itself.

## API

```go
type Config struct {
    Name string

    URL      string // amqp:// or amqps://; overrides the four fields below
    Address  string
    Vhost    string
    Username string
    Password string

    TLS           bool
    TLSSkipVerify bool // test use only

    DialTimeout time.Duration
    Heartbeat   time.Duration
    ChannelMax  int
    FrameSize   int

    ReconnectDelay    time.Duration
    ReconnectMaxDelay time.Duration

    RequireOnStart bool // default false: Run supervises, so waiting is right
}

func New(log *slog.Logger, config Config) *Conn
func NewWithURL(log *slog.Logger, url string, config Config) *Conn

func (conn *Conn) Run(ctx context.Context) error
func (conn *Conn) Close() error
func (conn *Conn) Wait(ctx context.Context) (*amqp.Connection, error)
func (conn *Conn) Channel(ctx context.Context) (*amqp.Channel, error)
func (conn *Conn) Session(ctx context.Context, fn func(context.Context, *amqp.Channel) error) error
func (conn *Conn) Connected() bool
func (conn *Conn) Health(ctx context.Context) error
func (conn *Conn) Stats(cb func(key monkit.SeriesKey, field string, val float64))

type Topology struct {
    Exchanges  []Exchange
    Queues     []Queue
    Bindings   []Binding
    DeadLetter DeadLetter
}

func (topology Topology) Declare(ctx context.Context, ch *amqp.Channel) error
func (topology Topology) Validate() error

func NewPublisher(conn *Conn, log *slog.Logger, config PublisherConfig, topology Topology) *Publisher

func (pub *Publisher) Run(ctx context.Context) error
func (pub *Publisher) Close() error
func (pub *Publisher) Publish(ctx context.Context, msg Message) error
func (pub *Publisher) PublishJSON(ctx context.Context, routingKey string, v any) error
func (pub *Publisher) Stats(cb func(key monkit.SeriesKey, field string, val float64))

type Handler func(ctx context.Context, msg Delivery) error

func Requeue(err error) error
func IsRequeue(err error) bool

func NewConsumer(conn *Conn, log *slog.Logger, config ConsumerConfig, topology Topology, handler Handler) *Consumer

func (c *Consumer) Run(ctx context.Context) error
func (c *Consumer) Close() error
func (c *Consumer) Stats(cb func(key monkit.SeriesKey, field string, val float64))
```

## Tests

The container tests skip when there is no docker daemon. Set
`AIOZ_REQUIRE_DOCKER=1` to turn that skip into a failure, which is what CI
does:

```
AIOZ_REQUIRE_DOCKER=1 go test -race ./rabbitmq/...
```

The broker's own `guest` user only accepts loopback connections, so the test
container is started with a user of its own. From outside the container,
`guest` is refused with `ACCESS_REFUSED`, which reads exactly like a wrong
password.
