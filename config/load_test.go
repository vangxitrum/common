package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"

	config "gitlab.internal/tuan.quang.tran/aioz-config"
)

// loadFixture is a command with no bound config struct, so every key in the
// config file takes the flag propagation path through Load rather than being
// decoded straight into a struct. That path is where the two bugs this file
// regresses against lived.
type loadFixture struct {
	registry *config.Registry
	cmd      *cobra.Command
	dir      string
}

func newLoadFixture(t *testing.T, contents string) *loadFixture {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, config.DefaultCfgFilename),
		[]byte(contents),
		0o600,
	))

	cmd := &cobra.Command{Use: "test", Run: func(*cobra.Command, []string) {}}
	cmd.Flags().String("config-dir", dir, "config directory")
	cmd.Flags().String("address", "127.0.0.1:8080", "listen address")
	cmd.Flags().Duration("timeout", 0, "request timeout")
	cmd.Flags().StringSlice("peers", []string{"alpha", "beta"}, "peer list")
	require.NoError(t, cmd.ParseFlags(nil))

	f := &loadFixture{registry: config.New(), cmd: cmd, dir: dir}
	// Bind a struct with no fields: every key in the file is then unclaimed by
	// any struct, which is exactly the flag propagation path under test.
	f.registry.Bind(cmd, &struct{}{})
	return f
}

func (f *loadFixture) load(
	t *testing.T,
	opts ...config.LoadOption,
) *config.LoadResult {
	t.Helper()

	res, err := f.registry.Load(f.cmd, opts...)
	require.NoError(t, err)
	return res
}

func TestLoadPartitionsKeys(t *testing.T) {
	t.Parallel()

	f := newLoadFixture(t, "address: 10.0.0.1:9000\nunknown-key: whatever\n")
	res := f.load(t)

	require.Contains(t, res.UsedKeys, "address")
	require.Contains(t, res.MissingKeys, "unknown-key")
	require.Empty(t, res.BrokenKeys)
	require.Equal(
		t,
		"10.0.0.1:9000",
		f.cmd.Flags().Lookup("address").Value.String(),
	)
}

func TestLoadReportsSortedKeys(t *testing.T) {
	t.Parallel()

	f := newLoadFixture(t, "zebra: 1\nalpha: 2\nmiddle: 3\n")
	res := f.load(t)

	require.Equal(t, []string{"alpha", "middle", "zebra"}, res.MissingKeys)
}

// TestLoadMarksFileValuesAsChanged covers the side effect SaveConfig depends
// on: a value that came from the config file must look "set" afterwards, or
// regenerating the file would comment it back out.
func TestLoadMarksFileValuesAsChanged(t *testing.T) {
	t.Parallel()

	f := newLoadFixture(t, "address: 10.0.0.1:9000\n")
	f.load(t)

	require.True(t, f.cmd.Flags().Lookup("address").Changed)
}

// TestLoadDoesNotMarkBrokenValuesAsChanged is a regression test. The original
// marked a flag as changed before checking whether the value had parsed, so an
// unparseable value was persisted live into the next generated config,
// producing a file that its own binary would then refuse.
func TestLoadDoesNotMarkBrokenValuesAsChanged(t *testing.T) {
	t.Parallel()

	f := newLoadFixture(t, "timeout: not-a-duration\n")
	res := f.load(t)

	require.Contains(t, res.BrokenKeys, "timeout")
	require.False(
		t,
		f.cmd.Flags().Lookup("timeout").Changed,
		"a value that failed to parse must not be recorded as set",
	)
	require.NotContains(t, res.UsedKeys, "timeout")
}

// TestLoadReadsListValuesAsLists is the second regression test. Every missing
// key used to be read as a string, and viper cannot cast a list to one, so a
// list-valued setting was silently replaced with an empty list.
func TestLoadReadsListValuesAsLists(t *testing.T) {
	t.Parallel()

	f := newLoadFixture(t, "peers:\n  - gamma\n  - delta\n")
	f.load(t)

	require.Equal(
		t,
		"[gamma,delta]",
		f.cmd.Flags().Lookup("peers").Value.String(),
		"a list in the config file must survive as a list",
	)
}

func TestLoadFailOnValueError(t *testing.T) {
	t.Parallel()

	f := newLoadFixture(t, "timeout: not-a-duration\n")

	res, err := f.registry.Load(f.cmd, config.WithFailOnValueError())
	require.Error(t, err)
	require.NotNil(
		t,
		res,
		"the key sets are still reported alongside the error",
	)
	require.Contains(t, res.BrokenKeys, "timeout")
}

// TestLoadTwiceIsStable guards repeated loads, which happen whenever a setup
// command loads an existing config before rewriting it.
func TestLoadTwiceIsStable(t *testing.T) {
	t.Parallel()

	f := newLoadFixture(t, "address: 10.0.0.1:9000\n")

	first := f.load(t)
	second := f.load(t)

	require.Equal(t, first.UsedKeys, second.UsedKeys)
	require.Equal(t, first.MissingKeys, second.MissingKeys)
	require.Same(
		t,
		first.Viper,
		second.Viper,
		"the viper is memoized per command",
	)
}

// TestForgetDiscardsMemoizedViper covers the escape hatch from that
// memoization, which a long-lived process needs so that a command's config and
// flag set are not pinned forever.
func TestForgetDiscardsMemoizedViper(t *testing.T) {
	t.Parallel()

	f := newLoadFixture(t, "address: 10.0.0.1:9000\n")
	first := f.load(t)

	f.registry.Forget(f.cmd)
	second := f.load(t)

	require.NotSame(t, first.Viper, second.Viper)
}

// TestLoadBeforeParseIsRefused stops the silent failure where a command loads
// its config before cobra has parsed --config-dir, and so reads nothing.
func TestLoadBeforeParseIsRefused(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String("config-dir", t.TempDir(), "config directory")

	_, err := config.New().Load(cmd)
	require.ErrorIs(t, err, config.ErrFlagsNotParsed)
}

// TestCustomConfigLoaderCanCallBackIntoTheRegistry pins the deadlock fix: the
// registry must not hold its lock while running a caller-supplied loader,
// because a reasonable loader inspects the registry.
func TestCustomConfigLoaderCanCallBackIntoTheRegistry(t *testing.T) {
	t.Parallel()

	f := newLoadFixture(t, "address: 10.0.0.1:9000\n")

	other := &cobra.Command{Use: "other"}
	other.Flags().String("config-dir", f.dir, "config directory")
	require.NoError(t, other.ParseFlags(nil))

	loader := func(cmd *cobra.Command, vip *viper.Viper) error {
		// a loader that consults another command's configuration would
		// deadlock if the registry lock were held across this call
		if _, err := f.registry.Viper(other); err != nil {
			return err
		}
		return config.ReadConfigFiles(cmd, vip)
	}

	res := f.load(t, config.WithConfigLoader(loader))
	require.Contains(t, res.UsedKeys, "address")
}

// TestReadConfigFilesWithoutConfigDir covers a command that has no config-dir
// flag at all, which must fall back to flag defaults rather than fail.
func TestReadConfigFilesWithoutConfigDir(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{Use: "test", Run: func(*cobra.Command, []string) {}}
	cmd.Flags().String("address", "127.0.0.1:8080", "listen address")
	require.NoError(t, cmd.ParseFlags(nil))

	res, err := config.New().Load(cmd)
	require.NoError(t, err)
	require.Empty(t, res.BrokenKeys)
}
