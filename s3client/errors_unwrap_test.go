package s3client_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/s3client"
)

func TestErrorUnwrapsToTheSDKError(t *testing.T) {
	t.Parallel()

	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request, _ recorded) {
		errorXML(w, http.StatusForbidden, "AccessDenied", "nope")
	})

	_, err := srv.client(t).Get(t.Context(), "secret")
	require.Error(t, err)

	var s3Err *s3client.Error
	require.ErrorAs(t, err, &s3Err)
	require.Equal(t, s3client.CodePermission, s3Err.Code)

	var apiErr smithy.APIError
	require.ErrorAs(t, errors.Unwrap(s3Err), &apiErr,
		"the SDK's typed error must stay reachable through Unwrap")
	require.Equal(t, "AccessDenied", apiErr.ErrorCode())
}

func TestErrorWithNoCauseUnwrapsToNil(t *testing.T) {
	t.Parallel()

	var err error = &s3client.Error{}
	require.Nil(t, errors.Unwrap(err))
}
