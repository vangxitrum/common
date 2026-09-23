package version

import (
	"hash/crc32"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/spacemonkeygo/monkit/v3"
)

// registerMu and registered guard Register per registry, so that a service
// wiring itself up twice does not report the build stamp twice. This mirrors
// stats/monitor.Register, which solves the same problem for the process and
// runtime series.
var (
	registerMu sync.Mutex
	registered = map[*monkit.Registry]bool{}
)

// Register attaches the build stamp of the running binary to registry, so
// /metrics carries a version_info series. A nil registry means monkit.Default.
//
// It exists so a consumer does not have to know that a StatSource is attached
// through a Scope rather than to the registry itself:
//
//	version.Register(nil)
//
// Calling it more than once for the same registry is a no-op.
func Register(registry *monkit.Registry) {
	if registry == nil {
		registry = monkit.Default
	}

	registerMu.Lock()
	defer registerMu.Unlock()

	if registered[registry] {
		return
	}
	registered[registry] = true

	registry.ScopeNamed("").Chain(&Build)
}

// Stats implements monkit.StatSource, so the build stamp appears in /metrics
// alongside everything else.
//
// It answers the question a dashboard actually asks during a rollout: which
// build is each instance running, and how far behind is it. Prometheus stores
// float64 and nothing else, so each field is encoded to survive that:
//
//   - version becomes three numeric fields, major/minor/patch, which can be
//     compared and graphed. The string could only have been a label.
//   - the commit hash becomes its CRC32. The value is meaningless on its own,
//     but it changes exactly when the commit changes, so "how many distinct
//     builds are live right now" is a count of distinct values.
//   - age_sec is how long ago the commit was made, which is the number that
//     tells you an instance was left behind, without anyone having to know
//     what the current version is.
//   - os and arch are emitted as a field named for their value, set to 1, so
//     a query can group by platform.
//
// Call [Register] once at startup to attach it.
func (v *Info) Stats(cb func(key monkit.SeriesKey, field string, val float64)) {
	key := monkit.NewSeriesKey("version_info")

	// Identity rides on its own constant series rather than as tags on the
	// fields below. A field's VALUE changing is free - it stays the same
	// series - but a LABEL changing mints a new one, so tagging all eleven
	// fields would turn every upgrade into eleven new series per process. On a
	// fleet of independently-operated nodes that is the difference between
	// negligible and a real cost, which is why Storj encodes the commit as a
	// CRC32 value and never as a label.
	//
	// One series per process per build is the Prometheus build_info
	// convention, and depin already settled on it independently
	// (edgeserver.recordBuildInfo). The CRC32 field below stays: it answers
	// "did the build change" without minting anything at all, which is what
	// counting distinct live builds needs.
	buildKey := monkit.NewSeriesKey("build_info")
	if v.CommitHash != "" {
		buildKey = buildKey.WithTag("commit", shortCommit(v.CommitHash))
	}
	if ver := v.Version.String(); ver != "" {
		buildKey = buildKey.WithTag("version", ver)
	}
	buildKey = buildKey.WithTag("modified", strconv.FormatBool(v.Modified))
	cb(buildKey, "value", 1)

	cb(key, "release", boolAsFloat(v.Release))
	cb(key, "modified", boolAsFloat(v.Modified))
	cb(key, "dev", boolAsFloat(v.Version.IsDev()))

	if !v.Timestamp.IsZero() {
		cb(key, "timestamp", float64(v.Timestamp.Unix()))
		cb(key, "age_sec", time.Since(v.Timestamp).Seconds())
	}

	if v.CommitHash != "" {
		// Prometheus stores float64, so the hash is encoded as a CRC32. The
		// number is meaningless on its own but changes exactly when the commit
		// does, which is what makes "how many distinct builds are live" a
		// count of distinct values.
		//
		// Storj's version of this memoizes the CRC on the Info. That is not
		// worth a cache line: this is 40 bytes of CRC32 once per scrape, and a
		// cache that is not keyed by the hash silently returns the wrong value
		// for a second Info.
		cb(key, "commit", float64(crc32.ChecksumIEEE([]byte(v.CommitHash))))
	}

	cb(key, "major", float64(v.Version.Major))
	cb(key, "minor", float64(v.Version.Minor))
	cb(key, "patch", float64(v.Version.Patch))

	cb(key, "os_"+runtime.GOOS, 1)
	cb(key, "arch_"+runtime.GOARCH, 1)
}

func boolAsFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
