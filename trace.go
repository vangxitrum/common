package apierr

import (
	"errors"
	"fmt"

	"github.com/zeebo/errs/v2"
)

// Trace returns the origin stack of err, or "" when nothing in the chain
// carries one.
//
// zeebo/errs captures a stack on the first Tag.Wrap and keeps it through later
// ones, so this is where the failure started rather than where it was last
// touched. Getting at it needs care twice over: the stack only prints under
// the "+" flag, and errs.Tags/the formatter only see it when the errs error is
// the outermost one. Wrap it in an apierr - or in a plain fmt.Errorf - and a
// naive fmt.Sprintf("%+v", err) silently prints no stack at all. Hence the
// walk.
func Trace(err error) string {
	if inner := errsError(err); inner != nil {
		return fmt.Sprintf("%+v", inner)
	}
	return ""
}

// Tags returns the errs tags in the chain, outermost first: the packages the
// error passed through. Often enough to place a failure without reading the
// stack.
func Tags(err error) []string {
	inner := errsError(err)
	if inner == nil {
		return nil
	}

	tags := errs.Tags(inner)
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		out = append(out, string(tag))
	}
	return out
}

// errsError walks the Unwrap chain to the outermost error that zeebo/errs
// produced, which is the only one that can report tags or a stack.
func errsError(err error) error {
	for err != nil {
		if len(errs.Tags(err)) > 0 {
			return err
		}
		err = errors.Unwrap(err)
	}
	return nil
}

// Format lets a caller print an apierr with "%+v" and still reach the stack of
// the cause, rather than getting only the message.
func (e *Error) Format(f fmt.State, verb rune) {
	if verb != 'v' || !f.Flag('+') {
		_, _ = fmt.Fprint(f, e.Error())
		return
	}

	_, _ = fmt.Fprintf(f, "[%s] %s", e.reason, e.Error())
	if trace := Trace(e.err); trace != "" {
		_, _ = fmt.Fprint(f, trace)
	}
}
