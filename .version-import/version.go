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

	// channel selects which set of aioz-config defaults applies: the
	// devDefault tags or the releaseDefault ones.
	//
	// It is a separate variable from release on purpose. Deriving it from the
	// version stamp - "anything that is not dev is a release" - looks
	// reasonable and is wrong, because `git describe --tags --always --dirty`
	// stamps every local build with a sha. Every build was then a release
	// build, and no devDefault value was ever reachable.
	channel = channelDev
)

const (
	channelDev     = "dev"
	channelRelease = "release"
)

// Info returns the build stamp.
func Info() statsversion.Info {
	return statsversion.Info{Release: release, Build: build, SHA: sha}
}

// IsRelease reports whether this binary should use release defaults. It drives
// config.SetRelease, which selects the releaseDefault struct tags.
func IsRelease() bool { return channel == channelRelease }

// Channel returns the defaults channel this binary was built for.
func Channel() string { return channel }

// Register publishes the stamp to aioz-stats so the debug server's /version/
// endpoint serves it. Call once from main, before any command runs.
func Register() { statsversion.Build = Info() }

// String renders the stamp for the version subcommand.
func String() string {
	return fmt.Sprintf("release: %s\nbuild:   %s\nsha:     %s\nchannel: %s\ngo:      %s\n",
		release, build, sha, channel, runtime.Version())
}
