package retry

import "errors"

// permanent marks an error that must not be retried however the classifier
// would have judged it.
type permanent struct{ err error }

func (p permanent) Error() string { return p.err.Error() }
func (p permanent) Unwrap() error { return p.err }

// Permanent marks err as not worth retrying, whatever the classifier says.
//
// A classifier judges a failure from the outside, by its type. This is the
// judgment from the inside: code that knows this particular failure is its own
// doing - a request it built wrong, a value it failed to parse - and that
// running it again will fail the same way. Retrying that is four times the log
// noise and none of the recovery.
//
// The mark survives wrapping, so a caller may return
// fmt.Errorf("building the request: %w", retry.Permanent(err)) and keep its
// context. Permanent(nil) is nil, so it composes with a bare error return.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanent{err: err}
}

// IsPermanent reports whether err was marked by Permanent anywhere in its
// chain.
func IsPermanent(err error) bool {
	var p permanent
	return errors.As(err, &p)
}
