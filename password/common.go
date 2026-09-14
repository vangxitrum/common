package password

import (
	"errors"

	"github.com/spacemonkeygo/monkit/v3"
	"github.com/zeebo/errs/v2"
)

// Error tags every failure this package produces.
var Error = errs.Tag("password")

// Returned by Check, and by Hash and Verify for ErrTooLong. Test with
// errors.Is: the message carries the limit.
var (
	ErrTooShort = errors.New("password: too short")
	ErrTooLong  = errors.New("password: too long")
)

var mon = monkit.Package()
