package config

import (
	"bytes"
	"flag"
	"fmt"
	"sort"

	"github.com/spf13/cast"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"
)

// saveFilePerm is the permission the generated config file is written with.
// It may hold secrets, so it stays owner-only.
const saveFilePerm = 0o600

// saveIndent is the YAML indent used for the generated config file. yaml.v3
// defaults to 4; 2 is kept so generated files match what already exists in
// the fleet.
const saveIndent = 2

// SaveOption updates the options for SaveConfig.
type SaveOption func(*SaveOptions)

// SaveConfigOption is a deprecated alias for SaveOption.
//
// Deprecated: use SaveOption.
type SaveConfigOption = SaveOption

// SaveOptions controls the behavior of SaveConfig.
type SaveOptions struct {
	Overrides        map[string]any
	RemoveDeprecated bool
}

// SaveConfigWithOverrides sets the overrides to the provided map.
func SaveConfigWithOverrides(overrides map[string]any) SaveOption {
	return func(opts *SaveOptions) {
		opts.Overrides = overrides
	}
}

// SaveConfigWithOverride adds a single override to SaveConfig.
func SaveConfigWithOverride(name string, value any) SaveOption {
	return func(opts *SaveOptions) {
		if opts.Overrides == nil {
			opts.Overrides = make(map[string]any)
		}
		opts.Overrides[name] = value
	}
}

// SaveConfigRemovingDeprecated tells SaveConfig to not store deprecated flags.
func SaveConfigRemovingDeprecated() SaveOption {
	return func(opts *SaveOptions) {
		opts.RemoveDeprecated = true
	}
}

// SaveConfig writes the configuration bound to cmd to outfile. Values still at
// their default are written commented out, so that the file documents every
// available setting without pinning it; values that were explicitly set, that
// came from an existing config file, that are overridden here, or that are
// tagged `user:"true"`, are written live.
func SaveConfig(
	cmd *cobra.Command,
	outfile string,
	opts ...SaveOption,
) error {
	return defaultRegistry.SaveConfig(cmd, outfile, opts...)
}

// SaveConfig writes the configuration bound to cmd in this registry to
// outfile. See the package-level SaveConfig.
func (r *Registry) SaveConfig(
	cmd *cobra.Command,
	outfile string,
	opts ...SaveOption,
) error {
	// step 0. apply any options to change the behavior
	//

	var options SaveOptions
	for _, opt := range opts {
		opt(&options)
	}

	// step 1. load all of the configuration settings we are going to save
	//

	flags := cmd.Flags()
	vip, err := r.Viper(cmd)
	if err != nil {
		return err
	}
	if err := vip.MergeConfigMap(options.Overrides); err != nil {
		return fmt.Errorf("merge overrides: %w", err)
	}
	settings := vip.AllSettings()

	// step 2. construct some data describing what exactly we're saving to the
	// config file, and how they're saved.
	//

	flat, flatKeys := flattenSettings(flags, settings, &options)
	sort.Strings(flatKeys)

	// step 3. write out the configuration file
	//

	data, err := renderConfig(flatKeys, flat)
	if err != nil {
		return err
	}

	return AtomicWriteFile(outfile, data, saveFilePerm)
}

// configValue is one setting as it will appear in the generated file.
type configValue struct {
	value   any
	comment string
	set     bool
}

