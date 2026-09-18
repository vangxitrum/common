package s3client

import (
	"context"
	"errors"
	"net/http"

	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/spacemonkeygo/monkit/v3"
)

// classify turns whatever the SDK returned into an [*Error].
//
// It reads the service's own error code first and falls back to the HTTP
// status, because our service answers some failures with a status and a body
// the SDK cannot parse into a typed error.
func classify(op string, err error) *Error {
	if err == nil {
		return nil
	}

	var already *Error
	if errors.As(err, &already) {
		return already
	}

	out := &Error{Op: op, detail: err.Error()}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		out.AWS = apiErr.ErrorCode()
	}

	var respErr *smithyhttp.ResponseError
	if errors.As(err, &respErr) && respErr.Response != nil {
		out.Status = respErr.Response.StatusCode
	}

	switch {
	case errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		out.Code = CodeTransport
		out.Temporary = false
		return out
	}

	switch out.AWS {
	case "NoSuchKey", "NoSuchBucket", "NotFound", "NoSuchUpload":
		out.Code = CodeNotFound
	case "PreconditionFailed":
		out.Code = CodePrecondition
	case "AccessDenied", "AllAccessDisabled":
		out.Code = CodePermission
	case "InvalidAccessKeyId", "SignatureDoesNotMatch",
		"ExpiredToken", "InvalidToken", "TokenRefreshRequired":
		out.Code = CodeAuth
	case "SlowDown", "RequestLimitExceeded", "TooManyRequests":
		out.Code = CodeRateLimited
		out.Temporary = true
	case "EntityTooLarge", "EntityTooSmall", "MaxMessageLengthExceeded":
		out.Code = CodeTooLarge
	case "InternalError", "ServiceUnavailable":
		out.Code = CodeUnavailable
		out.Temporary = true
	case "InvalidArgument", "InvalidRequest", "MalformedXML",
		"InvalidBucketName", "KeyTooLongError", "InvalidPart",
		"InvalidPartOrder", "BadDigest", "InvalidDigest":
		out.Code = CodeInvalid
	}
	if out.Code != "" {
		return out
	}

	// No usable service code. Our service returns a bare 404 with an empty
	// body for a missing object on some paths, which is exactly the case a
	// caller must not read as a failure.
	switch out.Status {
	case http.StatusNotFound:
		out.Code = CodeNotFound
	case http.StatusPreconditionFailed:
		out.Code = CodePrecondition
	case http.StatusUnauthorized:
		out.Code = CodeAuth
	case http.StatusForbidden:
		out.Code = CodePermission
	case http.StatusRequestEntityTooLarge:
		out.Code = CodeTooLarge
	case http.StatusTooManyRequests:
		out.Code = CodeRateLimited
		out.Temporary = true
	case http.StatusBadRequest, http.StatusMethodNotAllowed,
		http.StatusNotImplemented:
		out.Code = CodeInvalid
	case 0:
		// The request never produced a response: a dial failure, a reset, a
		// body that stopped arriving.
		out.Code = CodeTransport
		out.Temporary = true
	default:
		if out.Status >= http.StatusInternalServerError {
			out.Code = CodeUnavailable
			out.Temporary = true
		} else {
			out.Code = CodeInvalid
		}
	}
	return out
}

// fail records a failure and returns it, so every give-up is one series tagged
// with a code from the closed set.
func fail(op string, err error) error {
	if err == nil {
		return nil
	}
	e := classify(op, err)
	mon.Event("s3client_failed", monkit.NewSeriesTag("code", e.Code))
	return e
}
