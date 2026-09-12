package mailtest_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/mail"
	"gitlab.internal/tuan.quang.tran/aioz-common/mail/mailtest"
)

var _ mail.Sender = (*mailtest.Recorder)(nil)

func TestRecorderRecordsWhatWasSent(t *testing.T) {
	t.Parallel()

	recorder := &mailtest.Recorder{}
	msg := mail.Message{
		To:      []string{"user@example.com"},
		Subject: "hello",
		Text:    "hi",
	}

	receipt, err := recorder.Send(t.Context(), msg)
	require.NoError(t, err)
	require.NotEmpty(t, receipt.ID)
	require.Equal(t, []mail.Message{msg}, recorder.Sent())
}

// A test drives a failure for one message and not another, the way a service
// sees a single bounced recipient.
func TestRecorderInjectsFailures(t *testing.T) {
	t.Parallel()

	refused := errors.New("refused")
	recorder := &mailtest.Recorder{
		Fail: func(msg mail.Message) error {
			if msg.To[0] == "bounce@example.com" {
				return refused
			}
			return nil
		},
	}

	_, err := recorder.Send(
		t.Context(),
		mail.Message{To: []string{"bounce@example.com"}},
	)
	require.ErrorIs(t, err, refused)

	_, err = recorder.Send(
		t.Context(),
		mail.Message{To: []string{"ok@example.com"}},
	)
	require.NoError(t, err)

	sent := recorder.Sent()
	require.Len(t, sent, 1, "a failed send is not recorded as sent")
	require.Equal(t, "ok@example.com", sent[0].To[0])
}
