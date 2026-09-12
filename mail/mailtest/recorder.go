// Package mailtest is a recording [mail.Sender] for tests.
package mailtest

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"gitlab.internal/tuan.quang.tran/aioz-common/mail"
)

// Recorder records every message it is asked to send, and sends nothing.
//
// It is safe for concurrent use, which a service that sends from goroutines
// needs.
type Recorder struct {
	// Fail, when set, is asked about every message; a non-nil error is
	// returned from Send and the message is not recorded. A test uses it to
	// make one recipient bounce, or the provider go down.
	Fail func(mail.Message) error

	mu   sync.Mutex
	sent []mail.Message
}

// Send records msg, unless Fail refuses it.
func (r *Recorder) Send(
	_ context.Context,
	msg mail.Message,
) (mail.Receipt, error) {
	if r.Fail != nil {
		if err := r.Fail(msg); err != nil {
			return mail.Receipt{}, err
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, msg)
	return mail.Receipt{ID: fmt.Sprintf("recorded-%d", len(r.sent))}, nil
}

// Sent returns the messages sent so far, in order.
func (r *Recorder) Sent() []mail.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.sent)
}
