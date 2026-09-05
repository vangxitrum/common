package apierr_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/errs/v2"

	"gitlab.internal/tuan.quang.tran/aioz-template/internal/utils/apierr"
)

func TestConstructorsCarryStatusAndReason(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		err    error
		status int
	}{
		{apierr.BadRequest("name-empty", "no name"), http.StatusBadRequest},
		{apierr.Unauthorized("no-token", "no token"), http.StatusUnauthorized},
		{apierr.Forbidden("not-owner", "not yours"), http.StatusForbidden},
		{apierr.NotFound("file-missing", "no file"), http.StatusNotFound},
		{apierr.Conflict("order-already-exists", "taken"), http.StatusConflict},
		{apierr.TooManyRequests("rate-limited", "slow down"), http.StatusTooManyRequests},
		{apierr.Unavailable("db-down", "database is down"), http.StatusServiceUnavailable},
		{apierr.Internal("write-failed", errors.New("disk full")), http.StatusInternalServerError},
	} {
		require.Equal(t, tc.status, apierr.Status(tc.err))
		require.NotEmpty(t, apierr.Reason(tc.err))
	}
}

// TestForbiddenIsNot500 pins the specific mistake depin's error mapping made:
// with no Forbidden case, an authorization failure answered as Internal, so a
// permission problem looked like an outage.
func TestForbiddenIsNot500(t *testing.T) {
	t.Parallel()

	err := apierr.Forbidden("not-owner", "you do not own this")
	require.Equal(t, http.StatusForbidden, apierr.Status(err))
	require.NotEqual(t, http.StatusInternalServerError, apierr.Status(err))
}

// TestUnclassifiedIsAServerFault is the safe default: an error nobody
// classified is a surprise, and a surprise is the server's fault.
func TestUnclassifiedIsAServerFault(t *testing.T) {
	t.Parallel()

	require.Equal(t, http.StatusInternalServerError, apierr.Status(errors.New("boom")))
	require.Empty(t, apierr.Reason(errors.New("boom")))

	// A nil error has no status to report either; callers check err != nil
	// first, so this only has to not panic.
	require.Equal(t, http.StatusInternalServerError, apierr.Status(nil))
}

func TestWrappingSurvives(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("underlying")
	wrapped := apierr.Wrap("write-failed", http.StatusInternalServerError, sentinel)

	require.ErrorIs(t, wrapped, sentinel, "errors.Is must see through an apierr")
	require.Equal(t, "underlying", wrapped.Error())

	// And through another layer of wrapping, which is how it arrives at the
	// transport in a real call stack.
	outer := fmt.Errorf("handling request: %w", wrapped)
	require.ErrorIs(t, outer, sentinel)
	require.Equal(t, http.StatusInternalServerError, apierr.Status(outer))
	require.Equal(t, "write-failed", apierr.Reason(outer))
}

func TestWrapfPrefixesTheCause(t *testing.T) {
	t.Parallel()

	wrapped := apierr.Wrapf("save-failed", http.StatusConflict,
		errors.New("duplicate key"), "saving %s", "order-7")

	require.Equal(t, "saving order-7: duplicate key", wrapped.Error())
	require.Equal(t, http.StatusConflict, apierr.Status(wrapped))
}

// TestWrapPassesNilThrough keeps `return apierr.Wrap(...)` safe on a path
// where err may be nil.
func TestWrapNilIsNil(t *testing.T) {
	t.Parallel()

	require.NoError(t, apierr.Wrap("x", http.StatusBadRequest, nil))
	require.NoError(t, apierr.Wrapf("x", http.StatusBadRequest, nil, "msg"))
}

func TestIs(t *testing.T) {
	t.Parallel()

	require.True(t, apierr.Is(apierr.NotFound("gone", "gone"), http.StatusNotFound))
	require.False(t, apierr.Is(apierr.NotFound("gone", "gone"), http.StatusBadRequest))
}

