package mail_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/mail"
)

// The provider is chosen by name. A missing key with the real provider is a
// startup error, never a quiet switch to not sending.
func TestNewChoosesTheProvider(t *testing.T) {
	t.Parallel()

	logger := newLogger(&logBuffer{})

	sender, err := mail.New(logger, mail.Config{Provider: mail.ProviderLog})
	require.NoError(t, err)
	require.NotNil(t, sender)

	_, err = mail.New(
		logger,
		mail.Config{Provider: mail.ProviderResend, From: testFrom},
	)
	require.Error(t, err, "resend without an API key")

	_, err = mail.New(logger, mail.Config{
		Provider: mail.ProviderResend,
		Resend:   mail.ResendConfig{APIKey: testKey},
	})
	require.Error(t, err, "resend without a sender")

	sender, err = mail.New(logger, mail.Config{
		Provider: mail.ProviderResend,
		From:     testFrom,
		Resend:   resendConfig("http://127.0.0.1:1"),
	})
	require.NoError(t, err)
	require.IsType(t, &mail.Resend{}, sender)

	_, err = mail.New(logger, mail.Config{Provider: "smtp"})
	require.Error(t, err, "an unknown provider")

	_, err = mail.New(logger, mail.Config{})
	require.Error(t, err, "no provider at all")
}

// The log provider is for development: it records that a mail was not sent,
// and nothing that was in it. A one-time code in a dev log is still a code.
func TestTheLogProviderLogsNoContent(t *testing.T) {
	t.Parallel()

	logs := &logBuffer{}
	sender := mail.NewLog(newLogger(logs))

	receipt, err := sender.Send(t.Context(), message())
	require.NoError(t, err)
	require.NotEmpty(t, receipt.ID)

	require.Contains(t, logs.String(), "mail not sent")
	require.Contains(
		t,
		logs.String(),
		"example.com",
		"the recipient domain helps without naming anyone",
	)
	for _, leak := range []string{"123456", "user@example.com", "Your code"} {
		require.NotContains(t, logs.String(), leak)
	}
}

func TestTheLogProviderStillValidates(t *testing.T) {
	t.Parallel()

	msg := message()
	msg.To = nil
	_, err := mail.NewLog(newLogger(&logBuffer{})).Send(t.Context(), msg)
	require.Equal(t, mail.CodeInvalid, requireMailError(t, err).Code)
}
