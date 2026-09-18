// Package s3client talks to an S3-compatible object store, ours included.
//
// Our own service at s3-storage.attoaioz.cyou is behind real S3, and since
// aws-sdk-go-v2 v1.72 the SDK computes a CRC32 for every request by default:
// it sends x-amz-checksum-crc32 and x-amz-sdk-checksum-algorithm, and it
// switches a streaming body to aws-chunked with a trailing checksum. Our
// service rejects all of it. DeleteObjects needs the older Content-MD5 header
// instead, and that header has to exist before SigV4 signs, or it is not in
// SignedHeaders and the signature is refused.
//
// None of that is discoverable from a stack trace, and it was rediscovered
// three times: aioz-stream, aioz-map and crawler-service each grew their own
// client and only the newest one is right. This package is the one copy. The
// workarounds are selected by [Config.Compat], which defaults to our service.
//
// [Client] embeds the vendor's *s3.Client rather than hiding it behind an
// interface, so an operation this package does not wrap is still reachable and
// still carries the endpoint, the path style and the checksum settings. The
// wrapped operations exist where the raw call has a trap: a body that has to be
// closed, an absence that is not an error, a batch limit, a header the service
// needs.
//
// Nothing here logs an object key. Keys carry user ids and original filenames,
// and a log line is where that leaks.
package s3client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"strings"
	"time"
)

// MaxKeyLen is the longest key S3 accepts, in bytes.
const MaxKeyLen = 1024

// MaxDeleteBatch is the most keys one DeleteObjects request may carry.
const MaxDeleteBatch = 1000

// ObjectInfo is what is known about an object without its body.
type ObjectInfo struct {
	Key string

	// Size is the object's length in bytes, or -1 when the service reported
	// none. Our service omits Content-Length on some HEAD responses, and a
	// missing length is not a failure. Check for -1 before sizing a buffer.
	Size int64

	ETag         string
	LastModified time.Time
	ContentType  string
	CacheControl string
	Metadata     map[string]string
}

// Object is an open object.
//
// Body is the live response body: it must be closed, or the connection is held
// until the garbage collector takes it. Info is filled in before Body is read,
// so an ETag can be captured for a later conditional delete without draining
// the object first.
type Object struct {
	Body io.ReadCloser
	Info ObjectInfo
}

// Close closes the body. It is safe to call on a nil Object.
func (o *Object) Close() error {
	if o == nil || o.Body == nil {
		return nil
	}
	return o.Body.Close()
}

// PutParams describes one Put.
type PutParams struct {
	// ContentType is what the object is served as. Set it: a browser given
	// application/octet-stream renders nothing.
	ContentType  string
	CacheControl string

	// ContentLength is the body's length when it is known. Zero means unknown,
	// which is fine: the uploader buffers a part at a time either way.
	ContentLength int64

	// Metadata is stored as user metadata and returned as it was stored.
	Metadata map[string]string
}

// ListParams is one page request.
type ListParams struct {
	Prefix string

	// Delimiter rolls keys sharing a prefix up into [Page.Prefixes]. "/" lists
	// one directory level.
	Delimiter string

	// MaxKeys bounds one page. Zero takes the service's own default, which is
	// 1000 on S3 and on ours.
	MaxKeys int32

	// Cursor continues a listing. It is [Page.Cursor] from the page before.
	Cursor string
}

// Page is one page of a listing.
type Page struct {
	Objects []ObjectInfo

	// Prefixes holds the common prefixes, and is empty unless
	// [ListParams.Delimiter] was set.
	Prefixes []string

	// Cursor continues the listing, and is empty when it is complete.
	Cursor string
}

// DeleteParams describes one Delete.
type DeleteParams struct {
	// IfMatch deletes only while the object still has this ETag. A delete that
	// lost the race fails with [CodePrecondition], which [IsPrecondition]
	// reports, rather than removing whatever replaced it.
	IfMatch string
}

// CopyParams describes one Copy.
type CopyParams struct {
	// ReplaceMetadata swaps the destination's metadata for the fields below
	// instead of copying the source's. S3 has no way to change one header and
	// keep the rest: it is all or nothing.
	ReplaceMetadata bool

	ContentType  string
	CacheControl string
	Metadata     map[string]string
}

