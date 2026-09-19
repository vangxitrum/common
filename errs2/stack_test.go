package errs2_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	errsv1 "github.com/zeebo/errs"
	"github.com/zeebo/errs/v2"

	"gitlab.internal/tuan.quang.tran/aioz-common/apierr"
	"gitlab.internal/tuan.quang.tran/aioz-common/errs2"
)

var (
	testError   = errs.Tag("test")
	testErrorV1 = errsv1.Class("test")
)

func TestFramesStartWhereTheErrorWasWrapped(t *testing.T) {
	t.Parallel()

	frames := errs2.Frames(testError.Errorf("boom"))

	require.NotEmpty(t, frames)
	require.Regexp(
		t,
		`errs2_test\.TestFramesStartWhereTheErrorWasWrapped:\d+$`,
		frames[0],
	)
	for _, frame := range frames {
		require.Regexp(t, `^\S+:\d+$`, frame)
	}
}

// The stack is reached through any wrapper, and it is the innermost one: a
// second errs wrap after a fmt.Errorf captures a new, shorter stack.
func TestFramesSurviveWrapping(t *testing.T) {
	t.Parallel()

	base := testError.Errorf("boom")
	want := errs2.Frames(base)

	for name, err := range map[string]error{
		"fmt.Errorf":         fmt.Errorf("outer: %w", base),
		"apierr":             apierr.Internal("db", base),
		"errs.Wrap":          errs.Wrap(base),
		"another errs layer": errs.Tag("outer").Wrap(fmt.Errorf("mid: %w", base)),
	} {
		require.Equal(t, want, errs2.Frames(err), name)
	}
}

// errs.Tags reports nothing for an untagged errs error, which is why Origin
// looks for the type instead.
func TestFramesFindAnUntaggedStack(t *testing.T) {
	t.Parallel()

	err := errs.Errorf("boom")

	require.Empty(t, errs.Tags(err))
	require.NotEmpty(t, errs2.Frames(err))
}

func TestOriginIsTheInnermostErrsError(t *testing.T) {
	t.Parallel()

	inner := testError.Errorf("boom")
	outer := errs.Tag("outer").Wrap(fmt.Errorf("mid: %w", inner))

	require.True(t, errs2.Origin(outer) == inner)
}

func TestNoStackWithoutAnErrsError(t *testing.T) {
	t.Parallel()

	require.Nil(t, errs2.Origin(nil))
	require.Nil(t, errs2.Frames(nil))
	require.Nil(t, errs2.Origin(errors.New("plain")))
	require.Nil(t, errs2.Frames(fmt.Errorf("wrapped: %w", errors.New("plain"))))
}

// Some callers are still on zeebo/errs v1, and a v1 error carries a stack in
// exactly the same shape, so it is read the same way.
func TestFramesReadsErrsV1(t *testing.T) {
	t.Parallel()

	frames := errs2.Frames(testErrorV1.New("boom"))

	require.NotEmpty(t, frames)
	require.Regexp(t, `errs2_test\.TestFramesReadsErrsV1:\d+$`, frames[0])
	for _, frame := range frames {
		require.Regexp(t, `^\S+:\d+$`, frame)
	}
}

// A v1 error reached through a wrapper is found too: the chain is walked by
// type, and both versions answer to it.
func TestFramesSurviveWrappingAnErrsV1Error(t *testing.T) {
	t.Parallel()

	base := testErrorV1.New("boom")

	require.Equal(t, errs2.Frames(base), errs2.Frames(fmt.Errorf("context: %w", base)))
}
