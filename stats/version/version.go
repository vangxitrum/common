// Package version exposes the build stamp of the running binary.
//
// The stamp is read from the information the Go toolchain embeds at build
// time, so a service does not have to wire anything into its Makefile: since
// Go 1.18 `go build` records the commit, the commit timestamp and whether the
// working tree was dirty, and debug.ReadBuildInfo reads them back. Link-time
// variables remain as a fallback for builds where that information is absent.
package version

import (
	"encoding/json"
	"fmt"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// Info is the build stamp. It is what the debug server serves on /version/.
type Info struct {
	Version    string    `json:"version"`
	CommitHash string    `json:"commitHash,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
	Release    bool      `json:"release,omitempty"`
	Modified   bool      `json:"modified,omitempty"`
}

// Build is the stamp of the running binary, filled in at startup.
var Build Info

func init() { Build = fromBuildInfo() }

// These are patched at link time by -ldflags -X and are only consulted when
// the Go build information does not already carry the field. They must stay
// plain string variables: the linker's -X can only patch a symbol whose type
// is exactly string, so -X aimed at a struct field such as Build.Version names
// a symbol that does not exist and is silently dropped.
var (
	buildVersion    string // semantic version, e.g. v1.2.3
	buildCommitHash string
	buildTimestamp  string // unix seconds since epoch
	buildRelease    string // "true" or "false"

	// channel selects which set of config defaults applies: the devDefault
	// tags or the releaseDefault ones. Unlike Release it is declared rather
	// than derived, because it is a deliberate choice about how the binary
	// should behave and not an observation about how it was built. `make
	// build` leaves it at dev; a Dockerfile or a release job sets it.
	channel = channelDev
)

const (
	channelDev     = "dev"
	channelRelease = "release"
)

// IsRelease reports whether this binary should use release config defaults. It
// drives config.SetRelease, which selects the releaseDefault struct tags.
//
// This is the defaults channel, not Info.Release: a release-channel build of a
// dirty tree is still not a release build.
func IsRelease() bool { return channel == channelRelease }

// Channel returns the defaults channel this binary was built for.
func Channel() string { return channel }

// Marshal returns the JSON encoding of v.
func (v Info) Marshal() ([]byte, error) {
	return json.Marshal(v)
}

// String renders the stamp for a version subcommand.
func (v Info) String() string {
	out := "release: " + strconv.FormatBool(v.Release) + "\n"
	if v.Version != "" {
		out += "version: " + v.Version + "\n"
	}
	if v.CommitHash != "" {
		out += "commit:  " + v.CommitHash + "\n"
	}
	if !v.Timestamp.IsZero() {
		out += "built:   " + v.Timestamp.Format(time.RFC3339) + "\n"
	}
	if v.Modified {
		out += "modified: true\n"
	}
	return out + fmt.Sprintf(
		"channel: %s\ngo:      %s\n",
		channel,
		runtime.Version(),
	)
}

// fromBuildInfo assembles the stamp from the Go build information, falling
// back to the link-time variables for anything it does not carry.
//
// Release is derived rather than declared: a build counts as a release only if
// it is provably a clean, tagged one - a real version, an unmodified tree, and
// a recorded commit and timestamp. A binary built from a dirty checkout says
// so instead of claiming to be a release. buildRelease overrides this, and is
// the only link-time variable that takes precedence rather than filling a gap.
func fromBuildInfo() Info {
	var info Info

	if bi, ok := debug.ReadBuildInfo(); ok {
		// bi.Main.Version is only meaningful for a binary installed with
		// `go install module@version`. For a local build Go synthesizes a
		// pseudo-version, which says nothing, so it is discarded.
		if v := bi.Main.Version; v != "" && v != "(devel)" &&
			!isPseudoVersion(v) {
			info.Version = v
		}

		for _, setting := range bi.Settings {
			switch setting.Key {
			case "vcs.revision":
				info.CommitHash = setting.Value
			case "vcs.time":
				if t, err := time.Parse(
					time.RFC3339,
					setting.Value,
				); err == nil {
					info.Timestamp = t
				}
			case "vcs.modified":
				info.Modified, _ = strconv.ParseBool(setting.Value)
			}
		}
	}

	// The Go toolchain omits the VCS block for a build made outside a
	// repository - a container build from a context with no .git, or -buildvcs
	// =false - and always inside `go test`. The link-time variables cover
	// those cases.
	if info.Version == "" {
		info.Version = buildVersion
	}
	if info.CommitHash == "" {
		info.CommitHash = buildCommitHash
	}
	if info.Timestamp.IsZero() && buildTimestamp != "" {
		if secs, err := strconv.ParseInt(buildTimestamp, 10, 64); err == nil {
			info.Timestamp = time.Unix(secs, 0).UTC()
		}
	}

	info.Release = info.Version != "" &&
		info.CommitHash != "" &&
		!info.Timestamp.IsZero() &&
		!info.Modified

	if buildRelease != "" {
		if release, err := strconv.ParseBool(buildRelease); err == nil {
			info.Release = release
		}
	}

	return info
}

// isPseudoVersion reports whether v is a version Go made up for an untagged
// commit, such as v0.0.0-20230601144555-f2db46069602. Such a version records
// that no version was set, so it is treated as none.
//
// This is a deliberately small check rather than a dependency on
// golang.org/x/mod. Every pseudo-version ends in a 14-digit UTC timestamp and
// a 12-character commit prefix; the separator before the timestamp is a dash
// in the vX.0.0-<ts>-<commit> form and a dot in the two forms that build on an
// existing tag, such as v1.0.3-0.<ts>-<commit>.
func isPseudoVersion(v string) bool {
	const tsLen, hashLen = 14, 12

	// Go 1.24 and later append the +dirty build metadata suffix to the
	// synthesized version when the working tree is modified. Semver build
	// metadata is not part of the version, and leaving it on would end-anchor
	// the checks below on "dirty" rather than on the commit prefix, so a
	// pseudo-version from a dirty tree would be mistaken for a real one.
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}

	// <sep> <14 digits> '-' <12 hex>
	if len(v) < 1+tsLen+1+hashLen {
		return false
	}
	if !isHex(v[len(v)-hashLen:]) || v[len(v)-hashLen-1] != '-' {
		return false
	}
	if !isDigits(v[len(v)-hashLen-1-tsLen : len(v)-hashLen-1]) {
		return false
	}
	sep := v[len(v)-hashLen-1-tsLen-1]
	return sep == '-' || sep == '.'
}

func isDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isHex(s string) bool {
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
