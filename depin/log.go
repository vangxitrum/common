package depin

import (
	"context"
	"io"
	"log/slog"
	"time"

	"gitlab.internal/tuan.quang.tran/aioz-common/random"
	"gitlab.internal/tuan.quang.tran/aioz-common/uuid"
)

// NewLog returns the provider for development and CI: it validates a call,
// stores nothing, and logs that a file was not stored.
//
// It exists so a developer can run a service without an identity directory and
// a reachable coordinator. The log lines carry sizes and ids, never filenames
// or user metadata, for the reason given in the package doc.
func NewLog(log *slog.Logger) Storage {
	return &logStorage{log: log}
}

type logStorage struct {
	log *slog.Logger
}

func (s *logStorage) Upload(
	ctx context.Context,
	r io.Reader,
	p UploadParams,
) (Receipt, error) {
	if err := p.validate(); err != nil {
		return Receipt{}, err
	}

	// Drained rather than ignored, so a caller that closes or reuses the
	// reader afterwards behaves as it would against the real provider.
	written, err := io.Copy(io.Discard, r)
	if err != nil {
		return Receipt{}, &Error{
			Code: CodeTransport, Op: "upload",
			detail: err.Error(), cause: err,
		}
	}

	id := uuid.Must()
	s.log.InfoContext(ctx, "file not stored (log provider)",
		"file_id", id, "declared_size", p.Size, "read_size", written)

	return Receipt{FileID: id, EncryptedSize: written}, nil
}

func (s *logStorage) Download(
	ctx context.Context,
	fileID uuid.UUID,
	_ io.Writer,
) error {
	// Nothing was ever stored, so there is nothing to write. Reporting this as
	// not found rather than as an empty success keeps a caller's error path
	// exercised in development.
	s.log.InfoContext(ctx, "file not fetched (log provider)", "file_id", fileID)
	return &Error{
		Code: CodeNotFound, Op: "download",
		detail: "the log provider stores nothing",
	}
}

func (s *logStorage) DownloadByTicket(
	ctx context.Context,
	_ string,
	_ io.Writer,
) error {
	s.log.InfoContext(ctx, "file not fetched by ticket (log provider)")
	return &Error{
		Code: CodeNotFound, Op: "download by ticket",
		detail: "the log provider stores nothing",
	}
}

func (s *logStorage) CreateTicket(
	ctx context.Context,
	fileID uuid.UUID,
	expiresAt time.Time,
) (Ticket, error) {
	s.log.InfoContext(
		ctx,
		"ticket not created (log provider)",
		"file_id",
		fileID,
	)
	return Ticket{
		Token:     "log-" + random.String(16, random.Base62),
		ExpiresAt: expiresAt,
	}, nil
}

func (s *logStorage) DeleteFile(ctx context.Context, fileID uuid.UUID) error {
	s.log.InfoContext(ctx, "file not deleted (log provider)", "file_id", fileID)
	return nil
}

func (s *logStorage) ListPlacements(ctx context.Context) ([]Placement, error) {
	s.log.InfoContext(ctx, "placements not listed (log provider)")
	return []Placement{{
		ID:          0,
		Name:        "log",
		Description: "the log provider's stand-in placement",
	}}, nil
}

func (s *logStorage) CreateContract(
	ctx context.Context,
	region string,
	placementID int32,
	_ map[string]string,
) (uuid.UUID, error) {
	id := uuid.Must()
	s.log.InfoContext(ctx, "contract not created (log provider)",
		"contract_id", id, "region", region, "placement_id", placementID)
	return id, nil
}

func (s *logStorage) Close() error { return nil }
