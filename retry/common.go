package retry

import (
	"github.com/spacemonkeygo/monkit/v3"
	"github.com/zeebo/errs/v2"
)

// Error tags every failure this package produces.
var Error = errs.Tag("retry")

var mon = monkit.Package()
