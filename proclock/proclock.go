// Package proclock keeps a second copy of a process from starting on the same
// machine.
//
//	lock, err := proclock.Acquire(config.Lock, "api")
//	if errors.Is(err, proclock.ErrHeld) {
//	    return err // "api" is already running as pid 4123
//	}
//	defer lock.Close()
//
// The lock is an flock on a file held open for the life of the process, so the
// kernel releases it when the process dies, SIGKILL and a panic included.
// There is no heartbeat, no timeout and no stale-lock recovery, because a dead
// process holds nothing.
//
// # What this package is not for
//
// One replica out of several running a job. That is a lease in Redis or a
// Postgres advisory lock, which has to survive a machine going away, and needs
// renewal and fencing this deliberately does not have. An flock says only
// "not twice on this box".
package proclock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/zeebo/errs/v2"
)

// Error tags every error this package produces, apart from HeldError.
var Error = errs.Tag("proclock")

// ErrHeld reports that another process holds the lock. A HeldError matches it,
// so a caller that only wants to exit cleanly can test errors.Is rather than
// the type.
var ErrHeld = errors.New("lock is held")

// MaxNameLen is the longest accepted lock name.
const MaxNameLen = 64

const (
	dirPerm  = 0o755
	filePerm = 0o644
)

// Config is where lock files live, under the owning service's config prefix.
type Config struct {
	Dir string `help:"directory holding process lock files" default:"$CONFDIR/run" path:"true"`
}

// Lock is a held lock. Close releases it; the process exiting does too.
type Lock struct {
	file *os.File
	path string

	once sync.Once
	err  error
}

// HeldError is returned when another process holds the lock, and names it.
type HeldError struct {
	Name string
	Path string

	// PID is the holder, or 0 when the lock file could not be read.
	PID int
}

// Error implements error.
func (e *HeldError) Error() string {
	if e.PID > 0 {
		return fmt.Sprintf(
			"proclock: %q is already running as pid %d (%s)",
			e.Name,
			e.PID,
			e.Path,
		)
	}
	return fmt.Sprintf("proclock: %q is already running (%s)", e.Name, e.Path)
}

// Is reports ErrHeld, so errors.Is answers "was it already running?".
func (e *HeldError) Is(target error) bool { return target == ErrHeld }

// Acquire takes the lock named name in config.Dir, creating the directory if
// it is missing. It returns a *HeldError when another process holds it, and
// never waits.
func Acquire(config Config, name string) (*Lock, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	if config.Dir == "" {
		return nil, Error.Errorf("lock directory is empty")
	}
	if err := os.MkdirAll(config.Dir, dirPerm); err != nil {
		return nil, Error.Wrap(err)
	}

	path := filepath.Join(config.Dir, name+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, filePerm)
	if err != nil {
		return nil, Error.Wrap(err)
	}

	if err := lockExclusive(file); err != nil {
		// Read the holder's pid while the descriptor is still open, so the
		// error can name it.
		held := errors.Is(err, errWouldBlock)
		pid := 0
		if held {
			pid = readPID(file)
		}
		_ = file.Close()
		if held {
			return nil, &HeldError{Name: name, Path: path, PID: pid}
		}
		return nil, Error.Wrap(err)
	}

	if err := writePID(file); err != nil {
		_ = file.Close()
		return nil, Error.Wrap(err)
	}

	return &Lock{file: file, path: path}, nil
}

// Path is the lock file's path.
func (l *Lock) Path() string { return l.path }

// Close releases the lock. It is safe to call more than once.
//
// The file is left behind on purpose. Deleting it is how two processes end up
// holding one lock: another process can be waiting on the open file while this
// one unlinks it, and a third then creates and locks a fresh file at the same
// path. An empty file costs nothing.
func (l *Lock) Close() error {
	l.once.Do(func() {
		if err := l.file.Close(); err != nil {
			l.err = Error.Wrap(err)
		}
	})
	return l.err
}

// validateName keeps the name to one readable path element: it is joined onto
// Dir, so a separator or a parent reference would put the lock somewhere else
// entirely.
func validateName(name string) error {
	if name == "" {
		return Error.Errorf("lock name is empty")
	}
	if len(name) > MaxNameLen {
		return Error.Errorf(
			"lock name %q is longer than %d characters",
			name,
			MaxNameLen,
		)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' ||
			c == '_' {
			continue
		}
		return Error.Errorf(
			"lock name %q must be lowercase letters, digits, - and _ only",
			name,
		)
	}
	return nil
}

// writePID records the holder, for the error another process reports.
func writePID(file *os.File) error {
	if err := file.Truncate(0); err != nil {
		return err
	}
	_, err := file.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return err
}

// readPID returns the pid in the lock file, or 0: an unreadable file makes the
// error vaguer, never fatal.
func readPID(file *os.File) int {
	var buf [32]byte
	n, _ := file.ReadAt(buf[:], 0)
	pid, err := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if err != nil {
		return 0
	}
	return pid
}
