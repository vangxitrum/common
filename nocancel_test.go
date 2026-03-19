package context2_test

import (
	"aioz-depin/internal/context2"
	"aioz-depin/internal/testcontext"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestWithoutCancellation(t *testing.T) {
	t.Parallel()
	ctx := testcontext.New(t)

	parent, cancel := context.WithCancel(ctx)
	cancel()

	without := context2.WithoutCancellation(parent)
	require.Equal(t, error(nil), without.Err())
	require.Equal(t, (<-chan struct{})(nil), without.Done())
}

func TestWithRetimeout(t *testing.T) {
	t.Parallel()
	ctx := testcontext.New(t)

	parent, cancel := context.WithCancel(ctx)
	cancel()

	start := time.Now()
	subctx, subcancel := context2.WithRetimeout(parent, 3*time.Second)
	<-subctx.Done()
	finish := time.Now()
	subcancel()

	require.Greater(t, finish.Sub(start), time.Second)
}