// TestValidReason is the rule the codebase-wide policy test enforces.
func TestValidReason(t *testing.T) {
	t.Parallel()

	t.Run("a reason naming the subject is fine", func(t *testing.T) {
		for _, reason := range []string{
			"contract-not-found",
			"file-not-found",
			"name-too-long",
			"billing-disabled",
			"segment-modified",
			"quota",
		} {
			require.NoError(t, apierr.ValidReason(reason), reason)
		}
	})

	// This is depin's failure mode. Every missing record answered "not-found",
	// so a client could not tell a missing contract from a missing file.
	t.Run("a reason restating the status is rejected", func(t *testing.T) {
		for _, reason := range []string{
			"not-found", "bad-request", "forbidden", "internal",
			"unauthorized", "conflict", "unavailable", "error", "failed",
		} {
			err := apierr.ValidReason(reason)
			require.Error(t, err, reason)
			require.Contains(t, err.Error(), "name the subject")
		}
	})

	t.Run("a malformed slug is rejected", func(t *testing.T) {
		for _, reason := range []string{
			"", "NotFound", "not_found", "not found", "-leading", "trailing-",
			"double--dash", "9lives",
		} {
			require.Error(t, apierr.ValidReason(reason), "%q should be rejected", reason)
		}
	})
}

// TestFieldsNameTheSubject covers the axis depin's model lacks entirely: with
// only a package tag and a status tag, nothing carries which record failed.
func TestFieldsNameTheSubject(t *testing.T) {
	t.Parallel()

	err := apierr.NotFound("contract-not-found", "no such contract").
		With("resource", "contract", "id", "c-42")

	require.Equal(t, []any{"resource", "contract", "id", "c-42"}, apierr.Fields(err))
	require.Equal(t, "contract-not-found", apierr.Reason(err))

	// Fields survive further wrapping, which is how they reach the transport.
	wrapped := fmt.Errorf("creating file: %w", err)
	require.Equal(t, []any{"resource", "contract", "id", "c-42"}, apierr.Fields(wrapped))

	require.Nil(t, apierr.Fields(errors.New("plain")))
}

// TestWrapWithNilStaysNil guards the typed-nil trap: a nil *Error returned
// into an error variable is not nil, because the interface holds a type.
func TestWrapWithNilStaysNil(t *testing.T) {
	t.Parallel()

	err := apierr.WrapWith("contract-load-failed", http.StatusInternalServerError, nil, "id", "c-1")
	require.NoError(t, err)
	require.Nil(t, err)

	require.NoError(t, apierr.Internal("contract-load-failed", nil))
}

// TestTraceAndTagsSurviveWrapping is the regression test for the two silent
// no-ops this tooling started life as.
//
// errs.Tags type-asserts on the error it is handed and gives up immediately if
// it is not an *errorT, and a stack only prints under the "+" flag. So an errs
// error wrapped in anything - an apierr, a plain fmt.Errorf - reports no tags
// and no stack unless the chain is walked. Nothing about that is visible at
// the call site; it just quietly logs nothing useful.
func TestTraceAndTagsSurviveWrapping(t *testing.T) {
	t.Parallel()

	repo := errs.Tag("repo")
	cause := repo.Wrap(errors.New("connection refused"))

	// Bare, the library works.
	require.Contains(t, apierr.Tags(cause), "repo")
	require.Contains(t, apierr.Trace(cause), "connection refused")

	// Wrapped in an apierr - the normal case - it still has to.
	wrapped := apierr.Internal("contract-load-failed", cause)
	require.Contains(t, apierr.Tags(wrapped), "repo")
	require.Contains(t, apierr.Trace(wrapped), "apierr_test")

	// And through a further fmt.Errorf, which is how it arrives in practice.
	outer := fmt.Errorf("creating file: %w", wrapped)
	require.Contains(t, apierr.Tags(outer), "repo")
	require.Contains(t, apierr.Trace(outer), "apierr_test")

	// An error with no errs anywhere in the chain has neither, and must not
	// panic or invent one.
	require.Empty(t, apierr.Tags(errors.New("plain")))
	require.Empty(t, apierr.Trace(errors.New("plain")))
	require.Empty(t, apierr.Trace(nil))
}

// TestFormatReachesTheStack covers the %+v path for anyone who prints an
// apierr directly.
func TestFormatReachesTheStack(t *testing.T) {
	t.Parallel()

	cause := errs.Tag("repo").Wrap(errors.New("connection refused"))
	err := apierr.Internal("contract-load-failed", cause)

	plain := fmt.Sprintf("%v", err)
	require.Contains(t, plain, "connection refused")
	require.NotContains(t, plain, "apierr_test", "%v must not print a stack")

	verbose := fmt.Sprintf("%+v", err)
	require.Contains(t, verbose, "contract-load-failed")
	require.Contains(t, verbose, "apierr_test", "%+v must reach the stack")
}
