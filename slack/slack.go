// Package slack posts messages to Slack through a provider-neutral [Notifier].
//
// Services that want to say something to a channel - a deploy finished, a
// payment could not be captured, a queue is backing up - were each about to
// grow their own copy of the same webhook call. This package is the one copy:
// a [Notifier] interface, [Slack] as the only real implementation, a log
// provider for development, and a recording fake in slack/slacktest.
//
// The Web API is not an HTTP-status API. chat.postMessage answers 200 with
// {"ok": false, "error": "..."} for most failures, so a client that reads only
// the status code reports a message as delivered when Slack refused it. Every
// failure here is decided by the body first and the status second, and arrives
// as an [*Error] whose Code and Temporary say what happened.
//
// Nothing here logs message text, blocks or thread ids. An alert often quotes
// the thing that went wrong, which is exactly the content that should not be
// duplicated into a second system.
package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Slack's own limits on one chat.postMessage call.
const (
	// MaxTextLen is the cap on the text field, in bytes.
	MaxTextLen = 40000

	// MaxBlocks is the cap on how many top-level Block Kit blocks one message
	// may carry.
	MaxBlocks = 50
)

// Message is one message to post.
type Message struct {
	// Channel is the channel, group or user to post to. A channel id (C…, G…,
	// D…) is preferred over a name: a name is resolved every time and changes
	// when someone renames the channel.
	Channel string

	// Text is required, even when Blocks is set. Slack uses it for the
	// notification, the sidebar preview and any client that cannot render
	// blocks, so a message with blocks and no text arrives blank on a watch.
	Text string

	// Blocks is a Block Kit array, already encoded. It is raw JSON rather than
	// a typed tree because Slack adds block and element types faster than a
	// wrapper can follow, and an unknown field here is the caller's business.
	Blocks json.RawMessage

	// ThreadTS threads this message under an earlier one, using the TS from
	// that message's [Receipt].
	ThreadTS string

	// ReplyBroadcast also posts a threaded reply to the channel. It is ignored
	// unless ThreadTS is set.
	ReplyBroadcast bool

	// Username and IconEmoji override the bot's display name and icon. Both
	// need chat:write.customize.
	Username  string
	IconEmoji string
}

// Receipt is what Slack returned for a posted message.
type Receipt struct {
	// Channel is the channel the message landed in, resolved to an id.
	Channel string

	// TS is the message timestamp, and Slack's identifier for it. Keep it to
	// thread replies under this message or to update it later.
	TS string
}

// Notifier posts messages to Slack.
//
// A post that should not hold up a request still needs a context with a
// deadline, and its error still needs reporting:
//
//	go func() {
//		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
//		defer cancel()
//		if _, err := notifier.Post(ctx, msg); err != nil {
//			log.ErrorContext(ctx, "posting the alert", "err", err)
//		}
//	}()
type Notifier interface {
	Post(ctx context.Context, msg Message) (Receipt, error)
}

// The codes an [*Error] carries. The set is closed, so a metric can be tagged
// with one.
const (
	CodeInvalid     = "invalid"      // the message is wrong and will stay wrong
	CodeAuth        = "auth"         // the token is missing, wrong, revoked or inactive
	CodeChannel     = "channel"      // no such channel, or the bot is not in it
	CodeRateLimited = "rate_limited" // too many requests; wait RetryAfter
	CodeUnavailable = "unavailable"  // Slack failed
	CodeTransport   = "transport"    // no answer: the connection failed or dropped
	CodeRejected    = "rejected"     // refused for a reason with no better code
)

// Error is how a post failed.
//
// It carries Slack's status and error name, never the message or the channel's
// content, so it is safe to log.
type Error struct {
	Code   string
	Status int    // Slack's HTTP status; 0 when there was no answer
	Name   string // Slack's own error string, e.g. channel_not_found

	// Temporary reports whether the same post may succeed later. A post that
	// returns a temporary error has already been retried up to its budget.
	Temporary bool

	// RetryAfter is how long Slack asked the caller to wait, if it did.
	RetryAfter time.Duration

	detail string
}

func (e *Error) Error() string {
	msg := "slack: " + e.Code
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

// validate refuses a message that cannot be posted, before it costs a request.
// The reasons name the field, never the text in it.
func (m Message) validate() error {
	if m.Channel == "" {
		return invalid("no channel")
	}
	if m.Text == "" {
		// Required even with blocks: it is what the notification shows.
		return invalid("no text")
	}
	if len(m.Text) > MaxTextLen {
		return invalid(fmt.Sprintf(
			"the text is longer than %d bytes", MaxTextLen,
		))
	}
	if len(m.Blocks) > 0 {
		var blocks []json.RawMessage
		if err := json.Unmarshal(m.Blocks, &blocks); err != nil {
			return invalid("the blocks are not a JSON array")
		}
		if len(blocks) > MaxBlocks {
			return invalid(fmt.Sprintf(
				"%d blocks is more than %d", len(blocks), MaxBlocks,
			))
		}
	}
	if m.ReplyBroadcast && m.ThreadTS == "" {
		return invalid("reply broadcast without a thread")
	}
	return nil
}
