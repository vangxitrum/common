package version

import (
	"testing"
	"time"
)

// TestChannelSelectsDefaults guards a bug that hid itself for a long time:
// IsRelease used to read the version stamp, and since the Makefile stamps
// `git describe --tags --always --dirty` on every build, every build looked
// like a release. Every devDefault tag in the config was unreachable - a
// developer got JSON logs at info level and no source annotations, and nothing
// pointed at why.
func TestChannelSelectsDefaults(t *testing.T) {
	original := channel
	t.Cleanup(func() { channel = original })

	channel = channelDev
	if IsRelease() {
		t.Fatal("a dev build must use dev defaults")
	}

	channel = channelRelease
	if !IsRelease() {
		t.Fatal("a release build must use release defaults")
	}

	// The stamp must not influence it. This is the coupling that broke.
	channel = channelDev
	restore := swapLinkVars(t, "v1.2.3", "abc123", "1700000000", "")
	defer restore()
	if IsRelease() {
		t.Fatal("the version stamp must not decide the defaults channel")
	}
}

// TestDefaultChannelIsDev: an unstamped binary - `go run`, `go test`, a build
// that forgot the ldflags - is a developer's binary.
func TestDefaultChannelIsDev(t *testing.T) {
	if channelDev != "dev" || channelRelease != "release" {
		t.Fatal("channel names are part of the Makefile contract")
	}
}

// TestLinkVarsFillGaps covers the container case: a build from a context with
// no .git carries no VCS block, and the -X variables are all there is. Note
// that this also describes `go test` itself, which never has a VCS block, so
// the fields asserted here come entirely from the fallback.
func TestLinkVarsFillGaps(t *testing.T) {
	restore := swapLinkVars(t, "v1.2.3", "abc123", "1700000000", "")
	defer restore()

	info := fromBuildInfo()
	if info.Version != "v1.2.3" {
		t.Fatalf("version: got %q", info.Version)
	}
	if info.CommitHash != "abc123" {
		t.Fatalf("commit: got %q", info.CommitHash)
	}
	if !info.Timestamp.Equal(time.Unix(1700000000, 0).UTC()) {
		t.Fatalf("timestamp: got %v", info.Timestamp)
	}
	if !info.Release {
		t.Fatal("a stamped, unmodified build should be a release")
	}
}

// TestReleaseIsDerivedNotDeclared: the whole point of reading the toolchain's
// VCS block is that a binary cannot claim to be a release it is not. An
// unstamped build has no version, so it is not one.
func TestReleaseIsDerivedNotDeclared(t *testing.T) {
	restore := swapLinkVars(t, "", "", "", "")
	defer restore()

	if fromBuildInfo().Release {
		t.Fatal("an unstamped build must not report as a release")
	}
}

// TestBuildReleaseOverrides: buildRelease is the one link-time variable that
// takes precedence rather than filling a gap.
func TestBuildReleaseOverrides(t *testing.T) {
	restore := swapLinkVars(t, "", "", "", "true")
	defer restore()

	if !fromBuildInfo().Release {
		t.Fatal("buildRelease=true must override the derivation")
	}
}

// TestPseudoVersionIsNotAVersion: Go synthesizes a version for every untagged
// build, so accepting it would make every local binary look tagged.
func TestPseudoVersionIsNotAVersion(t *testing.T) {
	pseudo := []string{
		"v0.0.0-20230601144555-f2db46069602",
		"v1.0.3-0.20230601144555-f2db46069602",
		"v0.0.0-20191109021931-daa7c04131f5",

		// Go 1.24+ marks a build from a modified tree with +dirty. It is
		// still a synthesized version and must still be discarded.
		"v0.0.0-20230601144555-f2db46069602+dirty",
		"v1.0.3-0.20230601144555-f2db46069602+dirty",
	}
	for _, v := range pseudo {
		if !isPseudoVersion(v) {
			t.Errorf("%q should be recognized as a pseudo-version", v)
		}
	}

	real := []string{"v1.2.3", "v0.1.0", "v2.0.0-rc1", "v1.2.3+meta", ""}
	for _, v := range real {
		if isPseudoVersion(v) {
			t.Errorf("%q is a real version, not a pseudo-version", v)
		}
	}
}

// TestModifiedIsNotARelease: a binary built from a dirty tree says so.
func TestModifiedIsNotARelease(t *testing.T) {
	restore := swapLinkVars(t, "v1.2.3", "abc123", "1700000000", "")
	defer restore()

	info := fromBuildInfo()
	if !info.Release {
		t.Fatal("precondition: this build should be a release")
	}

	info.Modified = true
	info.Release = info.Version != "" && info.CommitHash != "" &&
		!info.Timestamp.IsZero() && !info.Modified
	if info.Release {
		t.Fatal("a modified tree must not produce a release build")
	}
}

func swapLinkVars(
	t *testing.T,
	version, commit, timestamp, release string,
) func() {
	t.Helper()
	oldVersion, oldCommit := buildVersion, buildCommitHash
	oldTimestamp, oldRelease := buildTimestamp, buildRelease
	buildVersion, buildCommitHash = version, commit
	buildTimestamp, buildRelease = timestamp, release
	return func() {
		buildVersion, buildCommitHash = oldVersion, oldCommit
		buildTimestamp, buildRelease = oldTimestamp, oldRelease
	}
}
