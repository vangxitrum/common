package errs2

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// errsPkg and errsType name zeebo/errs's error type, which is unexported.
const (
	errsPkg  = "github.com/zeebo/errs/v2"
	errsType = "errorT"
)

// Origin returns the innermost error in err's Unwrap chain that zeebo/errs
// produced, or nil when there is none.
//
// errs captures a stack on its first wrap and keeps it through later wraps of
// the same error, so the innermost one carries the stack of where the failure
// began; an outer one, wrapped again after a fmt.Errorf, starts higher up. It
// is found by type rather than with errs.Tags, which reports nothing for
// errs.Errorf and errs.Wrap: they capture a stack under an empty tag.
func Origin(err error) error {
	var origin error
	for ; err != nil; err = errors.Unwrap(err) {
		if isErrs(err) {
			origin = err
		}
	}
	return origin
}

// Frames returns the stack [Origin] carries, one "function:line" per frame:
// the function that wrapped the error first, then its callers. It returns nil
// when nothing in the chain carries a stack.
//
// errs keeps its program counters private and prints them only under %+v, as
// "\n\tfunction:line" after the message, so that output is parsed here once
// rather than by every caller that wants the frames.
func Frames(err error) []string {
	origin := Origin(err)
	if origin == nil {
		return nil
	}

	stack := strings.TrimPrefix(fmt.Sprintf("%+v", origin), origin.Error())
	// The stack starts with the separator, so the first element is empty.
	frames := strings.Split(stack, "\n\t")[1:]
	if len(frames) == 0 {
		return nil
	}
	return frames
}

func isErrs(err error) bool {
	t := reflect.TypeOf(err)
	if t.Kind() != reflect.Pointer {
		return false
	}
	t = t.Elem()
	return t.Name() == errsType && t.PkgPath() == errsPkg
}
