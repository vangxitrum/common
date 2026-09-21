package depin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/spacemonkeygo/monkit/v3"
	uplinksdk "gitlab.internal/aioz-depin/go-sdk"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"gitlab.internal/tuan.quang.tran/aioz-common/retry"
	"gitlab.internal/tuan.quang.tran/aioz-common/uuid"
)

const (
	defaultMaxAttempts  = 4
	defaultRetryWaitMin = 200 * time.Millisecond
	defaultRetryWaitMax = 5 * time.Second
)

// Client stores and fetches files through the depin go-sdk.
//
// The SDK's identifiers are declared in its internal/vo package, which no
// other module may import. Their underlying type is the type literal []byte,
// and a defined type is assignable to and from its underlying type when one
// side is not itself defined, so every id crosses this boundary as a []byte
// and is converted to a [uuid.UUID] here. That is why nothing in this package
// names vo.
type Client struct {
	log *slog.Logger
	sdk *uplinksdk.Client

	// backoff covers the short coordinator RPCs only. The SDK retries
	// downloads itself, and an upload is too expensive to repeat blindly.
	backoff *retry.Backoff
}

// NewClient dials the coordinator and returns a Client.
//
// The context the SDK takes is used for that initial dial only; its background
// p2p observer deliberately runs on its own, so there is no context to keep
// here and none to accept. Close releases both.
func NewClient(log *slog.Logger, config ClientConfig) (*Client, error) {
	if log == nil {
		return nil, fmt.Errorf("depin: logger is nil")
	}
	if config.IdentityDir == "" {
		return nil, fmt.Errorf(
			"depin: the depin provider needs an identity directory",
		)
	}
	if config.CoordPeerURL == "" {
		return nil, fmt.Errorf(
			"depin: the depin provider needs a coordinator peer URL",
		)
	}

	opts := []uplinksdk.ClientOption{
		uplinksdk.WithIdentityDir(config.IdentityDir),
		uplinksdk.WithCoordPeerURL(config.CoordPeerURL),
		uplinksdk.WithLogger(log),
	}
	// Every tuning option is appended only when set, so the SDK's own default
	// stays the default and this package never has to restate it.
	if config.PieceKeyPath != "" {
		opts = append(opts, uplinksdk.WithPieceKey(config.PieceKeyPath))
	}
	if config.DownloadBuffer != 0 {
		opts = append(
			opts,
			uplinksdk.WithDownloadBuffer(config.DownloadBuffer.Int()),
		)
	}
	if config.DownloadConcurrency != 0 {
		opts = append(
			opts,
			uplinksdk.WithDownloadConcurrency(config.DownloadConcurrency),
		)
	}
	if config.GlobalDownloadConcurrency != 0 {
		opts = append(
			opts,
			uplinksdk.WithGlobalDownloadConcurrency(
				config.GlobalDownloadConcurrency,
			),
		)
	}
	if config.DownloadTimeout != 0 {
		opts = append(
			opts,
			uplinksdk.WithDownloadTimeout(config.DownloadTimeout),
		)
	}
	if config.DetailedMetrics {
		opts = append(opts, uplinksdk.WithDetailedMetrics(true))
	}
	if config.P2PListenAddr != "" {
		opts = append(opts, uplinksdk.WithP2PListenAddr(config.P2PListenAddr))
	}
	if config.P2PAnnounceAddr != "" {
		opts = append(
			opts,
			uplinksdk.WithP2PAnnounceAddr(config.P2PAnnounceAddr),
		)
	}
	if config.DisableHolePunching {
		opts = append(opts, uplinksdk.WithoutHolePunching())
	}

	sdk, err := uplinksdk.New(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("depin: %w", err)
	}

	return &Client{
		log: log,
		sdk: sdk,
		backoff: retry.New(retry.Config{
			Name:         "depin",
			MaxAttempts:  orDefault(config.MaxAttempts, defaultMaxAttempts),
			InitialDelay: orDefault(config.RetryWaitMin, defaultRetryWaitMin),
			MaxDelay:     orDefault(config.RetryWaitMax, defaultRetryWaitMax),
			Multiplier:   2,
			Jitter:       true,
		}),
	}, nil
}

