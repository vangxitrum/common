package depin_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/depin"
)

func TestErrorUnwrapsToItsCause(t *testing.T) {
	storage := depin.NewLog(slog.New(slog.DiscardHandler))
	sentinel := errors.New("the reader broke")

	_, err := storage.Upload(
		context.Background(),
		brokenReader{sentinel},
		depin.UploadParams{
			Size: 1, Filename: "probe.bin", ContractID: [16]byte{1},
		},
	)
	require.Error(t, err)

	require.ErrorIs(t, err, sentinel,
		"the cause must stay reachable, not be flattened into a string")

	var depinErr *depin.Error
	require.ErrorAs(t, err, &depinErr,
		"and the *Error must still be what a caller matches on")
	require.Equal(t, depin.CodeTransport, depinErr.Code)
}

func TestErrorWithNoCauseUnwrapsToNil(t *testing.T) {
	var err error = &depin.Error{}
	require.Nil(t, errors.Unwrap(err))
}

type brokenReader struct{ err error }

func (r brokenReader) Read([]byte) (int, error) { return 0, r.err }
