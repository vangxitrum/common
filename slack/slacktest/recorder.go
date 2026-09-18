// Package slacktest is a recording [slack.Notifier] for tests.
package slacktest

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"gitlab.internal/tuan.quang.tran/aioz-common/slack"
)

// Recorder records every message it is asked to post, and posts nothing.
//
// It is safe for concurrent use, which a service that alerts from goroutines
// needs.
type Recorder struct {
	// Fail, when set, is asked about every message; a non-nil error is
	// returned from Post and the message is not recorded. A test uses it to
	// make one channel refuse, or Slack go down.
	Fail func(slack.Message) error

	mu     sync.Mutex
	posted []slack.Message
}

var _ slack.Notifier = (*Recorder)(nil)

// Post records msg, unless Fail refuses it.
func (r *Recorder) Post(
	_ context.Context,
	msg slack.Message,
) (slack.Receipt, error) {
	if r.Fail != nil {
		if err := r.Fail(msg); err != nil {
			return slack.Receipt{}, err
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.posted = append(r.posted, msg)
	return slack.Receipt{
		Channel: msg.Channel,
		TS:      fmt.Sprintf("recorded-%d", len(r.posted)),
	}, nil
}

// Posted returns the messages posted so far, in order.
func (r *Recorder) Posted() []slack.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.posted)
}

// PostedTo returns the messages posted to one channel, in order.
func (r *Recorder) PostedTo(channel string) []slack.Message {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []slack.Message
	for _, msg := range r.posted {
		if msg.Channel == channel {
			out = append(out, msg)
		}
	}
	return out
}
