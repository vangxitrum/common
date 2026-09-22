package depin

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"gitlab.internal/tuan.quang.tran/aioz-common/retry"
)

// classify is what decides whether a caller may try again, so every gRPC code
// the coordinator can return is pinned here rather than left to the default.
func TestClassifyMapsGRPCCodes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		code      codes.Code
		want      string
		temporary bool
	}{
		{codes.InvalidArgument, CodeInvalid, false},
		{codes.FailedPrecondition, CodeInvalid, false},
		{codes.OutOfRange, CodeInvalid, false},
		{codes.Unimplemented, CodeInvalid, false},
		{codes.Unauthenticated, CodeAuth, false},
		{codes.PermissionDenied, CodePermission, false},
		{codes.NotFound, CodeNotFound, false},
		{codes.AlreadyExists, CodeConflict, false},
		{codes.Aborted, CodeConflict, true},
		{codes.ResourceExhausted, CodeExhausted, true},
		{codes.Unavailable, CodeUnavailable, true},
		{codes.Internal, CodeUnavailable, true},
		{codes.DataLoss, CodeUnavailable, true},
		{codes.Unknown, CodeUnavailable, true},
		{codes.DeadlineExceeded, CodeTransport, true},
		{codes.Canceled, CodeTransport, true},
	} {
		t.Run(tc.code.String(), func(t *testing.T) {
			t.Parallel()

			c := &Client{}
			err := c.classify("probe", status.Error(tc.code, "boom"))

			require.Equal(t, tc.want, err.Code)
			require.Equal(t, tc.temporary, err.Temporary)
			require.Equal(t, "probe", err.Op)
			require.Equal(t, tc.code.String(), err.GRPC)
		})
	}
}

// The SDK wraps every failure as fmt.Errorf("sdk: <op>: %w", err), so the
// status has to survive being wrapped or classify would see none of them.
func TestClassifyFindsAStatusThroughWrapping(t *testing.T) {
	t.Parallel()

	wrapped := errors.Join(
		errors.New("sdk: upload"),
		status.Error(codes.NotFound, "no such file"),
	)

	got := (&Client{}).classify("upload", wrapped)
	require.Equal(t, CodeNotFound, got.Code)
	require.False(t, got.Temporary)
}

// An error with no status never reached the coordinator, so it is worth
// another attempt.
func TestClassifyTreatsAStatuslessErrorAsTransport(t *testing.T) {
	t.Parallel()

	got := (&Client{}).classify("download", errors.New("connection reset"))
	require.Equal(t, CodeTransport, got.Code)
	require.True(t, got.Temporary)
}

// A canceled or timed-out context is the caller's doing, not the
// coordinator's, and must not be reported as a storage fault.
func TestClassifyKeepsContextErrorsSeparate(t *testing.T) {
	t.Parallel()

	for name, err := range map[string]error{
		"canceled": context.Canceled,
		"deadline": context.DeadlineExceeded,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := (&Client{}).classify("upload", err)
			require.Equal(t, CodeTransport, got.Code)
			require.True(t, got.Temporary)
		})
	}
}

// The message names the operation and the code, and carries the gRPC status,
// because that is all an operator gets in a log line.
func TestErrorMessage(t *testing.T) {
	t.Parallel()

	err := (&Client{}).classify(
		"list placements",
		status.Error(codes.NotFound, "gone"),
	)
	require.Contains(t, err.Error(), "depin: list placements: not_found")
	require.Contains(t, err.Error(), "grpc NotFound")
}

func TestOrDefault(t *testing.T) {
	t.Parallel()

	require.Equal(t, 4, orDefault(0, 4))
	require.Equal(t, 4, orDefault(-1, 4))
	require.Equal(t, 7, orDefault(7, 4))
	require.Equal(t, time.Second, orDefault(time.Duration(0), time.Second))
	require.Equal(t, 2*time.Second, orDefault(2*time.Second, time.Second))
}

func TestUploadParamsValidate(t *testing.T) {
	t.Parallel()

	require.NoError(t, UploadParams{Filename: "a", Size: 1}.validate())

	var e *Error
	require.ErrorAs(t, UploadParams{Filename: "a"}.validate(), &e)
	require.Equal(t, CodeInvalid, e.Code)

	require.ErrorAs(t, UploadParams{Size: 1}.validate(), &e)
	require.Equal(t, CodeInvalid, e.Code)
}

// The coordinator answers NotFound for an identity it has no record of, which
// is an identity that was never registered. Every caller has to treat that as
// an auth failure, so the code says auth and a NotFound on any other call is
// left alone.
func TestAccountNotFoundIsAuth(t *testing.T) {
	t.Parallel()

	e := (&Client{}).classify("account", status.Error(codes.NotFound, "gone"))
	accountNotFoundIsAuth(e)
	require.Equal(t, CodeAuth, e.Code)
	require.False(t, e.Temporary, "an unregistered identity stays unregistered")
	require.Equal(t, codes.NotFound.String(), e.GRPC,
		"the status it came from is still in the log line")

	other := (&Client{}).classify(
		"account",
		status.Error(codes.PermissionDenied, "nope"),
	)
	accountNotFoundIsAuth(other)
	require.Equal(t, CodePermission, other.Code)
}

// doMapped's remap runs before the retry decision, so a corrected permanent
// failure costs one round trip rather than the whole budget.
func TestDoMappedRemapsBeforeDecidingToRetry(t *testing.T) {
	t.Parallel()

	client := &Client{
		log: slog.New(slog.DiscardHandler),
		backoff: retry.New(retry.Config{
			Name: "depin-test", MaxAttempts: 4,
			InitialDelay: time.Millisecond, MaxDelay: time.Millisecond,
			Multiplier: 2,
		}),
	}

	attempts := 0
	err := client.doMapped(
		context.Background(),
		"account",
		accountNotFoundIsAuth,
		func() error {
			attempts++
			return status.Error(codes.NotFound, "no such client")
		},
	)

	var got *Error
	require.ErrorAs(t, err, &got)
	require.Equal(t, CodeAuth, got.Code)
	require.Equal(t, 1, attempts)
}
