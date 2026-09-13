package ratelimit

import (
	"github.com/spacemonkeygo/monkit/v3"
	"github.com/zeebo/errs/v2"
)

// Error tags every failure this package produces.
var Error = errs.Tag("ratelimit")

var mon = monkit.Package()
