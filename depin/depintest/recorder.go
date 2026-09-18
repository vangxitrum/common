// Package depintest is a recording [depin.Storage] for tests.
package depintest

import (
	"bytes"
	"context"
	"io"
	"sync"
	"time"

	"gitlab.internal/tuan.quang.tran/aioz-common/depin"
	"gitlab.internal/tuan.quang.tran/aioz-common/uuid"
)

// Upload is one recorded upload: the parameters it was given and the bytes it
// was handed.
type Upload struct {
	Params depin.UploadParams
	Body   []byte
	FileID uuid.UUID
}

// Recorder records every call it is asked to make, and stores nothing.
//
// It is safe for concurrent use, which a service that uploads from goroutines
// needs.
type Recorder struct {
	// Fail, when set, is asked about every call by operation name ("upload",
	// "download", "delete file", ...). A non-nil error is returned from the
	// call and nothing is recorded. A test uses it to make the coordinator go
	// down, or one file refuse to delete.
	Fail func(op string) error

	// Files, when set, is what Download serves for a file id. A id that is not
	// in the map downloads as not found.
	Files map[uuid.UUID][]byte

	// Placements, when set, is what ListPlacements returns.
	Placements []depin.Placement

	mu        sync.Mutex
	uploads   []Upload
	deleted   []uuid.UUID
	tickets   []uuid.UUID
	contracts []string
}

var _ depin.Storage = (*Recorder)(nil)

func (r *Recorder) Upload(
	_ context.Context,
	body io.Reader,
	p depin.UploadParams,
) (depin.Receipt, error) {
	if err := r.fail("upload"); err != nil {
		return depin.Receipt{}, err
	}

	var buf bytes.Buffer
	if body != nil {
		if _, err := io.Copy(&buf, body); err != nil {
			return depin.Receipt{}, err
		}
	}

	id := uuid.Must()

	r.mu.Lock()
	defer r.mu.Unlock()
	r.uploads = append(r.uploads, Upload{
		Params: p, Body: buf.Bytes(), FileID: id,
	})
	return depin.Receipt{
		FileID: id, EncryptedSize: int64(buf.Len()),
	}, nil
}

func (r *Recorder) Download(
	_ context.Context,
	fileID uuid.UUID,
	w io.Writer,
) error {
	if err := r.fail("download"); err != nil {
		return err
	}

	r.mu.Lock()
	body, ok := r.Files[fileID]
	r.mu.Unlock()
	if !ok {
		return &depin.Error{
			Code: depin.CodeNotFound, Op: "download",
		}
	}
	_, err := w.Write(body)
	return err
}

func (r *Recorder) DownloadByTicket(
	_ context.Context,
	ticket string,
	_ io.Writer,
) error {
	if err := r.fail("download by ticket"); err != nil {
		return err
	}
	if ticket == "" {
		return &depin.Error{Code: depin.CodeInvalid, Op: "download by ticket"}
	}
	return &depin.Error{Code: depin.CodeNotFound, Op: "download by ticket"}
}

func (r *Recorder) CreateTicket(
	_ context.Context,
	fileID uuid.UUID,
	expiresAt time.Time,
) (depin.Ticket, error) {
	if err := r.fail("create ticket"); err != nil {
		return depin.Ticket{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.tickets = append(r.tickets, fileID)
	return depin.Ticket{
		Token: "recorded-" + fileID.String(), ExpiresAt: expiresAt,
	}, nil
}

func (r *Recorder) DeleteFile(_ context.Context, fileID uuid.UUID) error {
	if err := r.fail("delete file"); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted = append(r.deleted, fileID)
	return nil
}

func (r *Recorder) ListPlacements(
	_ context.Context,
) ([]depin.Placement, error) {
	if err := r.fail("list placements"); err != nil {
		return nil, err
	}
	return r.Placements, nil
}

func (r *Recorder) CreateContract(
	_ context.Context,
	region string,
	_ int32,
	_ map[string]string,
) (uuid.UUID, error) {
	if err := r.fail("create contract"); err != nil {
		return uuid.Nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.contracts = append(r.contracts, region)
	return uuid.Must(), nil
}

func (r *Recorder) Close() error { return nil }

// Uploads returns the uploads recorded so far, in order.
func (r *Recorder) Uploads() []Upload {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Upload(nil), r.uploads...)
}

// Deleted returns the file ids DeleteFile was called with, in order.
func (r *Recorder) Deleted() []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uuid.UUID(nil), r.deleted...)
}

// Ticketed returns the file ids CreateTicket was called with, in order.
func (r *Recorder) Ticketed() []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uuid.UUID(nil), r.tickets...)
}

// Contracts returns the regions CreateContract was called with, in order.
func (r *Recorder) Contracts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.contracts...)
}

func (r *Recorder) fail(op string) error {
	if r.Fail == nil {
		return nil
	}
	return r.Fail(op)
}
