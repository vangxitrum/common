package proclock_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/proclock"
)

// holderEnv names the lock directory a helper process should hold, which is
// how a test gets a second process onto the same lock.
const holderEnv = "PROCLOCK_TEST_HOLD_DIR"

func config(t *testing.T) proclock.Config {
	t.Helper()
	return proclock.Config{Dir: t.TempDir()}
}

func TestAcquireWritesThePID(t *testing.T) {
	t.Parallel()

	cfg := config(t)
	lock, err := proclock.Acquire(cfg, "api")
	require.NoError(t, err)
	defer func() { require.NoError(t, lock.Close()) }()

	require.Equal(t, filepath.Join(cfg.Dir, "api.lock"), lock.Path())
	data, err := os.ReadFile(lock.Path())
	require.NoError(t, err)
	require.Equal(t, strconv.Itoa(os.Getpid()), strings.TrimSpace(string(data)))
}

// A second open of the same file conflicts even inside one process, because
// the lock belongs to the open file description, not to the process.
func TestASecondAcquireIsRefused(t *testing.T) {
	t.Parallel()

	cfg := config(t)
	lock, err := proclock.Acquire(cfg, "api")
	require.NoError(t, err)
	defer func() { require.NoError(t, lock.Close()) }()

	_, err = proclock.Acquire(cfg, "api")
	require.Error(t, err)
	require.ErrorIs(t, err, proclock.ErrHeld)

	var held *proclock.HeldError
	require.ErrorAs(t, err, &held)
	require.Equal(t, "api", held.Name)
	require.Equal(t, lock.Path(), held.Path)
	require.Equal(t, os.Getpid(), held.PID)
	require.Contains(t, err.Error(), strconv.Itoa(os.Getpid()))
}

func TestAnotherNameIsFree(t *testing.T) {
	t.Parallel()

	cfg := config(t)
	api, err := proclock.Acquire(cfg, "api")
	require.NoError(t, err)
	defer func() { require.NoError(t, api.Close()) }()

	migrate, err := proclock.Acquire(cfg, "migrate")
	require.NoError(t, err)
	require.NoError(t, migrate.Close())
}

func TestCloseReleases(t *testing.T) {
	t.Parallel()

	cfg := config(t)
	lock, err := proclock.Acquire(cfg, "api")
	require.NoError(t, err)
	require.NoError(t, lock.Close())
	require.NoError(t, lock.Close(), "Close is idempotent")

	again, err := proclock.Acquire(cfg, "api")
	require.NoError(t, err)
	require.NoError(t, again.Close())
}

func TestTheDirectoryIsCreated(t *testing.T) {
	t.Parallel()

	cfg := proclock.Config{Dir: filepath.Join(t.TempDir(), "run", "locks")}
	lock, err := proclock.Acquire(cfg, "api")
	require.NoError(t, err)
	require.NoError(t, lock.Close())
	require.FileExists(t, filepath.Join(cfg.Dir, "api.lock"))
}

// The name goes into a path, so anything that could climb out of Dir or make
// an unreadable filename is refused before the file is opened.
func TestBadNamesAreRefused(t *testing.T) {
	t.Parallel()

	cfg := config(t)
	for _, name := range []string{
		"", "..", "../escape", "a/b", "a\\b", "API", "a b", "a.lock",
		strings.Repeat("a", 65), "\x00",
	} {
		_, err := proclock.Acquire(cfg, name)
		require.Error(t, err, "name %q", name)
		require.NotErrorIs(t, err, proclock.ErrHeld, "name %q", name)
	}

	for _, name := range []string{"a", "api", "delete-expired_tokens", strings.Repeat("a", 64)} {
		lock, err := proclock.Acquire(cfg, name)
		require.NoError(t, err, "name %q", name)
		require.NoError(t, lock.Close())
	}
}

func TestAnEmptyDirIsAnError(t *testing.T) {
	t.Parallel()

	_, err := proclock.Acquire(proclock.Config{}, "api")
	require.Error(t, err)
}

// The lock is held by a real second process, and the kernel releases it when
// that process is killed: no stale lock, nothing to clean up.
func TestAnotherProcessHoldsAndAKillReleases(t *testing.T) {
	t.Parallel()

	cfg := config(t)
	holder := exec.Command(
		os.Args[0],
		"-test.run=TestHelperHoldsTheLock",
		"-test.v",
	)
	holder.Env = append(os.Environ(), holderEnv+"="+cfg.Dir)
	stdout, err := holder.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, holder.Start())
	defer func() { _ = holder.Process.Kill(); _, _ = holder.Process.Wait() }()

	// The helper prints one line once it holds the lock.
	buf := make([]byte, 1)
	deadline := time.Now().Add(30 * time.Second)
	var line strings.Builder
	for !strings.Contains(line.String(), "HELD") && time.Now().Before(deadline) {
		n, err := stdout.Read(buf)
		require.NoError(t, err)
		line.Write(buf[:n])
	}
	require.Contains(t, line.String(), "HELD")

	_, err = proclock.Acquire(cfg, "api")
	require.ErrorIs(t, err, proclock.ErrHeld)
	var held *proclock.HeldError
	require.ErrorAs(t, err, &held)
	require.Equal(t, holder.Process.Pid, held.PID)

	require.NoError(t, holder.Process.Signal(syscall.SIGKILL))
	_, _ = holder.Process.Wait()

	lock, err := proclock.Acquire(cfg, "api")
	require.NoError(t, err, "a killed holder leaves no stale lock")
	require.NoError(t, lock.Close())
}

// TestHelperHoldsTheLock is not a test: it is the body of the helper process
// started above, and does nothing in a normal run.
func TestHelperHoldsTheLock(t *testing.T) {
	dir := os.Getenv(holderEnv)
	if dir == "" {
		t.Skip("helper process only")
	}

	lock, err := proclock.Acquire(proclock.Config{Dir: dir}, "api")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()

	if _, err := os.Stdout.WriteString("HELD\n"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Minute)
}

func TestHeldErrorSaysWhoHoldsIt(t *testing.T) {
	t.Parallel()

	err := error(
		&proclock.HeldError{Name: "api", Path: "/run/api.lock", PID: 4123},
	)
	require.ErrorIs(t, err, proclock.ErrHeld)
	require.Contains(t, err.Error(), "api")
	require.Contains(t, err.Error(), "4123")
	require.Contains(t, err.Error(), "/run/api.lock")

	unknown := error(&proclock.HeldError{Name: "api", Path: "/run/api.lock"})
	require.NotContains(t, unknown.Error(), " 0")
	require.True(t, errors.Is(unknown, proclock.ErrHeld))
}
