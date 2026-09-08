package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	config "gitlab.internal/tuan.quang.tran/aioz-config"
)

// ServerConfig is a nested config struct, so the e2e test covers dotted flag
// names such as "server.address".
type ServerConfig struct {
	Address string        `help:"address to listen on" default:"127.0.0.1:8080"`
	Timeout time.Duration `help:"request timeout"      default:"30s"`
}

// AppConfig exercises every tag whose behavior the generated file depends on.
type AppConfig struct {
	Server   ServerConfig
	DataDir  string   `path:"true" help:"data directory" default:"$CONFDIR/data"`
	Peers    []string `            help:"peer list"      default:"alpha,beta"`
	Operator string   `            help:"operator name"  default:"unset"         user:"true"`

	Overwrite bool   `setup:"true" help:"overwrite existing config" default:"false"`
	Secret    string `             help:"internal secret"           default:"shh"   hidden:"true"`
	Verbose   bool   `             help:"verbose output"            default:"false"               source:"flag"`
}

// harness is one binary's worth of wiring: a root command with a
// --config-dir flag, a setup subcommand that generates the config file, and a
// run subcommand that loads it.
type harness struct {
	registry *config.Registry
	root     *cobra.Command
	setupCmd *cobra.Command
	runCmd   *cobra.Command
	cfg      *AppConfig
	result   *config.LoadResult
	loadErr  error
}

// newHarness builds a fresh command tree. Every call gets its own registry and
// its own *cobra.Command values, so a test can generate a config with one
// "binary" and load it with another, exactly as a real setup/run pair does.
func newHarness(configDir string, loadOpts ...config.LoadOption) *harness {
	h := &harness{registry: config.New(), cfg: &AppConfig{}}

	var configDirFlag string
	h.root = &cobra.Command{Use: "app"}
	config.SetupFlag(
		nil,
		h.root,
		&configDirFlag,
		"config-dir",
		configDir,
		"the directory to load the config from",
	)

	setupCfg := &struct{ AppConfig }{}
	setupCmd := &cobra.Command{
		Use:         "setup",
		Annotations: map[string]string{"type": "setup"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Load first so that regenerating preserves whatever an existing
			// config file already sets, exactly as a real setup command does.
			if _, err := h.registry.Load(cmd, loadOpts...); err != nil {
				return err
			}
			return h.registry.SaveConfig(
				cmd,
				filepath.Join(configDir, config.DefaultCfgFilename),
			)
		},
	}
	runCmd := &cobra.Command{
		Use: "run",
		RunE: func(cmd *cobra.Command, _ []string) error {
			h.result, h.loadErr = h.registry.Load(cmd, loadOpts...)
			return h.loadErr
		},
	}
	h.root.AddCommand(setupCmd, runCmd)
	h.setupCmd, h.runCmd = setupCmd, runCmd

	h.registry.Bind(
		setupCmd,
		setupCfg,
		config.UseReleaseDefaults(),
		config.SetupMode(),
		config.ConfDir(configDir),
	)
	h.registry.Bind(
		runCmd,
		h.cfg,
		config.UseReleaseDefaults(),
		config.ConfDir(configDir),
	)

	return h
}

