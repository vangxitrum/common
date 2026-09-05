// Package testcontext provides a test-scoped context with cleanup helpers.
package testcontext

import (
	"context"
	"testing"
)

// Context is a context.Context tied to a test's lifetime.
type Context struct {
	context.Context
	t *testing.T
}

// New returns a Context that is canceled when the test finishes.
func New(t *testing.T) *Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &Context{Context: ctx, t: t}
}

// Check calls fn and records any returned error as a test failure.
func (c *Context) Check(fn func() error) {
	c.t.Helper()
	if err := fn(); err != nil {
		c.t.Error(err)
	}
}
