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

// A packed file is read back by byte range, so the range arguments are the
// part a caller gets wrong, and they are refused before a round trip.
func TestDownloadRangeRefusesImpossibleRanges(t *testing.T) {
	t.Parallel()

	storage, err := depin.New(
		quiet(),
		depin.Config{Provider: depin.ProviderLog},
	)
	require.NoError(t, err)

	for name, tc := range map[string]struct {
		offset, length int64
	}{
		"a negative offset": {offset: -1, length: 10},
		"a zero length":     {offset: 0, length: 0},
		"a negative length": {offset: 0, length: -10},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := storage.DownloadRange(
				context.Background(), uuid.Must(), io.Discard,
				tc.offset, tc.length,
			)

			var storageErr *depin.Error
			require.ErrorAs(t, err, &storageErr)
			require.Equal(t, depin.CodeInvalid, storageErr.Code)
		})
	}
}

// Like Download, a range from the log provider is not found rather than an
// empty success.
func TestLogProviderDownloadRangeIsNotFound(t *testing.T) {
	t.Parallel()

	storage, err := depin.New(
		quiet(),
		depin.Config{Provider: depin.ProviderLog},
	)
	require.NoError(t, err)

	err = storage.DownloadRange(
		context.Background(), uuid.Must(), io.Discard, 0, 16,
	)

	var storageErr *depin.Error
	require.ErrorAs(t, err, &storageErr)
	require.Equal(t, depin.CodeNotFound, storageErr.Code)
	require.False(t, storageErr.Temporary)
}

// The log provider answers a local ticket rather than reporting a missing CA
// key, because its caller's fallback is a coordinator it also does not have.
func TestLogProviderMintsALocalTicket(t *testing.T) {
	t.Parallel()

	storage, err := depin.New(
		quiet(),
		depin.Config{Provider: depin.ProviderLog},
	)
	require.NoError(t, err)

	expiry := time.Now().Add(time.Hour)
	ticket, err := storage.CreateLocalTicket(uuid.Must(), expiry)
	require.NoError(t, err)
	require.NotEmpty(t, ticket.Token)
	require.Equal(t, expiry.Truncate(time.Second), ticket.ExpiresAt)

	_, err = storage.CreateLocalTicket(uuid.Nil, expiry)
	var storageErr *depin.Error
	require.ErrorAs(t, err, &storageErr)
	require.Equal(t, depin.CodeInvalid, storageErr.Code)
}

// A service reads its deposit address at startup, so the log provider answers
// one rather than failing the boot.
func TestLogProviderAccountIsAStandIn(t *testing.T) {
	t.Parallel()

	storage, err := depin.New(
		quiet(),
		depin.Config{Provider: depin.ProviderLog},
	)
	require.NoError(t, err)

	account, err := storage.Account(context.Background())
	require.NoError(t, err)
	require.Equal(t, "0x0000000000000000000000000000000000000000",
		account.Address)
	require.Equal(t, "log", account.Status)
}

// The recorder serves the bytes a range asked for, so a test of a packed file
// asserts the offsets its production code computed.
func TestRecorderServesRanges(t *testing.T) {
	t.Parallel()

	fileID := uuid.Must()
	rec := &depintest.Recorder{
		Files: map[uuid.UUID][]byte{fileID: []byte("0123456789")},
	}
	ctx := context.Background()

	var buf bytes.Buffer
	require.NoError(t, rec.DownloadRange(ctx, fileID, &buf, 2, 3))
	require.Equal(t, "234", buf.String())

	// A range running past the end is cut short, as a real file size does.
	buf.Reset()
	require.NoError(t, rec.DownloadRange(ctx, fileID, &buf, 8, 100))
	require.Equal(t, "89", buf.String())

	// And one starting past the end serves nothing.
	buf.Reset()
	require.NoError(t, rec.DownloadRange(ctx, fileID, &buf, 50, 10))
	require.Empty(t, buf.String())

	err := rec.DownloadRange(ctx, uuid.Must(), &buf, 0, 10)
	var storageErr *depin.Error
	require.ErrorAs(t, err, &storageErr)
	require.Equal(t, depin.CodeNotFound, storageErr.Code)

	require.Equal(t, []depintest.Range{
		{FileID: fileID, Offset: 2, Length: 3},
		{FileID: fileID, Offset: 8, Length: 100},
		{FileID: fileID, Offset: 50, Length: 10},
	}, rec.Ranges()[:3],
		"the ranges a caller asked for are what a test asserts on")
}

// Local tickets are recorded apart from coordinator ones, because which path a
// caller took is the thing worth asserting.
func TestRecorderRecordsLocalTicketsAndAccount(t *testing.T) {
	t.Parallel()

	fileID := uuid.Must()
	rec := &depintest.Recorder{
		AccountRecord: depin.Account{Address: "0xabc", Status: "active"},
	}

	ticket, err := rec.CreateLocalTicket(fileID, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Contains(t, ticket.Token, fileID.String())

	require.Equal(t, []uuid.UUID{fileID}, rec.LocallyTicketed())
	require.Empty(t, rec.Ticketed(), "a local ticket is not a coordinator one")

	account, err := rec.Account(context.Background())
	require.NoError(t, err)
	require.Equal(t, "0xabc", account.Address)

	rec.Fail = func(op string) error {
		return &depin.Error{Code: depin.CodeAuth, Op: op}
	}
	_, err = rec.Account(context.Background())
	var storageErr *depin.Error
	require.ErrorAs(t, err, &storageErr)
	require.Equal(t, depin.CodeAuth, storageErr.Code)
}
