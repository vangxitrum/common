package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	config "gitlab.internal/tuan.quang.tran/aioz-common/config"
)

// newConfigCmd builds a cobra command whose flags are the subject of the tests.
// SaveConfig reads flags and their annotations, so the command is the fixture.
func newConfigCmd(t *testing.T) *cobra.Command {
	t.Helper()

	cmd := &cobra.Command{Use: "test", Run: func(*cobra.Command, []string) {}}
	flags := cmd.Flags()

	flags.String("plain", "default-value", "a plain setting")
	flags.Int("number", 42, "a numeric setting")
	flags.Float64("ratio", 0.25, "a float setting")

	return cmd
}

// annotate marks a flag the way BindFlags does, so SaveConfig sees the same
// metadata it would in a real binary.
func annotate(t *testing.T, cmd *cobra.Command, name, key, value string) {
	t.Helper()

	require.NoError(
		t,
		cmd.Flags().SetAnnotation(name, key, []string{value}),
	)
}

// saveTo runs SaveConfig into a temp file and returns the file's contents.
func saveTo(
	t *testing.T,
	cmd *cobra.Command,
	opts ...config.SaveOption,
) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), config.DefaultCfgFilename)
	require.NoError(t, config.New().SaveConfig(cmd, out, opts...))

	data, err := os.ReadFile(out)
	require.NoError(t, err)
	return string(data)
}

// TestSaveConfigCommentsOutDefaults covers the central convention of the
// generated config: a setting left at its default is written commented out, and
// only settings the operator actually chose are written live.
//
// This is what makes a generated config safe to regenerate. If defaults were
// written live, every default would be frozen into the file at setup time, and
// a later change to a default would silently not apply to existing nodes.
func TestSaveConfigCommentsOutDefaults(t *testing.T) {
	t.Parallel()

	cmd := newConfigCmd(t)
	contents := saveTo(t, cmd)

	require.Contains(
		t, contents, "# plain: default-value",
		"an untouched default must be commented out, so a future change to "+
			"the default still reaches nodes that never set it",
	)
	require.Contains(t, contents, "# a plain setting",
		"the flag's usage text is written as a comment above it")
}

// TestSaveConfigWritesOverridesLive asserts an override is written uncommented:
// it is a deliberate choice, so it must actually take effect.
func TestSaveConfigWritesOverridesLive(t *testing.T) {
	t.Parallel()

	cmd := newConfigCmd(t)
	contents := saveTo(
		t, cmd, config.SaveConfigWithOverride("plain", "chosen-value"),
	)

	require.Contains(t, contents, "plain: chosen-value")
	require.NotContains(
		t, contents, "# plain: chosen-value",
		"an overridden setting must be live, not commented out",
	)
}

// TestSaveConfigWithOverridesReplacesTheMap covers the plural option, and that
// it carries several values at once.
func TestSaveConfigWithOverridesReplacesTheMap(t *testing.T) {
	t.Parallel()

	cmd := newConfigCmd(t)
	contents := saveTo(t, cmd, config.SaveConfigWithOverrides(map[string]any{
		"plain":  "from-map",
		"number": 7,
	}))

	require.Contains(t, contents, "plain: from-map")
	require.Contains(t, contents, "number: 7")
}

// TestSaveConfigWithOverrideAccumulates asserts the singular option can be
// passed repeatedly. It allocates the map on first use, so a second call must
// add to it rather than replace it.
func TestSaveConfigWithOverrideAccumulates(t *testing.T) {
	t.Parallel()

	cmd := newConfigCmd(t)
	contents := saveTo(
		t, cmd,
		config.SaveConfigWithOverride("plain", "first"),
		config.SaveConfigWithOverride("number", 99),
	)

	require.Contains(t, contents, "plain: first")
	require.Contains(
		t, contents, "number: 99",
		"a second SaveConfigWithOverride must add to the overrides, not "+
			"discard the first",
	)
}

// TestSaveConfigOmitsSetupAndHiddenFlags covers the annotations that keep
// internal plumbing out of an operator-facing file.
func TestSaveConfigOmitsSetupAndHiddenFlags(t *testing.T) {
	t.Parallel()

	cmd := newConfigCmd(t)
	cmd.Flags().String("setup-only", "x", "only meaningful during setup")
	cmd.Flags().String("hidden-one", "y", "not for humans")
	annotate(t, cmd, "setup-only", "setup", "true")
	annotate(t, cmd, "hidden-one", "hidden", "true")

	contents := saveTo(t, cmd)

	require.NotContains(t, contents, "setup-only")
	require.NotContains(t, contents, "hidden-one")
	require.Contains(t, contents, "plain", "ordinary flags are still written")
}