func (h *harness) execute(args ...string) error {
	h.root.SetArgs(args)
	h.root.SetOut(nopWriter{})
	h.root.SetErr(nopWriter{})
	return h.root.Execute()
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// generate runs the setup command and returns the generated file's contents.
func generate(t *testing.T, dir string) string {
	t.Helper()

	require.NoError(t, newHarness(dir).execute("setup"))

	data, err := os.ReadFile(filepath.Join(dir, config.DefaultCfgFilename))
	require.NoError(t, err)
	return string(data)
}

func TestGeneratesDefaultConfigFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	contents := generate(t, dir)

	// Defaults are written commented out, with their help text above them, so
	// the file documents every setting without pinning any of them.
	require.Contains(t, contents, "# address to listen on")
	require.Contains(t, contents, "# server.address: 127.0.0.1:8080")
	require.Contains(t, contents, "# server.timeout: 30s")

	// A user:"true" field is written live even though it is still a default,
	// because it is one the operator is expected to edit.
	require.Contains(t, contents, "\noperator: unset")

	// setup, hidden and source:"flag" fields are never persisted.
	require.NotContains(t, contents, "overwrite")
	require.NotContains(t, contents, "secret")
	require.NotContains(t, contents, "verbose")

	// $CONFDIR is expanded against the configured directory.
	require.Contains(t, contents, "# data-dir: "+filepath.Join(dir, "data"))

	// Keys are sorted, so regenerating produces a stable diff.
	require.Equal(t, sortedConfigKeys(contents), configKeys(contents))

	info, err := os.Stat(filepath.Join(dir, config.DefaultCfgFilename))
	require.NoError(t, err)
	require.Zero(
		t,
		info.Mode().Perm()&0o077,
		"config file may hold secrets and must not be group or world readable",
	)
}

// TestGeneratedListIndent pins the YAML indent of list values. yaml.v3
// defaults to 4 spaces where yaml.v2 used 2; the fleet's existing files use 2,
// so a change here rewrites every generated config in place.
func TestGeneratedListIndent(t *testing.T) {
	t.Parallel()

	contents := generate(t, t.TempDir())

	require.Contains(t, contents, "# peers:\n#   - alpha\n#   - beta")
}

func TestLoadsValuesFromConfigFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	generate(t, dir)
	writeConfig(t, dir, map[string]string{
		"server.address": "10.0.0.1:9000",
		"server.timeout": "45s",
		"operator":       "tuan",
		"peers":          "[gamma, delta]",
	})

	h := newHarness(dir)
	require.NoError(t, h.execute("run"))

	require.Equal(t, "10.0.0.1:9000", h.cfg.Server.Address)
	require.Equal(t, 45*time.Second, h.cfg.Server.Timeout)
	require.Equal(t, "tuan", h.cfg.Operator)
	require.Equal(t, []string{"gamma", "delta"}, h.cfg.Peers)
	require.Empty(t, h.result.BrokenKeys)
	require.Empty(t, h.result.MissingKeys)
}

// TestLoadedValuesAreRegeneratedLive guards the side effect the whole
// generate/edit/regenerate cycle rests on: a flag shares storage with the
// struct field it was bound to, so decoding the config file into the struct
// also moves the flag off its default, which is what makes SaveConfig write
// the value out live instead of commenting it out again.
func TestLoadedValuesAreRegeneratedLive(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	generate(t, dir)
	writeConfig(t, dir, map[string]string{"server.address": "10.0.0.1:9000"})

	h := newHarness(dir)
	require.NoError(t, h.execute("run"))
	require.Equal(
		t,
		"10.0.0.1:9000",
		h.runCmd.Flags().Lookup("server.address").Value.String(),
		"decoding the config file must move the bound flag off its default",
	)

	require.NoError(t, newHarness(dir).execute("setup"))
	data, err := os.ReadFile(filepath.Join(dir, config.DefaultCfgFilename))
	require.NoError(t, err)
	require.Contains(t, string(data), "\nserver.address: 10.0.0.1:9000")
}

func TestEnvOverridesConfigFileAndFlagOverridesEnv(t *testing.T) {
	dir := t.TempDir()
	generate(t, dir)
	writeConfig(t, dir, map[string]string{"server.address": "10.0.0.1:9000"})

	t.Setenv("AIOZ_SERVER_ADDRESS", "172.16.0.1:1234")

	h := newHarness(dir)
	require.NoError(t, h.execute("run"))
	require.Equal(t, "172.16.0.1:1234", h.cfg.Server.Address,
		"environment must win over the config file")

	h = newHarness(dir)
	require.NoError(t, h.execute("run", "--server.address=192.168.0.1:4321"))
	require.Equal(t, "192.168.0.1:4321", h.cfg.Server.Address,
		"an explicit flag must win over the environment")
}