func orDefault[T int | time.Duration](v, fallback T) T {
	if v <= 0 {
		return fallback
	}
	return v
}

// Upload streams r into storage. It is not retried: the bytes are consumed as
// they are sent, so a second attempt would need the reader rewound, which this
// package cannot do on an arbitrary io.Reader.
func (c *Client) Upload(
	ctx context.Context,
	r io.Reader,
	p UploadParams,
) (_ Receipt, err error) {
	defer mon.Task()(&ctx)(&err)

	if err := p.validate(); err != nil {
		return Receipt{}, err
	}
	if r == nil {
		return Receipt{}, invalid("upload", "reader is nil")
	}

	params := uplinksdk.UploadParams{
		Filename:      p.Filename,
		Size:          p.Size,
		Placement:     p.Placement,
		SystemHeaders: p.SystemHeaders,
		UserMetadata:  p.UserMetadata,
	}
	// A nil ContractID is how the SDK is told to generate one; a zero UUID
	// would be a real id made of zero bytes.
	if !p.ContractID.IsNil() {
		params.ContractID = p.ContractID.Bytes()
	}

	fileID, encryptedSize, err := c.sdk.UploadFile(ctx, r, params)
	if err != nil {
		return Receipt{}, c.classify("upload", err)
	}

	id, err := uuid.FromBytes(fileID)
	if err != nil {
		return Receipt{}, &Error{
			Code: CodeUnavailable, Op: "upload",
			detail: "the coordinator returned an unreadable file id",
		}
	}
	return Receipt{FileID: id, EncryptedSize: encryptedSize}, nil
}

// Download writes the file to w as it arrives. The SDK retries its own pieces,
// so a failure here means it already gave up.
func (c *Client) Download(
	ctx context.Context,
	fileID uuid.UUID,
	w io.Writer,
) (err error) {
	defer mon.Task()(&ctx)(&err)

	if fileID.IsNil() {
		return invalid("download", "no file id")
	}
	if w == nil {
		return invalid("download", "writer is nil")
	}
	if err := c.sdk.DownloadFile(ctx, fileID.Bytes(), w); err != nil {
		return c.classify("download", err)
	}
	return nil
}

// DownloadByTicket writes the ticket's file to w, without this client's
// identity being the one that authorized it.
func (c *Client) DownloadByTicket(
	ctx context.Context,
	ticket string,
	w io.Writer,
) (err error) {
	defer mon.Task()(&ctx)(&err)

	if ticket == "" {
		return invalid("download by ticket", "no ticket")
	}
	if w == nil {
		return invalid("download by ticket", "writer is nil")
	}
	if err := c.sdk.DownloadByTicket(ctx, ticket, w); err != nil {
		return c.classify("download by ticket", err)
	}
	return nil
}

// CreateTicket returns a capability to download fileID until expiresAt.
func (c *Client) CreateTicket(
	ctx context.Context,
	fileID uuid.UUID,
	expiresAt time.Time,
) (_ Ticket, err error) {
	defer mon.Task()(&ctx)(&err)

	if fileID.IsNil() {
		return Ticket{}, invalid("create ticket", "no file id")
	}

	var ticket Ticket
	err = c.do(ctx, "create ticket", func() error {
		token, expiry, err := c.sdk.CreateDownloadTicket(
			ctx,
			fileID.Bytes(),
			expiresAt,
		)
		if err != nil {
			return err
		}
		ticket = Ticket{Token: token, ExpiresAt: expiry}
		return nil
	})
	if err != nil {
		return Ticket{}, err
	}
	return ticket, nil
}

// DeleteFile removes one file.
func (c *Client) DeleteFile(ctx context.Context, fileID uuid.UUID) (err error) {
	defer mon.Task()(&ctx)(&err)

	if fileID.IsNil() {
		return invalid("delete file", "no file id")
	}
	return c.do(ctx, "delete file", func() error {
		return c.sdk.DeleteFile(ctx, fileID.Bytes())
	})
}

