package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"

	"github.com/spf13/cobra"
)

// isRelease reports whether this binary is a release build. Consumers that
// build release artifacts call SetRelease(true) from main before binding, so
// that DefaultsType returns "release" and `releaseDefault` tags win.
var isRelease atomic.Bool

// SetRelease records whether this binary is a release build. It affects which
// set of default tags DefaultsType selects, and must be called before
// DefaultsFlag or BindFlags. The `--defaults` flag still takes precedence.
func SetRelease(release bool) {
	isRelease.Store(release)
}

// DefaultsType returns the type of defaults (release/dev/test) this binary
// should use. An explicit `--defaults` argument wins; otherwise it follows
// SetRelease.
func DefaultsType() string {
	defaults := strings.ToLower(FindFlagEarly("defaults"))
	if defaults != "" {
		return defaults
	}
	if isRelease.Load() {
		return "release"
	}
	return "dev"
}

// DefaultsFlag sets up the defaults=dev/release/test flag option, which is
// needed before flag parsing has happened, and returns the matching
// BindOption.
func DefaultsFlag(cmd *cobra.Command) BindOption {
	// define a flag so that the flag parsing system will be happy.
	defaults := DefaultsType()

	// we're actually going to ignore this flag entirely and parse the
	// command line arguments early instead
	_ = cmd.PersistentFlags().String("defaults", defaults,
		"determines which set of configuration defaults to use. can either be 'dev' or 'release'")
	setSourceAnnotation(cmd.PersistentFlags(), "defaults", FlagSource)

	switch defaults {
	case "dev":
		return UseDevDefaults()
	case "release":
		return UseReleaseDefaults()
	case "test":
		return UseTestDefaults()
	default:
		panic(fmt.Sprintf("unsupported defaults value %q", defaults))
	}
}

// SetupFlag sets up a flag that is needed before flag parsing has happened,
// such as --config-dir, whose value other defaults are expanded against.
// A nil logger falls back to slog.Default.
func SetupFlag(
	log *slog.Logger,
	cmd *cobra.Command,
	dest *string,
	name, value, usage string,
) {
	if log == nil {
		log = slog.Default()
	}
	if foundValue := FindFlagEarly(name); foundValue != "" {
		value = foundValue
	}
	cmd.PersistentFlags().StringVar(dest, name, value, usage)
	if err := cmd.PersistentFlags().
		SetAnnotation(name, "setup", []string{"true"}); err != nil {
		log.Error("failed to set 'setup' annotation", "flag", name)
	}
}

// FindFlagEarly retrieves the value of a flag from os.Args before flag parsing
// has happened. Under `go test` os.Args holds the test binary's own flags, so
// tests should call FindFlagIn directly.
func FindFlagEarly(flagName string) string {
	return FindFlagIn(os.Args, flagName)
}

// FindFlagIn retrieves the value of a flag from args, supporting both
// --name=value and --name value forms.
func FindFlagIn(args []string, flagName string) string {
	for i, arg := range args {
		if after, ok := strings.CutPrefix(
			arg,
			fmt.Sprintf("--%s=", flagName),
		); ok {
			return after
		} else if arg == "--"+flagName && i < len(args)-1 {
			return args[i+1]
		}
	}
	return ""
}