// flattenSettings turns viper's nested settings into the flat, dotted keys the
// config file uses, dropping the ones that must not be persisted.
func flattenSettings(
	flags *pflag.FlagSet,
	settings map[string]any,
	options *SaveOptions,
) (map[string]configValue, []string) {
	flat := make(map[string]configValue, len(settings))
	flatKeys := make([]string, 0, len(settings))

	// N.B. we have to pre-declare the function so that it can make recursive
	// calls.
	var filterAndFlatten func(string, map[string]any)
	filterAndFlatten = func(base string, settings map[string]any) {
		for key, value := range settings {
			if value, ok := value.(map[string]any); ok {
				filterAndFlatten(base+key+".", value)
				continue
			}
			fullKey := base + key

			// the help key should not be persisted to the config file but
			// cannot use the FlagSource source annotation since it is a
			// standard flag and not a pflag.
			if fullKey == "help" {
				continue
			}

			// gather information about the flag under consideration
			var (
				changed    bool
				setup      bool
				hidden     bool
				user       bool
				deprecated bool
				source     string
				comment    string
				typ        string

				_, overrideExists = options.Overrides[fullKey]
			)
			if f := flags.Lookup(fullKey); f != nil { // first check pflags
				// When a value is loaded from the file, the flag won't be
				// "changed" but we still need to persist it. Therefore, for
				// the following code, "changed" means that either a flag has
				// been explicitly set or a value that differs from the
				// default.
				changed = f.Changed || f.Value.String() != f.DefValue
				setup = readBoolAnnotation(f, "setup")
				hidden = readBoolAnnotation(f, "hidden")
				user = readBoolAnnotation(f, "user")
				deprecated = readBoolAnnotation(f, "deprecated")
				source = readSourceAnnotation(f)
				comment = f.Usage
				typ = f.Value.Type()
			} else if f := flag.Lookup(fullKey); f != nil { // then stdlib flags
				changed = f.Value.String() != f.DefValue
				comment = f.Usage
			} else {
				// by default we store config values we know nothing about. we
				// abuse the meaning of "changed" to include this case.
				changed = true
			}

			// in any of these cases, don't store the key in the config file
			if setup ||
				hidden ||
				options.RemoveDeprecated && deprecated ||
				source == FlagSource {
				continue
			}

			// viper is super cool and doesn't cast floats automatically, so we
			// handle that ourselves.
			if typ == "float64" {
				value = cast.ToFloat64(value)
			}

			flatKeys = append(flatKeys, fullKey)
			flat[fullKey] = configValue{
				value:   value,
				comment: comment,
				set:     user || changed || overrideExists,
			}
		}
	}
	filterAndFlatten("", settings)

	return flat, flatKeys
}

// renderConfig serializes the flattened settings, commenting out every value
// that is not set.
func renderConfig(
	flatKeys []string,
	flat map[string]configValue,
) ([]byte, error) {
	nl := []byte("\n")
	// one help comment, at least one value line and one blank line per key
	lines := make([][]byte, 0, len(flatKeys)*3)
	for _, key := range flatKeys {
		config := flat[key]

		if config.comment != "" {
			lines = append(lines, []byte("# "+config.comment))
		}

		data, err := marshalSetting(key, config.value)
		if err != nil {
			return nil, err
		}
		dataLines := bytes.Split(bytes.TrimSpace(data), nl)

		// if the config value is set, concat in the yaml lines
		if config.set {
			lines = append(lines, dataLines...)
		} else {
			// otherwise, add them in but commented out
			for _, line := range dataLines {
				lines = append(lines, append([]byte("# "), line...))
			}
		}

		// add a blank line separator
		lines = append(lines, nil)
	}

	return bytes.Join(lines, nl), nil
}

// marshalSetting serializes one key/value pair on its own.
func marshalSetting(key string, value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(saveIndent)
	if err := enc.Encode(map[string]any{key: value}); err != nil {
		return nil, fmt.Errorf("marshal %s: %w", key, err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("marshal %s: %w", key, err)
	}
	return buf.Bytes(), nil
}

// readSourceAnnotation returns the source annotation, or AnySource if unset.
func readSourceAnnotation(flag *pflag.Flag) string {
	annotation := flag.Annotations["source"]
	if len(annotation) == 0 {
		return AnySource
	}
	return annotation[0]
}

// readBoolAnnotation reports whether a boolean annotation is set to true on
// the flag.
func readBoolAnnotation(flag *pflag.Flag, key string) bool {
	annotation := flag.Annotations[key]
	return len(annotation) > 0 && annotation[0] == "true"
}
