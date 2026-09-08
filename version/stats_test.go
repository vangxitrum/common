package version_test

import (
	"runtime"
	"testing"
	"time"

	"gitlab.internal/tuan.quang.tran/aioz-common/version"

	"github.com/spacemonkeygo/monkit/v3"
	"github.com/stretchr/testify/require"
)

// collect drains a StatSource into a field -> value map.
func collect(t *testing.T, src monkit.StatSource) map[string]float64 {
	t.Helper()

	out := map[string]float64{}
	src.Stats(func(key monkit.SeriesKey, field string, val float64) {
		require.Equal(t, "version_info", key.Measurement)
		out[field] = val
	})
	return out
}

func TestInfoStats(t *testing.T) {
	t.Parallel()

	v, err := version.NewSemVer("v3.14.15")
	require.NoError(t, err)

	info := version.Info{
		Version:    v,
		CommitHash: "abc1234def",
		Timestamp:  time.Now().Add(-2 * time.Hour),
		Release:    true,
	}

	got := collect(t, &info)

	require.Equal(t, float64(3), got["major"])
	require.Equal(t, float64(14), got["minor"])
	require.Equal(t, float64(15), got["patch"])
	require.Equal(t, float64(1), got["release"])
	require.Equal(t, float64(0), got["modified"])
	require.Equal(t, float64(0), got["dev"])

	require.InDelta(t, (2 * time.Hour).Seconds(), got["age_sec"], 60,
		"age_sec is what shows an instance was left behind")
	require.Equal(t, float64(info.Timestamp.Unix()), got["timestamp"])

	require.NotZero(t, got["commit"], "the hash is encoded as a CRC so it fits a float")

	require.Equal(t, float64(1), got["os_"+runtime.GOOS])
	require.Equal(t, float64(1), got["arch_"+runtime.GOARCH])
}

// The commit field has to change when the commit changes and only then, since
// "how many distinct builds are live" is a count of distinct values.
func TestStatsCommitIsStableAndDistinct(t *testing.T) {
	t.Parallel()

	a := version.Info{CommitHash: "aaaaaaa"}
	b := version.Info{CommitHash: "bbbbbbb"}

	first := collect(t, &a)["commit"]
	second := collect(t, &a)["commit"]
	require.Equal(t, first, second, "the same commit must give the same value")

	require.NotEqual(t, first, collect(t, &b)["commit"])
}

func TestStatsOmitsWhatItDoesNotKnow(t *testing.T) {
	t.Parallel()

	got := collect(t, &version.Info{})

	require.NotContains(t, got, "commit", "no commit hash, no commit field")
	require.NotContains(t, got, "timestamp")
	require.NotContains(t, got, "age_sec")

	// The always-present fields are still there, so a series exists at all.
	require.Contains(t, got, "major")
	require.Contains(t, got, "release")
}

func TestStatsMarksADerivedBuild(t *testing.T) {
	t.Parallel()

	info := version.Info{Version: version.DevVersion("abc1234", time.Now())}
	require.Equal(t, float64(1), collect(t, &info)["dev"])
}

// The point of implementing monkit.StatSource is that Build drops into a
// registry with no adapter.
func TestBuildRegistersWithMonkit(t *testing.T) {
	t.Parallel()

	registry := monkit.NewRegistry()
	version.Register(registry)
	version.Register(registry) // must be idempotent

	found := map[string]bool{}
	registry.Stats(func(key monkit.SeriesKey, field string, _ float64) {
		if key.Measurement == "version_info" {
			found[field] = true
		}
	})

	require.True(t, found["major"], "version_info should reach the registry")
	require.True(t, found["release"])
}
