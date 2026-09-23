// Package depin stores and fetches files through a provider-neutral [Storage].
//
// The depin go-sdk is the only implementation. It is wrapped rather than used
// directly for two reasons. Its identifiers are declared in
// go-sdk/internal/vo, which Go's internal rule puts out of reach of any other
// module, so every service that used it directly would end up passing raw
// []byte around. And it reports every failure as a formatted string, so a
// caller deciding whether to retry would be matching on error text.
//
// So [Storage] speaks in [uuid.UUID], and a failure arrives as an [*Error]
// whose Code and Temporary say what happened and whether trying again could
// help. The SDK logs through the caller's *slog.Logger.
//
// Nothing here logs file contents, filenames or user metadata. A filename is
// often the only description of a private file, and a log line is where it
// would leak.
package depin

import (
	"context"
	"fmt"
	"io"
	"time"

	"gitlab.internal/tuan.quang.tran/aioz-common/uuid"
)

// Placement is one storage placement the coordinator offers. A placement
// selects the redundancy algorithm and any geo or reputation rules applied to
// the files uploaded under a contract pinned to it.
type Placement struct {
	ID          int32
	Name        string
	Description string
}

// UploadParams describes one upload.
type UploadParams struct {
	// Filename is the name recorded with the file. It is metadata: nothing in
	// this package or the SDK treats it as a path.
	Filename string

	// Size is the plaintext length in bytes and is required. The SDK needs it
	// up front to plan segments, so an unknown-length stream has to be
	// measured or buffered by the caller first.
	Size int64

	// ContractID pins the file to a storage contract. The zero UUID lets the
	// coordinator generate one, which is rarely what a service wants: a
	// contract is how usage is attributed and how files are deleted in bulk.
	ContractID uuid.UUID

	// Placement chooses a placement for a generated contract. Nil takes the
	// coordinator's default.
	Placement []byte

	// SystemHeaders are headers the coordinator understands, such as
	// content-type. UserMetadata is arbitrary and returned as it was stored.
	SystemHeaders map[string]string
	UserMetadata  map[string]string
}

// Receipt is what an accepted upload returned.
type Receipt struct {
	// FileID is the durable identifier of the stored file.
	FileID uuid.UUID

	// EncryptedSize is the on-wire size written across every segment, which is
	// larger than UploadParams.Size by the redundancy and encryption overhead.
	// It is what storage is billed on, so it is worth recording.
	EncryptedSize int64
}

// Ticket is a capability to download one file without holding this client's
// identity. It is handed to something that should be able to fetch exactly
// that file until it expires, such as a browser or a CDN.
type Ticket struct {
	Token     string
	ExpiresAt time.Time
}

// Account is the coordinator's record of the identity this client holds.
//
// It is this package's own struct rather than the SDK's protobuf message,
// whose id and address fields are declared in go-sdk/internal/vo and so cannot
// be named outside that module.
type Account struct {
	// ID is the account's identifier at the coordinator.
	ID uuid.UUID

	// Address is the deposit address, an EVM-style hex string (0x...). It is
	// empty when the coordinator holds none.
	Address string

	Email     string
	Status    string
	CreatedAt time.Time
}

