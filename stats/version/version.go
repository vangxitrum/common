// Package version exposes the build stamp linked into the binary.
//
// It is both the stamp a service sets at link time and the value the debug
// server serves on /version/. Keeping the two in one package means there is a
// single set of symbols for -ldflags to patch; a service does not declare its
// own string variables and copy them across.
package version

import (
	"encoding/json"
	"fmt"
	"runtime"
)

// Info holds build-time version information.
type Info struct {
	Release string `json:"release"`
	Build   string `json:"build"`
	SHA     string `json:"sha"`
}

// Marshal returns the JSON encoding of v.
func (v Info) Marshal() ([]byte, error) {
	return json.Marshal(v)
}

// Build is what the debug server's /version/ endpoint serves. Register fills
// it from the linked stamp; a test or an embedder can also set it directly.
var Build = Info{}

// These are patched at link time by -ldflags -X; see LDFLAGS in the service's
// Makefile. The symbols to patch are:
//
//	gitlab.internal/tuan.quang.tran/aioz-common/stats/version.release
//	gitlab.internal/tuan.quang.tran/aioz-common/stats/version.build
//	gitlab.internal/tuan.quang.tran/aioz-common/stats/version.sha
//	gitlab.internal/tuan.quang.tran/aioz-common/stats/version.channel
//
// They must stay plain string variables. The linker's -X can only patch a
// symbol whose type is exactly string, so -X on a struct field such as
// version.Build.Release names a symbol that does not exist and is silently
// dropped. Register copies these into Build at runtime, where it does work.
var (
	release = "dev"
	build   = "unknown"
	sha     = "unknown"

	// channel selects which set of config defaults applies: the devDefault
	// tags or the releaseDefault ones.
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

// Current returns the build stamp linked into this binary.
func Current() Info {
	return Info{Release: release, Build: build, SHA: sha}
}

// IsRelease reports whether this binary should use release defaults. It drives
// config.SetRelease, which selects the releaseDefault struct tags.
func IsRelease() bool { return channel == channelRelease }

// Channel returns the defaults channel this binary was built for.
func Channel() string { return channel }

// Register publishes the linked stamp to Build so the debug server's /version/
// endpoint serves it. Call once from main, before any command runs.
func Register() { Build = Current() }

// String renders the stamp for a version subcommand.
func String() string {
	return fmt.Sprintf(
		"release: %s\nbuild:   %s\nsha:     %s\nchannel: %s\ngo:      %s\n",
		release,
		build,
		sha,
		channel,
		runtime.Version(),
	)
}