func TestEnvPrefixIsConfigurable(t *testing.T) {
	dir := t.TempDir()
	generate(t, dir)
	t.Setenv("CUSTOM_SERVER_ADDRESS", "203.0.113.1:80")

	h := newHarness(dir, config.WithEnvPrefix("custom"))
	require.NoError(t, h.execute("run"))
	require.Equal(t, "203.0.113.1:80", h.cfg.Server.Address)
}

func TestSecretsFileMergesOverConfigFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	generate(t, dir)
	writeConfig(t, dir, map[string]string{
		"server.address": "10.0.0.1:9000",
		"operator":       "from-config",
	})
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, config.DefaultSecretFilename),
		[]byte("operator: from-secrets\n"),
		0o600,
	))

	h := newHarness(dir)
	require.NoError(t, h.execute("run"))

	require.Equal(t, "from-secrets", h.cfg.Operator,
		"secrets.yaml must be merged over config.yaml")
	require.Equal(t, "10.0.0.1:9000", h.cfg.Server.Address,
		"keys absent from secrets.yaml must survive the merge")
}

func TestMissingConfigFileIsNotAnError(t *testing.T) {
	t.Parallel()

	h := newHarness(t.TempDir())
	require.NoError(t, h.execute("run"))
	require.Equal(t, "127.0.0.1:8080", h.cfg.Server.Address)
}

func TestUnknownKeyIsReportedAsMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	generate(t, dir)
	writeConfig(t, dir, map[string]string{"server.adress": "typo"})

	h := newHarness(dir)
	require.NoError(t, h.execute("run"))
	require.Contains(t, h.result.MissingKeys, "server.adress")
}

func TestUnparseableValueIsReportedAsBroken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	generate(t, dir)
	writeConfig(t, dir, map[string]string{"server.timeout": "not-a-duration"})

	h := newHarness(dir)
	require.NoError(t, h.execute("run"))
	require.Contains(t, h.result.BrokenKeys, "server.timeout")

	// The same file with WithFailOnValueError must stop the command instead.
	h = newHarness(dir, config.WithFailOnValueError())
	require.Error(t, h.execute("run"))
}

func TestLoadBeforeFlagsParsedIsRefused(t *testing.T) {
	t.Parallel()

	h := newHarness(t.TempDir())
	_, err := h.registry.Load(h.runCmd)
	require.ErrorIs(t, err, config.ErrFlagsNotParsed)
}

// writeConfig rewrites the generated config file with the given keys set live,
// standing in for an operator editing it by hand.
func writeConfig(t *testing.T, dir string, values map[string]string) {
	t.Helper()

	path := filepath.Join(dir, config.DefaultCfgFilename)
	data, err := os.ReadFile(path)
	require.NoError(t, err)

	// Drop whatever the generator wrote for these keys, commented or live, so
	// the result stays a valid document rather than one with duplicate keys.
	var b strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		if _, edited := values[lineKey(line)]; edited {
			continue
		}
		b.WriteString(line + "\n")
	}
	for key, value := range values {
		b.WriteString(key + ": " + value + "\n")
	}
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o600))
}

// lineKey returns the setting a generated line defines, if any, looking
// through the comment marker that unset values carry.
func lineKey(line string) string {
	line = strings.TrimPrefix(line, "# ")
	key, _, found := strings.Cut(line, ":")
	if !found || strings.ContainsAny(key, " \t") {
		return ""
	}
	return key
}

// configKeys returns the keys of the generated file in the order they appear,
// counting commented-out settings.
func configKeys(contents string) []string {
	var keys []string
	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimPrefix(line, "# ")
		key, _, found := strings.Cut(line, ":")
		// help comments have no colon, and list items are indented
		if !found || strings.HasPrefix(key, " ") || strings.Contains(key, " ") {
			continue
		}
		keys = append(keys, key)
	}
	return keys
}

func sortedConfigKeys(contents string) []string {
	keys := configKeys(contents)
	sorted := make([]string, len(keys))
	copy(sorted, keys)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	return sorted
}
