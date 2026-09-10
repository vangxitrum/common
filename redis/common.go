package redis

import (
	"github.com/spacemonkeygo/monkit/v3"
	"github.com/zeebo/errs/v2"
)

// Error tags every failure this package produces.
var Error = errs.Tag("redis")

var mon = monkit.Package()
