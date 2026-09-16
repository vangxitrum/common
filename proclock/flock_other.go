//go:build !unix

package proclock

import (
	"errors"
	"os"
	"runtime"
)

// errWouldBlock exists so the shared code compiles; nothing returns it here.
var errWouldBlock = errors.New("proclock: lock would block")

// lockExclusive refuses rather than pretending to lock. Windows can do this
// with LockFileEx, which would make golang.org/x/sys a direct dependency of
// every service for a platform none of them deploy on.
func lockExclusive(*os.File) error {
	return Error.Errorf("process locking is not supported on %s", runtime.GOOS)
}