// ListPlacements returns the placements the coordinator offers.
func (c *Client) ListPlacements(
	ctx context.Context,
) (_ []Placement, err error) {
	defer mon.Task()(&ctx)(&err)

	var out []Placement
	err = c.do(ctx, "list placements", func() error {
		found, err := c.sdk.ListPlacements(ctx)
		if err != nil {
			return err
		}
		out = make([]Placement, 0, len(found))
		for _, p := range found {
			if p == nil {
				continue
			}
			out = append(out, Placement{
				ID:          p.PlacementId,
				Name:        p.Name,
				Description: p.Description,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CreateContract creates a storage contract pinned to a placement. tags are
// set once, at creation, and are how usage is attributed later.
func (c *Client) CreateContract(
	ctx context.Context,
	region string,
	placementID int32,
	tags map[string]string,
) (_ uuid.UUID, err error) {
	defer mon.Task()(&ctx)(&err)

	var out uuid.UUID
	err = c.do(ctx, "create contract", func() error {
		contractID, err := c.sdk.CreateContractWithPlacement(
			ctx,
			region,
			placementID,
			tags,
		)
		if err != nil {
			return err
		}
		id, err := uuid.FromBytes(contractID)
		if err != nil {
			return err
		}
		out = id
		return nil
	})
	if err != nil {
		return uuid.Nil, err
	}
	return out, nil
}

// Close releases the p2p host and the connection pool.
func (c *Client) Close() error {
	if err := c.sdk.Close(); err != nil {
		return fmt.Errorf("depin: %w", err)
	}
	return nil
}

// do runs one short coordinator call, retrying what can pass.
//
// Only the calls that are idempotent and cheap go through here. The retry
// stops at the first permanent failure, so a NotFound costs one round trip.
func (c *Client) do(ctx context.Context, op string, call func() error) error {
	loop := c.backoff.Loop()
	var last *Error

	for loop.Wait(ctx) {
		err := call()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("depin: %s: %w", op, ctx.Err())
		}

		last = c.classify(op, err)
		if !last.Temporary {
			return failed(last)
		}

		c.log.WarnContext(ctx, "retrying a depin call",
			"op", op, "code", last.Code, "grpc", last.GRPC,
			"attempt", loop.Attempt()+2)
	}

	if ctx.Err() != nil {
		return fmt.Errorf("depin: %s: %w", op, ctx.Err())
	}
	if last == nil {
		return fmt.Errorf("depin: %s: %w", op, loop.Err())
	}
	return failed(last)
}

// failed counts a call that gave up.
func failed(e *Error) *Error {
	mon.Event("depin_call_failed", monkit.NewSeriesTag("code", e.Code))
	return e
}

// classify turns an SDK error into an [*Error].
//
// The SDK reports everything as fmt.Errorf("sdk: <op>: %w", err), so the
// gRPC status underneath is what carries the meaning; matching on the message
// text would break the first time a string changed.
func (c *Client) classify(op string, err error) *Error {
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) {
		return &Error{
			Code: CodeTransport, Op: op, Temporary: true,
			detail: err.Error(),
		}
	}

	st, ok := status.FromError(err)
	if !ok {
		// No status: it never reached the coordinator, or it failed before a
		// response. Worth another try.
		return &Error{
			Code: CodeTransport, Op: op, Temporary: true,
			detail: err.Error(),
		}
	}

	out := &Error{Op: op, GRPC: st.Code().String(), detail: st.Message()}
	switch st.Code() {
	case codes.OK:
		// An error carrying OK is the SDK failing on its own side, after a
		// call that the coordinator answered.
		out.Code, out.Temporary = CodeUnavailable, true
		out.detail = err.Error()
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		out.Code = CodeInvalid
	case codes.Unauthenticated:
		out.Code = CodeAuth
	case codes.PermissionDenied:
		out.Code = CodePermission
	case codes.NotFound:
		out.Code = CodeNotFound
	case codes.AlreadyExists, codes.Aborted:
		out.Code, out.Temporary = CodeConflict, st.Code() == codes.Aborted
	case codes.ResourceExhausted:
		out.Code, out.Temporary = CodeExhausted, true
	case codes.Unavailable, codes.Internal, codes.DataLoss, codes.Unknown:
		out.Code, out.Temporary = CodeUnavailable, true
	case codes.DeadlineExceeded, codes.Canceled:
		out.Code, out.Temporary = CodeTransport, true
	case codes.Unimplemented:
		out.Code = CodeInvalid
	default:
		out.Code, out.Temporary = CodeUnavailable, true
	}
	return out
}
