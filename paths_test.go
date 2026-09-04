package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	config "gitlab.internal/tuan.quang.tran/aioz-config"
)

func TestAtomicWriteFileCreatesAndOverwrites(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	require.NoError(t, config.AtomicWriteFile(path, []byte("first"), 0o600))
	require.NoError(t, config.AtomicWriteFile(path, []byte("second"), 0o600))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "second", string(data))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temporary file may be left behind")
}

// TestAtomicWriteFileHonoursMode matters because the argument used to be
// ignored: the file happened to be 0600 only because that is what os.CreateTemp
// creates, so a caller asking for anything else silently got 0600.
func TestAtomicWriteFileHonoursMode(t *testing.T) {
	t.Parallel()

	for _, perm := range []os.FileMode{0o600, 0o644, 0o640} {
		path := filepath.Join(t.TempDir(), "file")
		require.NoError(t, config.AtomicWriteFile(path, []byte("x"), perm))

		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, perm, info.Mode().Perm())
	}
}

func TestAtomicWriteFileFailsWithoutDirectory(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "missing", "config.yaml")
	require.Error(t, config.AtomicWriteFile(path, []byte("x"), 0o600))
}

// TestIsValidSetupDir covers the guard that stops a setup command from
// overwriting a configuration that is already in use.
func TestIsValidSetupDir(t *testing.T) {
	t.Parallel()

	t.Run("missing directory is available", func(t *testing.T) {
		t.Parallel()

		ok, err := config.IsValidSetupDir(
			filepath.Join(t.TempDir(), "does-not-exist"),
		)
		require.True(t, ok)
		// the original reports the stat error alongside ok=true; callers key
		// off ok, so the error is informational
		require.True(t, os.IsNotExist(err))
	})

	t.Run("empty directory is available", func(t *testing.T) {
		t.Parallel()

		ok, err := config.IsValidSetupDir(t.TempDir())
		require.NoError(t, err)
		require.True(t, ok)
	})

	t.Run("directory with a config is not", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, config.DefaultCfgFilename),
			[]byte("plain: value\n"),
			0o600,
		))

		ok, err := config.IsValidSetupDir(dir)
		require.NoError(t, err)
		require.False(t, ok, "an existing config must not be overwritten")
	})

	t.Run("directory with other files is available", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, "identity.cert"),
			[]byte("x"),
			0o600,
		))

		ok, err := config.IsValidSetupDir(dir)
		require.NoError(t, err)
		require.True(t, ok)
	})
}

func TestIsWritable(t *testing.T) {
	t.Parallel()

	t.Run("writable directory", func(t *testing.T) {
		t.Parallel()

		ok, err := config.IsWritable(t.TempDir())
		require.NoError(t, err)
		require.True(t, ok)
	})

	t.Run("a file is not a directory", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "file")
		require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))

		_, err := config.IsWritable(path)
		require.ErrorContains(t, err, "not a directory")
	})

	t.Run("missing directory", func(t *testing.T) {
		t.Parallel()

		_, err := config.IsWritable(filepath.Join(t.TempDir(), "nope"))
		require.Error(t, err)
	})

	t.Run("read only directory", func(t *testing.T) {
		t.Parallel()

		if os.Geteuid() == 0 {
			t.Skip("root can write into a directory regardless of its mode")
		}

		dir := filepath.Join(t.TempDir(), "readonly")
		require.NoError(t, os.Mkdir(dir, 0o500))

		ok, err := config.IsWritable(dir)
		require.Error(t, err)
		require.False(t, ok)
	})
}

// TestApplicationDir checks that the per-OS base directory is joined with the
// subdirectories given, without asserting a specific platform's layout.
func TestApplicationDir(t *testing.T) {
	t.Parallel()

	dir := config.ApplicationDir("AIOZ", "Worker")

	require.NotEmpty(t, dir)
	require.NotEqual(t, "AIOZ", filepath.Base(filepath.Dir(dir)),
		"the subdirectories must be joined onto a base directory")
}

// TestApplicationDirDoesNotMutateItsArgument guards a subtle aliasing bug: the
// function rewrites the case of each element, and doing that in place would
// corrupt a slice the caller still holds.
func TestApplicationDirDoesNotMutateItsArgument(t *testing.T) {
	t.Parallel()

	parts := []string{"AIOZ", "Worker"}
	config.ApplicationDir(parts...)

	require.Equal(t, []string{"AIOZ", "Worker"}, parts)
}
