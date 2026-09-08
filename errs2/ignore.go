// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package errs2

import (
	"context"
	"errors"
)

// IsCanceled returns true, when the error is a cancellation.
//
// [errors.Is] walks a wrap chain and a multi-error alike, and errs/v2's group
// type implements both Unwrap and Is, so a cancellation stays visible through
// an [errs.Combine]. The errs.IsFunc this used to call predates errors.Is and
// no longer exists in v2.
func IsCanceled(err error) bool {
	return errors.Is(err, context.Canceled)
}

// IgnoreCanceled returns nil, when the operation was about canceling.
func IgnoreCanceled(err error) error {
	if IsCanceled(err) {
		return nil
	}
	return err
}
