// Package version exposes the build stamp linked into the binary.
package version

import (
	"fmt"
	"runtime"

	statsversion "gitlab.internal/tuan.quang.tran/aioz-stats/version"
)

// These are patched at link time by -ldflags -X; see LDFLAGS in the Makefile.
//
// They must stay plain string variables. The linker's -X can only patch a
// symbol whose type is exactly string, so -X on a struct field such as
// aioz-stats/version.Build.Release names a symbol that does not exist and is
// silently dropped. Register copies these into that struct at runtime, where
// it does work.
var (
	release = "dev"
	build   = "unknown"
	sha     = "unknown"
)

// Info returns the build stamp.
func Info() statsversion.Info {
	return statsversion.Info{Release: release, Build: build, SHA: sha}
}

// IsRelease reports whether this binary was stamped as a release build. It
// drives config.SetRelease, which selects the releaseDefault struct tags.
func IsRelease() bool { return release != "dev" }

// Register publishes the stamp to aioz-stats so the debug server's /version/
// endpoint serves it. Call once from main, before any command runs.
func Register() { statsversion.Build = Info() }

// String renders the stamp for the version subcommand.
func String() string {
	return fmt.Sprintf("release: %s\nbuild:   %s\nsha:     %s\ngo:      %s\n",
		release, build, sha, runtime.Version())
}
