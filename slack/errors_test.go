package slack_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/slack"
)

func TestTransportErrorUnwrapsToItsCause(t *testing.T) {
	_, err := newSlack(t, "http://127.0.0.1:1", nil).
		Post(context.Background(), message())
	require.Error(t, err)

	var slackErr *slack.Error
	require.ErrorAs(t, err, &slackErr)
	require.Equal(t, slack.CodeTransport, slackErr.Code)
	require.NotNil(t, errors.Unwrap(slackErr),
		"the transport error underneath must stay reachable")
}

func TestErrorWithNoCauseUnwrapsToNil(t *testing.T) {
	var err error = &slack.Error{}
	require.Nil(t, errors.Unwrap(err))
}
