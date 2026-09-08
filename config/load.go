package config

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"github.com/zeebo/structs"
)

const (
	// DefaultCfgFilename is the default filename used for storing a
	// configuration.
	DefaultCfgFilename = "config.yaml"

	// DefaultSecretFilename is the name of the additional configuration file
	// merged over the main one. NOTE: it is parsed as regular yaml, no
	// encryption is provided. It is the responsibility of the runtime
	// environment to make it safe (eg. map a Kubernetes secret).
	DefaultSecretFilename = "secrets.yaml"

	// DefaultEnvPrefix is the environment variable prefix used when none is
	// configured. AIOZ_ENV_PREFIX overrides it at runtime.
	DefaultEnvPrefix = "aioz"
)

// ErrFlagsNotParsed is returned by Load when it is called before cobra has
// parsed the command's flags. Load reads --config-dir to find the config
// file, so it must run from RunE, PreRunE or PersistentPreRunE.
var ErrFlagsNotParsed = errors.New(
	"config: command flags are not parsed yet; call Load from RunE, PreRunE or PersistentPreRunE",
)

// ConfigLoader reads configuration sources into vip. ReadConfigFiles is the
// default implementation.
type ConfigLoader func(cmd *cobra.Command, vip *viper.Viper) error

// LoadResult reports what Load did with each configuration key.
type LoadResult struct {
	// Viper is the viper instance the values were read from.
	Viper *viper.Viper
	// UsedKeys were decoded into a bound config struct or set on a flag.
	UsedKeys []string
	// MissingKeys are present in the configuration but match no field of any
	// bound struct and no flag. Usually a typo or a stale config file.
	MissingKeys []string
	// BrokenKeys matched a field or flag but their value could not be parsed.
	BrokenKeys []string
}

type loadOptions struct {
	loader           ConfigLoader
	logger           *slog.Logger
	envPrefix        string
	cfgFilename      string
	secretFilename   string
	failOnValueError bool
}

// LoadOption configures Load and Viper.
type LoadOption func(*loadOptions)

// WithConfigLoader replaces ReadConfigFiles with a custom source of
// configuration.
func WithConfigLoader(loader ConfigLoader) LoadOption {
	return func(o *loadOptions) { o.loader = loader }
}

// WithLogger sets the logger used to report missing and broken keys. A nil
// logger, or no option at all, uses slog.Default.
func WithLogger(log *slog.Logger) LoadOption {
	return func(o *loadOptions) { o.logger = log }
}

// WithEnvPrefix overrides the environment variable prefix. The AIOZ_ENV_PREFIX
// environment variable still takes precedence.
func WithEnvPrefix(prefix string) LoadOption {
	return func(o *loadOptions) { o.envPrefix = prefix }
}

// WithFilenames overrides the config and secret file names looked for in the
// --config-dir directory. An empty name disables that file.
func WithFilenames(cfgFilename, secretFilename string) LoadOption {
	return func(o *loadOptions) {
		o.cfgFilename = cfgFilename
		o.secretFilename = secretFilename
	}
}

// WithFailOnValueError makes Load return an error when any configuration
// value fails to parse, instead of only logging it.
func WithFailOnValueError() LoadOption {
	return func(o *loadOptions) { o.failOnValueError = true }
}

func newLoadOptions(opts []LoadOption) *loadOptions {
	o := &loadOptions{
		logger:         slog.Default(),
		envPrefix:      DefaultEnvPrefix,
		cfgFilename:    DefaultCfgFilename,
		secretFilename: DefaultSecretFilename,
	}
	for _, opt := range opts {
		opt(o)
	}
	if o.logger == nil {
		o.logger = slog.Default()
	}
	if o.loader == nil {
		o.loader = func(cmd *cobra.Command, vip *viper.Viper) error {
			return readConfigFiles(cmd, vip, o.cfgFilename, o.secretFilename)
		}
	}
	return o
}

