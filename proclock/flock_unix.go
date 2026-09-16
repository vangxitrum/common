//go:build unix

package proclock

import (
	"errors"
	"os"
	"syscall"
)

// errWouldBlock is the one outcome Acquire treats as "somebody else has it",
// as opposed to a fault.
var errWouldBlock = errors.New("proclock: lock would block")

// lockExclusive takes a non-blocking exclusive flock on file.
//
// The lock belongs to the open file description, so it is released when the
// descriptor is closed or the process dies, and a second open of the same file
// conflicts even within one process.
func lockExclusive(file *os.File) error {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errWouldBlock
	}
	return err
}
