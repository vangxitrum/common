package context2_test

import (
	"aioz-depin/internal/context2"
	"aioz-depin/internal/testcontext"
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCustomCancel(t *testing.T) {
	t.Parallel()
	ctx := testcontext.New(t)

	sub, cancel := context2.WithCustomCancel(ctx)

	require.NoError(t, sub.Err())

	go func() {
		cancel(io.EOF)
	}()
	<-sub.Done()

	require.Equal(t, io.EOF, sub.Err())
}

func TestCustomCancel_ParentCancel(t *testing.T) {
	t.Parallel()
	ctx := testcontext.New(t)

	tx, c := context.WithTimeout(ctx, 50*time.Millisecond)
	defer c()

	sub, c2 := context2.WithCustomCancel(tx)
	defer c2(nil)

	time.Sleep(100 * time.Millisecond)
	require.Error(t, sub.Err())
}
