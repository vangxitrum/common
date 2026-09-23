// Package mail sends email through a provider-neutral [Sender].
//
// Four services each carried their own copy of the same Resend helper, with no
// retries, a dev mode that differed per service, and no shared test double.
// This package is the one copy: a [Sender] interface, [Resend] as the only
// implementation for now, a log provider for development, [Templates] for the
// HTML bodies, and a recording fake in mail/mailtest.
//
// A send that retries must not become two emails. [Resend] sends one
// idempotency key per [Sender.Send] - the caller's, or a random one - and
// repeats it on every attempt, so Resend drops a replay of a message it already
// accepted. It retries only what can pass: a rate limit, a request with the
// same key still in flight, a 5xx, a dropped connection. A spent quota, a bad
// key or an invalid message is reported at once as an [*Error], whose Code and
// Temporary say which it was.
//
// Nothing here logs a subject, a body, template data or an address. A
// one-time code travels in a body, and a log line is where it would leak.
package mail

import (
	"context"
	"fmt"
	netmail "net/mail"
	"time"
)

// MaxRecipients is Resend's cap on the To list of one message.
const MaxRecipients = 50

// maxIdempotencyKey is Resend's cap on an idempotency key's length.
const maxIdempotencyKey = 256

// Message is one email.
type Message struct {
	// From is the sender, as "Name <addr>". Empty uses the configured sender.
	From string

	To, Cc, Bcc []string
	ReplyTo     string

	Subject string

	// HTML and Text are the bodies; at least one is required.
	HTML, Text string

	Headers map[string]string

	// IdempotencyKey makes a repeated Send of the same message a no-op at the
	// provider for 24 hours. Empty generates one per Send, which covers the
	// retries inside it; set it to something stable - an order id, a claim key
	// - to also cover a caller that sends the same message twice.
	IdempotencyKey string
}

// Receipt is what a provider returned for an accepted message.
type Receipt struct {
	// ID is the provider's message id. Store it rather than the body: it finds
	// the message in the provider's dashboard without keeping its content.
	ID string
}

// Sender sends email.
//
// A send that should not hold up a request - a login code, a notification -
// still needs a context with a deadline, and its error still needs reporting:
//
//	go func() {
//		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
//		defer cancel()
//		if _, err := sender.Send(ctx, msg); err != nil {
//			log.ErrorContext(ctx, "sending the login code", "err", err)
//		}
//	}()
type Sender interface {
	Send(ctx context.Context, msg Message) (Receipt, error)
}

// The codes an [*Error] carries. The set is closed, so a metric can be tagged
// with one.
const (
	CodeInvalid     = "invalid"      // the message or request is wrong
	CodeAuth        = "auth"         // the API key is missing, wrong or restricted
	CodeRejected    = "rejected"     // refused, e.g. a sender domain not verified
	CodeQuota       = "quota"        // a daily or monthly sending quota is spent
	CodeRateLimited = "rate_limited" // too many requests; wait RetryAfter
	CodeConflict    = "conflict"     // the idempotency key is in flight or was reused
	CodeUnavailable = "unavailable"  // the provider failed
	CodeTransport   = "transport"    // no answer: the connection failed or dropped
)

// Error is how a send failed.
//
// It carries the provider's status and error name, never the message or its
// addresses, so it is safe to log.
type Error struct {
	Code   string
	Status int    // the provider's HTTP status; 0 when there was no answer
	Name   string // the provider's own error name, e.g. rate_limit_exceeded

	// Temporary reports whether the same send may succeed later. A send that
	// returns a temporary error has already been retried up to its budget.
	Temporary bool

	// RetryAfter is how long the provider asked the caller to wait, if it did.
	RetryAfter time.Duration

	detail string

	// cause is the error underneath, when there was one. Unwrap returns it.
	cause error
}

// Unwrap returns the error underneath, or nil when the failure originated
// here. It is what lets errors.Is and errors.As reach a transport error through the failure,
// instead of finding it flattened into the message string.
func (e *Error) Unwrap() error { return e.cause }

func (e *Error) Error() string {
	msg := "mail: " + e.Code
	if e.detail != "" {
		msg += ": " + e.detail
	}
	if e.Status != 0 {
		msg += fmt.Sprintf(" (status %d", e.Status)
		if e.Name != "" {
			msg += ", " + e.Name
		}
		msg += ")"
	}
	return msg
}

func invalid(detail string) *Error {
	return &Error{Code: CodeInvalid, detail: detail}
}

// validate refuses a message that cannot be sent, before it costs a request.
// The reasons name the field, never the address in it.
func (m Message) validate(defaultFrom string, requireFrom bool) error {
	from := m.From
	if from == "" {
		from = defaultFrom
	}
	if from == "" && requireFrom {
		return invalid("no sender")
	}
	if from != "" {
		if _, err := netmail.ParseAddress(from); err != nil {
			return invalid("the sender is not an address")
		}
	}

	if len(m.To) == 0 {
		return invalid("no recipient")
	}
	if len(m.To) > MaxRecipients {
		return invalid(
			fmt.Sprintf(
				"%d recipients is more than %d",
				len(m.To),
				MaxRecipients,
			),
		)
	}
	for _, list := range [][]string{m.To, m.Cc, m.Bcc} {
		for _, addr := range list {
			if _, err := netmail.ParseAddress(addr); err != nil {
				return invalid("a recipient is not an address")
			}
		}
	}
	if m.ReplyTo != "" {
		if _, err := netmail.ParseAddress(m.ReplyTo); err != nil {
			return invalid("the reply-to is not an address")
		}
	}

	if m.Subject == "" {
		return invalid("no subject")
	}
	if m.HTML == "" && m.Text == "" {
		return invalid("no body")
	}
	if len(m.IdempotencyKey) > maxIdempotencyKey {
		return invalid(
			fmt.Sprintf(
				"idempotency key is longer than %d characters",
				maxIdempotencyKey,
			),
		)
	}
	return nil
}

// recipients counts every address a message goes to.
func (m Message) recipients() int {
	return len(m.To) + len(m.Cc) + len(m.Bcc)
}
