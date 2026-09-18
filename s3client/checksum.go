package s3client

import (
	"context"
	"crypto/md5" // #nosec G501 -- S3 defines Content-MD5 as a wire checksum.
	"encoding/base64"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// flexibleChecksumMiddlewareID is the SDK's own checksum middleware, declared
// in service/internal/checksum, which this replaces for DeleteObjects.
const flexibleChecksumMiddlewareID = "AWSChecksum:ComputeInputPayloadChecksum"

// deleteObjectsOperationID is the operation whose stack gets the replacement.
// The SDK names each operation's stack after the operation
// (middleware.NewStack(opID, ...) in the generated client), so one registration
// can apply to exactly one operation.
const deleteObjectsOperationID = "DeleteObjects"

// withDeleteObjectsContentMD5 gives DeleteObjects the Content-MD5 header the
// S3 protocol defines for it, in place of the flexible CRC32 the SDK now sends
// by default and our service rejects.
//
// It is registered on the client rather than passed to each call, so a
// DeleteObjects made through the embedded *s3.Client is covered too, and it
// checks the stack's operation so every other operation keeps the SDK's own
// checksum behavior.
func withDeleteObjectsContentMD5(options *s3.Options) {
	options.APIOptions = append(
		options.APIOptions,
		func(stack *middleware.Stack) error {
			if stack.ID() != deleteObjectsOperationID {
				return nil
			}
			// Swap, not Add: the replacement inherits the original's position
			// in the Finalize step, which is before SigV4 signs. Added instead,
			// it would run after signing, Content-MD5 would be missing from
			// SignedHeaders, and the service would reject the signature.
			_, err := stack.Finalize.Swap(
				flexibleChecksumMiddlewareID,
				&deleteObjectsContentMD5{},
			)
			if err != nil {
				return fmt.Errorf(
					"s3client: install DeleteObjects Content-MD5 middleware: %w",
					err,
				)
			}
			return nil
		},
	)
}

type deleteObjectsContentMD5 struct{}

func (*deleteObjectsContentMD5) ID() string {
	return "AiozDeleteObjectsContentMD5"
}

func (*deleteObjectsContentMD5) HandleFinalize(
	ctx context.Context,
	input middleware.FinalizeInput,
	next middleware.FinalizeHandler,
) (
	middleware.FinalizeOutput,
	middleware.Metadata,
	error,
) {
	request, ok := input.Request.(*smithyhttp.Request)
	if !ok {
		return middleware.FinalizeOutput{}, middleware.Metadata{}, fmt.Errorf(
			"s3client: compute DeleteObjects Content-MD5: unexpected request type %T",
			input.Request,
		)
	}
	if request.GetStream() == nil {
		return middleware.FinalizeOutput{}, middleware.Metadata{}, fmt.Errorf(
			"s3client: compute DeleteObjects Content-MD5: request has no body",
		)
	}
	// Rewound before, because something earlier in the stack may already have
	// read the body, and again after, because the signer and the transport
	// both have to see it from byte zero.
	if err := request.RewindStream(); err != nil {
		return middleware.FinalizeOutput{}, middleware.Metadata{}, fmt.Errorf(
			"s3client: rewind DeleteObjects body before checksum: %w", err,
		)
	}

	hash := md5.New() // #nosec G401 -- S3 requires MD5 as a transport checksum.
	if _, err := io.Copy(hash, request.GetStream()); err != nil {
		return middleware.FinalizeOutput{}, middleware.Metadata{}, fmt.Errorf(
			"s3client: read DeleteObjects body for checksum: %w", err,
		)
	}
	if err := request.RewindStream(); err != nil {
		return middleware.FinalizeOutput{}, middleware.Metadata{}, fmt.Errorf(
			"s3client: rewind DeleteObjects body after checksum: %w", err,
		)
	}

	for header := range request.Header {
		if strings.HasPrefix(strings.ToLower(header), "x-amz-checksum-") {
			request.Header.Del(header)
		}
	}
	request.Header.Del("X-Amz-Sdk-Checksum-Algorithm")
	request.Header.Set(
		"Content-MD5",
		base64.StdEncoding.EncodeToString(hash.Sum(nil)),
	)
	return next.HandleFinalize(ctx, input)
}