// Storage stores and fetches files.
//
// Download, DownloadRange and DownloadByTicket write to w as the data arrives
// rather than returning a reader, because that is the shape the SDK offers. A
// caller that needs a reader bridges it with io.Pipe.
//
// Close releases the p2p host and the connection pool. A Storage is meant to
// live as long as the process and be registered with the lifecycle package,
// not built per request.
type Storage interface {
	Upload(ctx context.Context, r io.Reader, p UploadParams) (Receipt, error)
	Download(ctx context.Context, fileID uuid.UUID, w io.Writer) error

	// DownloadRange writes the plaintext bytes [offset, offset+length) to w.
	// It exists because a caller that packs several small objects into one
	// uploaded file - thumbnail sizes, HLS segments, captions - reads them
	// back by byte range, and Download would fetch the whole pack each time.
	//
	// offset must not be negative and length must be greater than zero; a
	// whole file is Download's job, not a length that means "the rest".
	DownloadRange(
		ctx context.Context,
		fileID uuid.UUID,
		w io.Writer,
		offset, length int64,
	) error

	DownloadByTicket(ctx context.Context, ticket string, w io.Writer) error
	CreateTicket(
		ctx context.Context,
		fileID uuid.UUID,
		expiresAt time.Time,
	) (Ticket, error)

	// CreateLocalTicket signs a ticket with the identity's own CA key, with no
	// coordinator round trip, so a caller minting one link per playlist
	// segment is not making hundreds of RPCs. It takes no context for that
	// reason: nothing here blocks.
	//
	// It fails with [CodeAuth] when the identity directory holds no CA key
	// pair, which is the signal to fall back to [Storage.CreateTicket]. A
	// caller checks the code, never the message.
	CreateLocalTicket(fileID uuid.UUID, expiresAt time.Time) (Ticket, error)

	// Account returns the coordinator's record of this client's identity. An
	// identity the coordinator does not know is [CodeAuth].
	Account(ctx context.Context) (Account, error)

	DeleteFile(ctx context.Context, fileID uuid.UUID) error
	ListPlacements(ctx context.Context) ([]Placement, error)
	CreateContract(
		ctx context.Context,
		region string,
		placementID int32,
		tags map[string]string,
	) (uuid.UUID, error)
	Close() error
}

// The codes an [*Error] carries. The set is closed, so a metric can be tagged
// with one.
const (
	CodeInvalid     = "invalid"      // the request is wrong and will stay wrong
	CodeAuth        = "auth"         // the identity is missing, wrong or not registered
	CodePermission  = "permission"   // authenticated, but not allowed this file or contract
	CodeNotFound    = "not_found"    // no such file, contract or placement
	CodeConflict    = "conflict"     // the contract still holds files, or the id is taken
	CodeExhausted   = "exhausted"    // a quota or a resource limit is spent
	CodeRateLimited = "rate_limited" // too many requests; try later
	CodeUnavailable = "unavailable"  // the coordinator or a worker failed
	CodeTransport   = "transport"    // no answer: the connection failed, dropped or timed out
)

// Error is how a storage operation failed.
//
// It carries the operation and the coordinator's gRPC status, never the file's
// name or contents, so it is safe to log.
type Error struct {
	// Code is one of the Code constants above.
	Code string

	// Op is the operation that failed, such as "upload" or "list placements".
	Op string

	// GRPC is the gRPC status code's name, e.g. NotFound, or empty when the
	// failure never produced a status.
	GRPC string

	// Temporary reports whether the same call may succeed later. A temporary
	// error from a short coordinator call has already been retried up to its
	// budget.
	Temporary bool

	detail string

	// cause is the error underneath, when there was one: the SDK's error, or
	// whatever a reader or the network reported. Unwrap returns it, so
	// errors.Is and errors.As reach it.
	cause error
}

// Unwrap returns the error underneath, or nil when the failure originated
// here. It is what lets errors.Is find a context.Canceled or an io error
// through a depin failure.
func (e *Error) Unwrap() error { return e.cause }

func (e *Error) Error() string {
	msg := "depin: " + e.Op + ": " + e.Code
	if e.detail != "" {
		msg += ": " + e.detail
	}
	if e.GRPC != "" {
		msg += fmt.Sprintf(" (grpc %s)", e.GRPC)
	}
	return msg
}

// invalid is a failure caught here, before it cost a round trip.
func invalid(op, detail string) *Error {
	return &Error{Code: CodeInvalid, Op: op, detail: detail}
}

// validate refuses an upload that cannot succeed. The reasons name the field,
// never the filename in it.
func (p UploadParams) validate() error {
	if p.Size <= 0 {
		return invalid("upload", "size must be greater than zero")
	}
	if p.Filename == "" {
		return invalid("upload", "no filename")
	}
	return nil
}
