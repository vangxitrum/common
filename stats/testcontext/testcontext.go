// Package testcontext provides a test-scoped context with cleanup helpers.
package testcontext

import (
	"context"
	"sync"
	"testing"
)

// Context is a context.Context tied to a test's lifetime.
type Context struct {
	context.Context
	t      *testing.T
	cancel context.CancelFunc
	group  sync.WaitGroup
}

// New returns a Context that is canceled when the test finishes.
func New(t *testing.T) *Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &Context{Context: ctx, t: t, cancel: cancel}
}

// Go runs fn in a goroutine and records a returned error as a test failure.
//
// It exists so a test does not hand-roll a WaitGroup around every background
// goroutine, and so an error from one is attributed to the test rather than
// lost: t.Error is safe to call from any goroutine, t.Fatal is not, so Go
// reports and lets the test finish rather than killing the wrong stack.
func (c *Context) Go(fn func() error) {
	c.t.Helper()
	c.group.Go(func() {
		if err := fn(); err != nil {
			c.t.Error(err)
		}
	})
}

// Wait blocks until every function passed to Go has returned.
func (c *Context) Wait() {
	c.t.Helper()
	c.group.Wait()
}

// Cleanup waits for the goroutines started with Go and then cancels the
// context. It is safe to call more than once, so `defer ctx.Cleanup()` can sit
// alongside the cancellation New already registers with t.Cleanup.
func (c *Context) Cleanup() {
	c.t.Helper()
	c.group.Wait()
	c.cancel()
}

// Check calls fn and records any returned error as a test failure.
func (c *Context) Check(fn func() error) {
	c.t.Helper()
	if err := fn(); err != nil {
		c.t.Error(err)
	}
}