// Viper returns the viper instance for cmd, creating it on first use. Options
// only take effect on that first call; use Forget to discard a command's
// viper so that a later call rebuilds it.
func (r *Registry) Viper(
	cmd *cobra.Command,
	opts ...LoadOption,
) (*viper.Viper, error) {
	r.mu.Lock()
	existing := r.vipers[cmd]
	r.mu.Unlock()
	if existing != nil {
		return existing, nil
	}

	// Build outside the lock: the loader is caller-supplied and may call back
	// into the registry, which would deadlock on a non-reentrant mutex.
	options := newLoadOptions(opts)
	vip := viper.New()
	if err := vip.BindPFlags(cmd.Flags()); err != nil {
		return nil, fmt.Errorf("bind flags: %w", err)
	}

	prefix := os.Getenv("AIOZ_ENV_PREFIX")
	if prefix == "" {
		prefix = options.envPrefix
	}

	vip.SetEnvPrefix(prefix)
	vip.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	vip.AutomaticEnv()

	if err := options.loader(cmd, vip); err != nil {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Another goroutine may have built one while we were unlocked; prefer
	// theirs so every caller sees the same instance.
	if existing := r.vipers[cmd]; existing != nil {
		return existing, nil
	}
	r.vipers[cmd] = vip
	return vip, nil
}

// Viper returns the viper instance for cmd from the default registry.
func Viper(cmd *cobra.Command, opts ...LoadOption) (*viper.Viper, error) {
	return defaultRegistry.Viper(cmd, opts...)
}

// ViperWithCustomConfig returns the viper instance for cmd from the default
// registry, reading configuration through loadConfig instead of
// ReadConfigFiles.
func ViperWithCustomConfig(
	cmd *cobra.Command,
	loadConfig ConfigLoader,
) (*viper.Viper, error) {
	return defaultRegistry.Viper(cmd, WithConfigLoader(loadConfig))
}

// ReadConfigFiles loads configuration into vip from the directory named by the
// "config-dir" flag, reading DefaultCfgFilename and then merging
// DefaultSecretFilename over it. Missing files are not an error.
func ReadConfigFiles(cmd *cobra.Command, vip *viper.Viper) error {
	return readConfigFiles(
		cmd,
		vip,
		DefaultCfgFilename,
		DefaultSecretFilename,
	)
}

func readConfigFiles(
	cmd *cobra.Command,
	vip *viper.Viper,
	filenames ...string,
) error {
	cfgFlag := cmd.Flags().Lookup("config-dir")
	if cfgFlag == nil || cfgFlag.Value.String() == "" {
		return nil
	}
	dir := os.ExpandEnv(cfgFlag.Value.String())
	setupCommand := cmd.Annotations["type"] == "setup"

	read := (*viper.Viper).ReadInConfig
	for ix, name := range filenames {
		if ix > 0 {
			read = (*viper.Viper).MergeInConfig
		}
		if name == "" {
			continue
		}
		path := filepath.Join(dir, name)
		exists, err := fileExists(path)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		vip.SetConfigFile(path)
		// A setup command is allowed to run against an unreadable or
		// half-written config, since its job is to write a new one.
		if err := read(vip); err != nil && !setupCommand {
			return fmt.Errorf("read %s: %w", path, err)
		}
	}
	return nil
}

// fileExists reports whether path exists.
func fileExists(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("check for file existence %s: %w", path, err)
	}
	return true, nil
}

