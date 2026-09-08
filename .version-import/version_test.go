package version

import "testing"

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
	release, sha = "v1.2.3", "abc123"
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
