package rabbitmq

import (
	"context"
	"errors"
	"strconv"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Exchange is one exchange to declare.
type Exchange struct {
	Name       string `help:"exchange name; empty uses the default exchange"`
	Kind       string `help:"exchange type: direct, topic, fanout or headers" default:"topic"`
	Durable    bool   `help:"survive a broker restart" default:"true"`
	AutoDelete bool   `help:"delete when the last queue unbinds" default:"false"`
	Args       amqp.Table
}

// Queue is one queue to declare.
type Queue struct {
	Name       string `help:"queue name"`
	Durable    bool   `help:"survive a broker restart" default:"true"`
	AutoDelete bool   `help:"delete when the last consumer disconnects" default:"false"`
	Exclusive  bool   `help:"restrict to this connection" default:"false"`
	Args       amqp.Table
}

// Binding routes an exchange's messages into a queue.
type Binding struct {
	Exchange   string
	Queue      string
	RoutingKey string
	Args       amqp.Table
}

// DeadLetter describes a broker-side dead-letter path.
//
// When Exchange is set, Declare creates the dead-letter exchange, queue and
// binding, and adds x-dead-letter-exchange and x-dead-letter-routing-key to
// every main queue that does not already set them. That is the broker's own
// dead-lettering: a message a consumer nacks without requeue lands there
// instead of vanishing.
type DeadLetter struct {
	Exchange   string        `help:"dead-letter exchange name; empty disables dead-lettering"`
	Queue      string        `help:"dead-letter queue name" default:""`
	RoutingKey string        `help:"routing key used when a message is dead-lettered" default:""`
	TTL        time.Duration `help:"message TTL on the dead-letter queue; 0 keeps messages forever" default:"0"`
}

// Topology is everything a channel needs declared before it is useful.
//
// It is built in code rather than bound from a config file, because Args is an
// amqp.Table and the config package cannot bind one. The queue and exchange
// names belong in the service's own config, where they already are.
type Topology struct {
	Exchanges  []Exchange
	Queues     []Queue
	Bindings   []Binding
	DeadLetter DeadLetter
}

// Dead-letter queue arguments, as the broker names them.
const (
	argDeadLetterExchange   = "x-dead-letter-exchange"
	argDeadLetterRoutingKey = "x-dead-letter-routing-key"
	argMessageTTL           = "x-message-ttl"
)

// Validate reports a topology that cannot work, before anything is sent to the
// broker: a binding naming a queue or exchange that is not declared, a
// dead-letter queue with no exchange, an exchange with no kind.
func (topology Topology) Validate() error {
	exchanges := map[string]bool{}
	for _, exchange := range topology.Exchanges {
		if exchange.Name == "" {
			return Error.Errorf("exchange name is empty")
		}
		if exchange.Kind == "" {
			return Error.Errorf("exchange %q has no kind", exchange.Name)
		}
		exchanges[exchange.Name] = true
	}

	queues := map[string]bool{}
	for _, queue := range topology.Queues {
		if queue.Name == "" {
			return Error.Errorf("queue name is empty")
		}
		queues[queue.Name] = true
	}

	for _, binding := range topology.Bindings {
		if !queues[binding.Queue] {
			return Error.Errorf("binding routes to queue %q, which is not declared", binding.Queue)
		}
		// The default exchange is always there and is never declared, so an
		// empty name is legal here and nothing else is.
		if binding.Exchange != "" && !exchanges[binding.Exchange] {
			return Error.Errorf("binding uses exchange %q, which is not declared", binding.Exchange)
		}
	}

	dead := topology.DeadLetter
	if dead.Exchange == "" && (dead.Queue != "" || dead.RoutingKey != "" || dead.TTL != 0) {
		return Error.Errorf("dead-letter queue is configured without an exchange")
	}

	return nil
}

// Declare declares everything in the topology on ch, in dependency order:
// dead-letter exchange and queue first, then exchanges, queues, bindings.
//
// It is idempotent against a broker that already has them, and it is called
// again on every reconnect - a new connection inherits nothing, and a broker
// that was re-created has nothing, so this is the only thing that brings one
// back usable.
//
// Re-declaring an existing queue with different arguments is not idempotent:
// the broker kills the channel with PRECONDITION_FAILED. Since this runs on
// every reconnect, a changed durability or TTL turns into a reconnect loop
// that looks like a network fault, so that error is wrapped to say what it
// actually is.
func (topology Topology) Declare(ctx context.Context, ch *amqp.Channel) (err error) {
	defer mon.Task()(&ctx)(&err)

	if err := topology.Validate(); err != nil {
		return err
	}

	if err := topology.declareDeadLetter(ch); err != nil {
		return err
	}

	for _, exchange := range topology.Exchanges {
		err := ch.ExchangeDeclare(exchange.Name, exchange.Kind, exchange.Durable,
			exchange.AutoDelete, false, false, exchange.Args)
		if err != nil {
			return declareError(err, "exchange", exchange.Name)
		}
	}

	for _, queue := range topology.Queues {
		_, err := ch.QueueDeclare(queue.Name, queue.Durable, queue.AutoDelete,
			queue.Exclusive, false, topology.queueArgs(queue))
		if err != nil {
			return declareError(err, "queue", queue.Name)
		}
	}

	for _, binding := range topology.Bindings {
		err := ch.QueueBind(binding.Queue, binding.RoutingKey, binding.Exchange, false, binding.Args)
		if err != nil {
			return declareError(err, "binding", binding.Queue)
		}
	}

	return nil
}

// queueArgs adds the dead-letter arguments to a queue that does not already
// set them, so a service gets the broker's dead-lettering by configuring the
// path once rather than repeating two magic strings per queue.
func (topology Topology) queueArgs(queue Queue) amqp.Table {
	dead := topology.DeadLetter
	if dead.Exchange == "" {
		return queue.Args
	}
	if _, ok := queue.Args[argDeadLetterExchange]; ok {
		return queue.Args
	}

	args := amqp.Table{}
	for key, value := range queue.Args {
		args[key] = value
	}
	args[argDeadLetterExchange] = dead.Exchange
	if dead.RoutingKey != "" {
		args[argDeadLetterRoutingKey] = dead.RoutingKey
	}
	return args
}

// declareDeadLetter creates the dead-letter exchange, queue and binding. The
// dead-letter exchange is a direct one: a dead-lettered message carries the
// routing key it was published with unless RoutingKey overrides it, and direct
// is the kind that routes such a key to exactly one queue.
func (topology Topology) declareDeadLetter(ch *amqp.Channel) error {
	dead := topology.DeadLetter
	if dead.Exchange == "" {
		return nil
	}

	if err := ch.ExchangeDeclare(dead.Exchange, "topic", true, false, false, false, nil); err != nil {
		return declareError(err, "dead-letter exchange", dead.Exchange)
	}
	if dead.Queue == "" {
		return nil
	}

	args := amqp.Table{}
	if dead.TTL > 0 {
		args[argMessageTTL] = dead.TTL.Milliseconds()
	}

	if _, err := ch.QueueDeclare(dead.Queue, true, false, false, false, args); err != nil {
		return declareError(err, "dead-letter queue", dead.Queue)
	}

	// An empty routing key on a topic exchange matches only an empty key, so
	// a dead-letter path with no explicit key binds "#" and catches whatever
	// the message was originally routed with.
	key := dead.RoutingKey
	if key == "" {
		key = "#"
	}
	if err := ch.QueueBind(dead.Queue, key, dead.Exchange, false, nil); err != nil {
		return declareError(err, "dead-letter binding", dead.Queue)
	}

	return nil
}

// declareError says what a broker refusal means, because the raw one does not.
func declareError(err error, kind, name string) error {
	var amqpErr *amqp.Error
	if errors.As(err, &amqpErr) && amqpErr.Code == amqp.PreconditionFailed {
		return Error.Errorf(
			"%s %q already exists with different arguments (%s). "+
				"The topology is re-declared on every reconnect, so this will "+
				"retry forever: delete the %s or change the declaration to match: %w",
			kind, name, amqpErr.Reason, kind, err)
	}
	return Error.Errorf("declaring %s %q: %w", kind, name, err)
}

// ttlString renders a duration the way the broker's per-message expiration
// field wants it: whole milliseconds, as a string.
func ttlString(d time.Duration) string {
	return strconv.FormatInt(d.Milliseconds(), 10)
}