// Load reads the configuration files and environment into every config struct
// bound to cmd, and reports which keys were used, missing or broken. It must
// be called after cobra has parsed the command's flags, that is from RunE,
// PreRunE or PersistentPreRunE.
//
// Load also marks flags whose value came from the configuration as changed,
// which is what makes SaveConfig write them out live rather than commented.
func (r *Registry) Load(
	cmd *cobra.Command,
	opts ...LoadOption,
) (*LoadResult, error) {
	if !cmd.Flags().Parsed() {
		return nil, ErrFlagsNotParsed
	}

	options := newLoadOptions(opts)
	vip, err := r.Viper(cmd, opts...)
	if err != nil {
		return nil, err
	}

	var (
		brokenKeys  = map[string]struct{}{}
		missingKeys = map[string]struct{}{}
		usedKeys    = map[string]struct{}{}
		allSettings = vip.AllSettings()
	)

	for _, config := range r.boundConfigs(cmd) {
		res := structs.Decode(allSettings, config)
		for key := range res.Used {
			usedKeys[key] = struct{}{}
		}
		for key := range res.Missing {
			missingKeys[key] = struct{}{}
		}
		for key := range res.Broken {
			brokenKeys[key] = struct{}{}
		}
	}

	// Propagate keys that no struct claimed onto their flags, if any, and
	// remove any used keys from the missing set.
	for key := range missingKeys {
		switch flagName, f := lookupFlag(cmd, key); {
		case f != nil:
			if _, done := usedKeys[flagName]; done {
				// a list arrives as one key per element; the first of them
				// already set the whole flag
				usedKeys[key] = struct{}{}
				continue
			}
			if err := setFlagFromViper(f, vip, flagName); err != nil {
				brokenKeys[key] = struct{}{}
				continue
			}
			// Only a value that actually parsed counts as set; marking a
			// broken value as changed would make SaveConfig persist it.
			f.Changed = f.Value.String() != f.DefValue
			usedKeys[flagName] = struct{}{}
			usedKeys[key] = struct{}{}
		default:
			if f := flag.Lookup(key); f != nil {
				if err := f.Value.Set(vip.GetString(key)); err != nil {
					brokenKeys[key] = struct{}{}
					continue
				}
				usedKeys[key] = struct{}{}
			}
		}
	}
	for key := range usedKeys {
		delete(missingKeys, key)
	}

	result := &LoadResult{
		Viper:       vip,
		UsedKeys:    sortedKeys(usedKeys),
		MissingKeys: sortedKeys(missingKeys),
		BrokenKeys:  sortedKeys(brokenKeys),
	}

	// Helper commands run against other commands' configs, so unknown keys
	// are expected there and reporting them is just noise.
	if cmd.Annotations["type"] != "helper" && len(result.MissingKeys) > 0 {
		options.logger.Debug(
			"unknown configuration keys",
			"keys", result.MissingKeys,
		)
	}
	if len(result.BrokenKeys) > 0 {
		options.logger.Error(
			"invalid configuration values",
			"keys", result.BrokenKeys,
		)
		if options.failOnValueError {
			return result, fmt.Errorf(
				"config: invalid values for keys: %s",
				strings.Join(result.BrokenKeys, ", "),
			)
		}
	}

	return result, nil
}

// Load reads configuration into every config struct bound to cmd in the
// default registry.
func Load(cmd *cobra.Command, opts ...LoadOption) (*LoadResult, error) {
	return defaultRegistry.Load(cmd, opts...)
}

// lookupFlag resolves the flag a configuration key belongs to, and the name it
// is known by. A list value is reported one element at a time, as "peers.0",
// "peers.1" and so on, so a key that names no flag is retried against its
// parent as long as the trailing segment is an index and the parent is a
// list-valued flag.
func lookupFlag(cmd *cobra.Command, key string) (string, *pflag.Flag) {
	if f := cmd.Flags().Lookup(key); f != nil {
		return key, f
	}
	for {
		dot := strings.LastIndex(key, ".")
		if dot < 0 {
			return key, nil
		}
		parent, index := key[:dot], key[dot+1:]
		if !isIndex(index) {
			return key, nil
		}
		if f := cmd.Flags().Lookup(parent); f != nil {
			if _, isSlice := f.Value.(pflag.SliceValue); isSlice {
				return parent, f
			}
			return key, nil
		}
		key = parent
	}
}

// isIndex reports whether s is a list index, i.e. all digits.
func isIndex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// setFlagFromViper sets a flag from the configuration value at key, reading it
// as the type the flag actually holds. Reading everything as a string would
// silently empty list-valued flags, since viper cannot cast a list to a
// string.
func setFlagFromViper(f *pflag.Flag, vip *viper.Viper, key string) error {
	if sv, ok := f.Value.(pflag.SliceValue); ok {
		return sv.Replace(vip.GetStringSlice(key))
	}
	return f.Value.Set(vip.GetString(key))
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
