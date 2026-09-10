package rabbitmq

import (
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
)

func TestTopologyValidate(t *testing.T) {
	t.Parallel()

	valid := Topology{
		Exchanges: []Exchange{{Name: "events", Kind: "topic", Durable: true}},
		Queues:    []Queue{{Name: "orders", Durable: true}},
		Bindings:  []Binding{{Exchange: "events", Queue: "orders", RoutingKey: "data.received"}},
	}

	for name, tc := range map[string]struct {
		topology Topology
		wantErr  string
	}{
		"valid": {topology: valid},
		"empty": {topology: Topology{}},
		"default exchange binding": {
			// The default exchange is never declared and always exists, so a
			// binding to "" is legal.
			topology: Topology{
				Queues:   []Queue{{Name: "orders"}},
				Bindings: []Binding{{Queue: "orders", RoutingKey: "orders"}},
			},
		},
		"binding to an undeclared queue": {
			topology: Topology{
				Exchanges: []Exchange{{Name: "events", Kind: "topic"}},
				Bindings:  []Binding{{Exchange: "events", Queue: "typo", RoutingKey: "k"}},
			},
			wantErr: `queue "typo"`,
		},
		"binding to an undeclared exchange": {
			topology: Topology{
				Queues:   []Queue{{Name: "orders"}},
				Bindings: []Binding{{Exchange: "typo", Queue: "orders", RoutingKey: "k"}},
			},
			wantErr: `exchange "typo"`,
		},
		"exchange with no kind": {
			topology: Topology{Exchanges: []Exchange{{Name: "events"}}},
			wantErr:  "no kind",
		},
		"unnamed queue": {
			topology: Topology{Queues: []Queue{{Durable: true}}},
			wantErr:  "queue name is empty",
		},
		"dead-letter queue with no exchange": {
			topology: Topology{DeadLetter: DeadLetter{Queue: "orders.dlq"}},
			wantErr:  "without an exchange",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := tc.topology.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// Configuring the dead-letter path once has to reach every queue, because the
// alternative is two magic strings repeated per queue - which is exactly the
// shape three of the wrappers this replaces got subtly different from each
// other.
func TestDeadLetterArgumentsAreInjected(t *testing.T) {
	t.Parallel()

	topology := Topology{
		Queues: []Queue{{Name: "orders"}},
		DeadLetter: DeadLetter{
			Exchange:   "events.dlx",
			Queue:      "orders.dlq",
			RoutingKey: "failed",
			TTL:        time.Hour,
		},
	}

	args := topology.queueArgs(topology.Queues[0])
	require.Equal(t, "events.dlx", args[argDeadLetterExchange])
	require.Equal(t, "failed", args[argDeadLetterRoutingKey])
}

func TestDeadLetterArgumentsDoNotOverrideAnExplicitOne(t *testing.T) {
	t.Parallel()

	topology := Topology{
		Queues: []Queue{{
			Name: "orders",
			Args: amqp.Table{argDeadLetterExchange: "queue.own.dlx"},
		}},
		DeadLetter: DeadLetter{Exchange: "events.dlx"},
	}

	args := topology.queueArgs(topology.Queues[0])
	require.Equal(t, "queue.own.dlx", args[argDeadLetterExchange])
}

// Injection must not write into the caller's table: a Topology is often built
// once and reused, and mutating it would make the second Declare differ from
// the first.
func TestDeadLetterInjectionDoesNotMutateTheQueue(t *testing.T) {
	t.Parallel()

	queue := Queue{Name: "orders", Args: amqp.Table{"x-max-priority": 5}}
	topology := Topology{
		Queues:     []Queue{queue},
		DeadLetter: DeadLetter{Exchange: "events.dlx"},
	}

	args := topology.queueArgs(topology.Queues[0])
	require.Equal(t, "events.dlx", args[argDeadLetterExchange])
	require.NotContains(t, queue.Args, argDeadLetterExchange)
	require.Equal(t, 5, args["x-max-priority"])
}

func TestNoDeadLetterLeavesArgsAlone(t *testing.T) {
	t.Parallel()

	topology := Topology{Queues: []Queue{{Name: "orders"}}}
	require.Nil(t, topology.queueArgs(topology.Queues[0]))
}
