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
	// "download", "download range", "create local ticket", "account",
	// "delete file", ...). A non-nil error is returned from the
	// call and nothing is recorded. A test uses it to make the coordinator go
	// down, or one file refuse to delete.
	Fail func(op string) error

	// Files, when set, is what Download serves for a file id. A id that is not
	// in the map downloads as not found.
	Files map[uuid.UUID][]byte

	// Placements, when set, is what ListPlacements returns.
	Placements []depin.Placement

	// AccountRecord is what Account returns. Its zero value is an account
	// with no deposit address, which is what a caller reading one has to
	// handle anyway.
	AccountRecord depin.Account

	mu           sync.Mutex
	uploads      []Upload
	deleted      []uuid.UUID
	tickets      []uuid.UUID
	localTickets []uuid.UUID
	ranges       []Range
	contracts    []string
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

// Range is one recorded ranged download.
type Range struct {
	FileID uuid.UUID
	Offset int64
	Length int64
}

func (r *Recorder) DownloadRange(
	_ context.Context,
	fileID uuid.UUID,
	w io.Writer,
	offset, length int64,
) error {
	if err := r.fail("download range"); err != nil {
		return err
	}
	if offset < 0 || length <= 0 {
		return &depin.Error{
			Code: depin.CodeInvalid, Op: "download range",
		}
	}

	r.mu.Lock()
	body, ok := r.Files[fileID]
	r.ranges = append(r.ranges, Range{
		FileID: fileID, Offset: offset, Length: length,
	})
	r.mu.Unlock()
	if !ok {
		return &depin.Error{
			Code: depin.CodeNotFound, Op: "download range",
		}
	}
	// A range that starts past the end serves nothing, and one that runs past
	// it is cut short, which is what the real provider does with the file size
	// the coordinator reports.
	if offset >= int64(len(body)) {
		return nil
	}
	end := min(offset+length, int64(len(body)))
	_, err := w.Write(body[offset:end])
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

func (r *Recorder) CreateLocalTicket(
	fileID uuid.UUID,
	expiresAt time.Time,
) (depin.Ticket, error) {
	if err := r.fail("create local ticket"); err != nil {
		return depin.Ticket{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.localTickets = append(r.localTickets, fileID)
	return depin.Ticket{
		Token:     "recorded-local-" + fileID.String(),
		ExpiresAt: expiresAt.Truncate(time.Second),
	}, nil
}

func (r *Recorder) Account(_ context.Context) (depin.Account, error) {
	if err := r.fail("account"); err != nil {
		return depin.Account{}, err
	}
	return r.AccountRecord, nil
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

// LocallyTicketed returns the file ids CreateLocalTicket was called with, in
// order.
func (r *Recorder) LocallyTicketed() []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uuid.UUID(nil), r.localTickets...)
}

// Ranges returns the ranges DownloadRange was asked for, in order, including
// the ones that found no file.
func (r *Recorder) Ranges() []Range {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Range(nil), r.ranges...)
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
