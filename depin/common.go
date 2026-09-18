package depin

import (
	"github.com/spacemonkeygo/monkit/v3"
)

// This package has no errs.Tag, unlike most of the repo. Every failure that
// reached the coordinator or a worker is an [*Error], whose Code says what a
// caller may do about it, and the failures that never got that far are
// fmt.Errorf("depin: ..."), which is where a construction error has always
// been reported. mail does the same, for the same reason.
var mon = monkit.Package()