// The codes an [*Error] carries. The set is closed, so a metric can be tagged
// with one.
const (
	CodeInvalid      = "invalid"      // the request is wrong and will stay wrong
	CodeAuth         = "auth"         // the credentials are missing, wrong or expired
	CodePermission   = "permission"   // authenticated, but not allowed this bucket or key
	CodeNotFound     = "not_found"    // no such key or bucket
	CodePrecondition = "precondition" // an If-Match lost the race
	CodeTooLarge     = "too_large"    // over a service limit
	CodeRateLimited  = "rate_limited" // slow down; try later
	CodeUnavailable  = "unavailable"  // the service failed
	CodeTransport    = "transport"    // no answer: the connection failed, dropped or timed out
)

// Error is how an object operation failed.
//
// It carries the operation and the service's own code, never the key, so it is
// safe to log. A partial DeleteObjects failure reports how many keys failed,
// not which.
type Error struct {
	// Code is one of the Code constants above.
	Code string

	// Op is the operation that failed, such as "put" or "delete many".
	Op string

	// AWS is the service's error code, e.g. NoSuchKey, or empty when the
	// failure never produced one.
	AWS string

	// Status is the HTTP status, or 0 when there was no response.
	Status int

	// Temporary reports whether the same call may succeed later. The SDK's
	// retryer has already spent its budget on a temporary failure by the time
	// it reaches here.
	Temporary bool

	detail string
}

func (e *Error) Error() string {
	msg := "s3client: " + e.Op + ": " + e.Code
	if e.detail != "" {
		msg += ": " + e.detail
	}
	switch {
	case e.AWS != "" && e.Status != 0:
		msg += fmt.Sprintf(" (%s, http %d)", e.AWS, e.Status)
	case e.AWS != "":
		msg += fmt.Sprintf(" (%s)", e.AWS)
	case e.Status != 0:
		msg += fmt.Sprintf(" (http %d)", e.Status)
	}
	return msg
}

// invalid is a failure caught here, before it cost a round trip.
func invalid(op, detail string) *Error {
	return &Error{Code: CodeInvalid, Op: op, detail: detail}
}

// Code returns the Code of err's [*Error], or "" when err is not one.
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// IsNotFound reports whether err is a missing object or bucket.
//
// This is the replacement for an "exists" call returning a bool. A bool cannot
// tell a missing object from an expired credential, and a cleanup job that
// reads one as the other deletes nothing while reporting success.
func IsNotFound(err error) bool { return Code(err) == CodeNotFound }

// IsPrecondition reports whether a conditional delete lost the race, meaning
// the object changed after it was read.
func IsPrecondition(err error) bool { return Code(err) == CodePrecondition }

// IsTemporary reports whether trying again could help.
func IsTemporary(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Temporary
}

// validateKey refuses a key that cannot work. The reasons describe the key,
// never quote it.
func validateKey(op, key string) error {
	switch {
	case key == "":
		return invalid(op, "the key is empty")
	case len(key) > MaxKeyLen:
		return invalid(op, fmt.Sprintf(
			"the key is %d bytes, over the %d-byte limit", len(key), MaxKeyLen))
	case strings.HasPrefix(key, "/"):
		return invalid(op, "the key starts with a slash")
	}
	for segment := range strings.SplitSeq(key, "/") {
		if segment == ".." {
			return invalid(op, "the key contains a .. segment")
		}
	}
	return nil
}

// All walks every object matching p, fetching one page at a time.
//
// It stops at the first failure, yielding it with a zero [ObjectInfo]. Use
// [Client.List] directly when the cursor itself has to be stored, as a resumable
// job would.
func All(
	ctx context.Context,
	c *Client,
	p ListParams,
) iter.Seq2[ObjectInfo, error] {
	return func(yield func(ObjectInfo, error) bool) {
		for {
			page, err := c.List(ctx, p)
			if err != nil {
				yield(ObjectInfo{}, err)
				return
			}
			for _, obj := range page.Objects {
				if !yield(obj, nil) {
					return
				}
			}
			if page.Cursor == "" {
				return
			}
			p.Cursor = page.Cursor
		}
	}
}
