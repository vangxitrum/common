// Package cycle runs a function on an interval until it is told to stop.
//
// It exists so a background chore is testable without sleeping: Trigger and
// TriggerWait fire the work on demand, so a test asserts on the result rather
// than on a timer. A chore built on a bare time.Ticker can only be tested by
// waiting for one, which is slow when it passes and flaky when it does not.
package cycle

import (
	"context"
	"sync"
	"time"
)

// Cycle calls a function immediately, then every interval.
//
// The zero value is not usable; call New.
type Cycle struct {
	interval   time.Duration
	delayStart bool

	control  chan message
	stopping chan struct{}
	stopped  chan struct{}

	once    sync.Once
	running bool
	mu      sync.Mutex
}

// message is a control instruction for a running cycle.
type message struct {
	kind kind
	done chan struct{}
}

type kind int

const (
	trigger kind = iota
	pause
	resume
)

// New returns a Cycle that fires every interval. An interval of zero or less
// disables it: Run returns immediately without ever calling the function,
// which is how a chore is switched off by configuration.
func New(interval time.Duration) *Cycle {
	return &Cycle{
		interval: interval,
		control:  make(chan message),
		stopping: make(chan struct{}),
		stopped:  make(chan struct{}),
	}
}

// SetDelayStart skips the immediate first call, so the first run happens one
// interval in. Call it before Run.
func (c *Cycle) SetDelayStart() { c.delayStart = true }

// Disabled reports whether the interval switches this cycle off.
func (c *Cycle) Disabled() bool { return c.interval <= 0 }

// Run calls fn immediately - unless SetDelayStart was used - and then every
// interval, until Close is called or ctx is done.
//
// An error from fn stops the cycle and is returned, which for a chore running
// under an errgroup takes the process down with it. That is rarely what a
// periodic job wants: a failing pass should usually be logged so the next
// interval can retry. Handle the error inside fn and return nil, and reserve a
// returned error for a fault that genuinely should stop the service.
func (c *Cycle) Run(ctx context.Context, fn func(context.Context) error) error {
	c.mu.Lock()
	c.running = true
	c.mu.Unlock()

	defer close(c.stopped)

	if c.Disabled() {
		return nil
	}

	if !c.delayStart {
		if err := fn(ctx); err != nil {
			return err
		}
	}

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	paused := false

	for {
		// Stopping wins over any pending tick, so Close is prompt even under a
		// short interval.
		select {
		case <-c.stopping:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		select {
		case <-c.stopping:
			return nil

		case <-ctx.Done():
			return ctx.Err()

		case msg := <-c.control:
			switch msg.kind {
			case trigger:
				if err := fn(ctx); err != nil {
					return err
				}
			case pause:
				if !paused {
					ticker.Stop()
					paused = true
				}
			case resume:
				if paused {
					ticker = time.NewTicker(c.interval)
					paused = false
				}
			}
			if msg.done != nil {
				close(msg.done)
			}

		case <-ticker.C:
			if err := fn(ctx); err != nil {
				return err
			}
		}
	}
}

// Close stops the cycle and waits for Run to return. It is safe to call more
// than once, and safe on a cycle that was never run.
func (c *Cycle) Close() {
	c.once.Do(func() { close(c.stopping) })

	c.mu.Lock()
	running := c.running
	c.mu.Unlock()

	if running {
		<-c.stopped
	}
}

// Trigger asks for one extra run, without waiting for it. It does nothing if
// the cycle is not running.
func (c *Cycle) Trigger() { c.send(message{kind: trigger}) }

// TriggerWait asks for one extra run and waits for it to finish. This is what
// makes a chore testable: fire the work, then assert, with no timer involved.
func (c *Cycle) TriggerWait() {
	done := make(chan struct{})
	if c.send(message{kind: trigger, done: done}) {
		<-done
	}
}

// Pause stops the ticker without stopping the cycle. Trigger still works.
func (c *Cycle) Pause() { c.send(message{kind: pause, done: make(chan struct{})}) }

// Resume restarts the ticker after Pause.
func (c *Cycle) Resume() { c.send(message{kind: resume, done: make(chan struct{})}) }

// send delivers a control message, giving up if the cycle stops first so a
// caller can never block on a cycle that has already gone away.
func (c *Cycle) send(msg message) bool {
	select {
	case c.control <- msg:
		return true
	case <-c.stopping:
		return false
	case <-c.stopped:
		return false
	}
}