// TestSaveConfigOmitsFlagSourceSettings covers the source annotation: values
// that only make sense as command-line flags must never be persisted, or a
// stale file would keep re-applying them.
func TestSaveConfigOmitsFlagSourceSettings(t *testing.T) {
	t.Parallel()

	cmd := newConfigCmd(t)
	cmd.Flags().String("flag-only", "z", "command line only")
	annotate(t, cmd, "flag-only", "source", config.FlagSource)

	require.NotContains(t, saveTo(t, cmd), "flag-only")
}

// TestSaveConfigRemovingDeprecated covers the opt-in cleanup, including that
// deprecated settings are kept by default - dropping them silently from an
// existing config would change a node's behavior on a rewrite.
func TestSaveConfigRemovingDeprecated(t *testing.T) {
	t.Parallel()

	build := func(t *testing.T) *cobra.Command {
		cmd := newConfigCmd(t)
		cmd.Flags().String("old-thing", "legacy", "kept for compatibility")
		annotate(t, cmd, "old-thing", "deprecated", "true")
		return cmd
	}

	require.Contains(
		t, saveTo(t, build(t)), "old-thing",
		"deprecated settings are retained unless removal is requested",
	)
	require.NotContains(
		t, saveTo(t, build(t), config.SaveConfigRemovingDeprecated()),
		"old-thing",
	)
}

// TestSaveConfigOmitsHelp pins the special case for the stdlib help flag, which
// has no annotation to exclude it and would otherwise be persisted.
func TestSaveConfigOmitsHelp(t *testing.T) {
	t.Parallel()

	contents := saveTo(t, newConfigCmd(t))

	for _, line := range strings.Split(contents, "\n") {
		require.NotEqual(t, "help: false", strings.TrimSpace(line))
		require.NotEqual(t, "# help: false", strings.TrimSpace(line))
	}
}

// TestSaveConfigSortsKeys asserts a stable ordering. The file is regenerated by
// setup commands and read by humans, so map iteration order would produce a
// spurious diff on every write.
func TestSaveConfigSortsKeys(t *testing.T) {
	t.Parallel()

	contents := saveTo(t, newConfigCmd(t))

	var keys []string
	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimPrefix(strings.TrimSpace(line), "# ")
		name, _, found := strings.Cut(line, ":")
		if !found || strings.Contains(name, " ") {
			continue
		}
		keys = append(keys, name)
	}

	require.NotEmpty(t, keys)
	require.IsIncreasing(
		t, keys,
		"keys must be sorted, or every regeneration produces a noisy diff",
	)
}

// TestSaveConfigWritesRestrictivePermissions matters because a config file can
// carry credentials; it must not be world-readable.
func TestSaveConfigWritesRestrictivePermissions(t *testing.T) {
	t.Parallel()

	out := filepath.Join(t.TempDir(), config.DefaultCfgFilename)
	require.NoError(t, config.New().SaveConfig(newConfigCmd(t), out))

	info, err := os.Stat(out)
	require.NoError(t, err)
	require.Zero(
		t, info.Mode().Perm()&0o077,
		"config must not be group- or world-readable; got %v",
		info.Mode().Perm(),
	)
}

// TestSaveConfigIndentsListsWithTwoSpaces pins one of the two differences
// between yaml.v2, which this code was written against, and the yaml.v3
// encoder it now uses. v3 defaults to a 4-space indent; keeping 2 means
// regenerating a config in the field does not rewrite every list in it.
func TestSaveConfigIndentsListsWithTwoSpaces(t *testing.T) {
	t.Parallel()

	cmd := newConfigCmd(t)
	cmd.Flags().StringSlice("peers", []string{"alpha", "beta"}, "peer list")

	require.Contains(t, saveTo(t, cmd), "# peers:\n#   - alpha\n#   - beta")
}

// TestSaveConfigQuotesYAML11Booleans pins the encoder's handling of strings
// that a YAML 1.1 reader would take for booleans. yaml.v3 follows YAML 1.2,
// where only true/false are boolean, but it still quotes these on output, so
// the generated file stays unambiguous for readers of either dialect.
func TestSaveConfigQuotesYAML11Booleans(t *testing.T) {
	t.Parallel()

	cmd := newConfigCmd(t)
	cmd.Flags().String("confirm", "yes", "a string that looks like a bool")

	require.Contains(t, saveTo(t, cmd), `# confirm: "yes"`)
}
