package depin_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/depin"
	"gitlab.internal/tuan.quang.tran/aioz-common/depin/depintest"
	"gitlab.internal/tuan.quang.tran/aioz-common/uuid"
)

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

// A production deploy that forgot the identity directory must fail at startup,
// not quietly store nothing.
func TestConfigValidate(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		config  depin.Config
		wantErr string
	}{
		"log provider needs nothing": {
			config: depin.Config{Provider: depin.ProviderLog},
		},
		"depin provider is complete": {
			config: depin.Config{
				Provider: depin.ProviderDepin,
				Client: depin.ClientConfig{
					IdentityDir:  "/etc/aioz/identity",
					CoordPeerURL: "coord@10.0.0.1:7777",
				},
			},
		},
		"depin provider without an identity directory": {
			config: depin.Config{
				Provider: depin.ProviderDepin,
				Client:   depin.ClientConfig{CoordPeerURL: "coord@10.0.0.1:7777"},
			},
			wantErr: "identity directory",
		},
		"depin provider without a coordinator": {
			config: depin.Config{
				Provider: depin.ProviderDepin,
				Client:   depin.ClientConfig{IdentityDir: "/etc/aioz/identity"},
			},
			wantErr: "coordinator peer URL",
		},
		"an unknown provider": {
			config:  depin.Config{Provider: "s3"},
			wantErr: `provider "s3"`,
		},
		"an empty provider is not inferred": {
			config:  depin.Config{},
			wantErr: "provider",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := tc.config.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// New must not dial anything for the log provider, so a developer can run the
// service with no identity and no coordinator.
func TestNewLogProviderNeedsNoNetwork(t *testing.T) {
	t.Parallel()

	storage, err := depin.New(
		quiet(),
		depin.Config{Provider: depin.ProviderLog},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, storage.Close()) })

	receipt, err := storage.Upload(
		context.Background(),
		strings.NewReader("hello"),
		depin.UploadParams{Filename: "greeting.txt", Size: 5},
	)
	require.NoError(t, err)
	require.False(t, receipt.FileID.IsNil())
	require.EqualValues(t, 5, receipt.EncryptedSize)
}

// The log provider stores nothing, so a download has to fail rather than look
// like an empty file, which would hide a missing configuration until
// production.
func TestLogProviderDownloadIsNotFound(t *testing.T) {
	t.Parallel()

	storage, err := depin.New(
		quiet(),
		depin.Config{Provider: depin.ProviderLog},
	)
	require.NoError(t, err)

	err = storage.Download(context.Background(), uuid.Must(), io.Discard)

	var storageErr *depin.Error
	require.ErrorAs(t, err, &storageErr)
	require.Equal(t, depin.CodeNotFound, storageErr.Code)
	require.False(t, storageErr.Temporary)
}

// An upload the caller got wrong is refused here, before it costs anything.
func TestLogProviderValidatesUploads(t *testing.T) {
	t.Parallel()

	storage, err := depin.New(
		quiet(),
		depin.Config{Provider: depin.ProviderLog},
	)
	require.NoError(t, err)

	for name, params := range map[string]depin.UploadParams{
		"no size":     {Filename: "a.txt"},
		"no filename": {Size: 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := storage.Upload(
				context.Background(), strings.NewReader("x"), params,
			)

			var storageErr *depin.Error
			require.ErrorAs(t, err, &storageErr)
			require.Equal(t, depin.CodeInvalid, storageErr.Code)
		})
	}
}

// Nothing about a file's name or contents may reach a log line.
func TestLogProviderLogsNoFilename(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	storage, err := depin.New(
		slog.New(slog.NewJSONHandler(&buf, nil)),
		depin.Config{Provider: depin.ProviderLog},
	)
	require.NoError(t, err)

	_, err = storage.Upload(
		context.Background(),
		strings.NewReader("top secret"),
		depin.UploadParams{Filename: "salaries-2026.xlsx", Size: 10},
	)
	require.NoError(t, err)

	require.NotContains(t, buf.String(), "salaries-2026")
	require.NotContains(t, buf.String(), "top secret")
}

func TestRecorderRecords(t *testing.T) {
	t.Parallel()

	rec := &depintest.Recorder{}
	ctx := context.Background()

	receipt, err := rec.Upload(ctx, strings.NewReader("body"),
		depin.UploadParams{Filename: "a.txt", Size: 4})
	require.NoError(t, err)

	require.NoError(t, rec.DeleteFile(ctx, receipt.FileID))
	_, err = rec.CreateTicket(ctx, receipt.FileID, time.Now().Add(time.Hour))
	require.NoError(t, err)

	uploads := rec.Uploads()
	require.Len(t, uploads, 1)
	require.Equal(t, "a.txt", uploads[0].Params.Filename)
	require.Equal(t, []byte("body"), uploads[0].Body)

	require.Equal(t, []uuid.UUID{receipt.FileID}, rec.Deleted())
	require.Equal(t, []uuid.UUID{receipt.FileID}, rec.Ticketed())
}

// Fail is how a test makes the coordinator go down.
func TestRecorderFailHook(t *testing.T) {
	t.Parallel()

	rec := &depintest.Recorder{
		Fail: func(op string) error {
			if op == "upload" {
				return &depin.Error{Code: depin.CodeUnavailable, Op: op}
			}
			return nil
		},
	}

	_, err := rec.Upload(context.Background(), strings.NewReader("x"),
		depin.UploadParams{Filename: "a.txt", Size: 1})

	var storageErr *depin.Error
	require.ErrorAs(t, err, &storageErr)
	require.Equal(t, depin.CodeUnavailable, storageErr.Code)
	require.Empty(t, rec.Uploads())
}
