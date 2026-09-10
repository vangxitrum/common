package rabbitmq

import (
	"strconv"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"gitlab.internal/tuan.quang.tran/aioz-common/uuid"
)

// Message is what a Publisher sends.
//
// It is deliberately smaller than amqp.Publishing: the fields left out are
// either set by this package (DeliveryMode, the timestamp) or have never been
// used by any of the services this replaces. A caller who needs one of them
// drops to Conn.Session.
type Message struct {
	// RoutingKey overrides the publisher's default for this message.
	RoutingKey string

	Body        []byte
	ContentType string
	Headers     amqp.Table

	// MessageID identifies this message. One is generated when it is empty,
	// because a message that cannot be named cannot be traced through a log,
	// and a mandatory publish needs it to match a return.
	MessageID string

	// Correlation ties a reply to a request.
	Correlation string

	// Priority is 0 to 9, and only means anything on a queue declared with
	// x-max-priority.
	Priority uint8

	// Expiration is a per-message TTL. Zero means the message does not expire
	// on its own.
	Expiration time.Duration
}

// publishing renders the message for the wire.
func (msg Message) publishing(persistent bool) (amqp.Publishing, error) {
	id := msg.MessageID
	if id == "" {
		generated, err := uuid.New()
		if err != nil {
			return amqp.Publishing{}, Error.Wrap(err)
		}
		id = generated.String()
	}

	deliveryMode := amqp.Transient
	if persistent {
		deliveryMode = amqp.Persistent
	}

	out := amqp.Publishing{
		Headers:       msg.Headers,
		ContentType:   msg.ContentType,
		DeliveryMode:  deliveryMode,
		Priority:      msg.Priority,
		CorrelationId: msg.Correlation,
		MessageId:     id,
		Timestamp:     time.Now(),
		Body:          msg.Body,
	}
	if msg.Expiration > 0 {
		out.Expiration = ttlString(msg.Expiration)
	}

	return out, nil
}

// mustUUID returns a new identifier, falling back to the time when the system
// entropy source fails - which it does not, but a publish is not the place to
// find out.
func mustUUID() string {
	id, err := uuid.New()
	if err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return id.String()
}
