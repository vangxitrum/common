package apierr_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

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
		{apierr.Conflict("already-exists", "taken"), http.StatusConflict},
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
