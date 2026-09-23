package version_test

import (
	"runtime"
	"testing"
	"time"

	"gitlab.internal/tuan.quang.tran/aioz-common/version"

	"github.com/spacemonkeygo/monkit/v3"
	"github.com/stretchr/testify/require"
)

// collect drains a StatSource into a field -> value map of the version_info
// series. The build_info series emitted alongside it carries identity as tags
// rather than numbers, so it has its own helper (buildInfoTags).
func collect(t *testing.T, src monkit.StatSource) map[string]float64 {
	t.Helper()

	out := map[string]float64{}
	src.Stats(func(key monkit.SeriesKey, field string, val float64) {
		require.Contains(t, []string{"version_info", "build_info"}, key.Measurement)
		if key.Measurement != "version_info" {
			return
		}
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

// buildInfoTags drains a StatSource and returns the tags of the build_info
// series, plus the tag sets seen on version_info.
func buildInfoTags(t *testing.T, src monkit.StatSource) (build map[string]string, versionInfoTagged bool) {
	t.Helper()

	src.Stats(func(key monkit.SeriesKey, field string, val float64) {
		tags := map[string]string{}
		for k, v := range key.Tags.All() {
			tags[k] = v
		}

		switch key.Measurement {
		case "build_info":
			require.Equal(t, "value", field)
			require.Equal(t, float64(1), val, "build_info is a constant 1 series")
			build = tags
		case "version_info":
			if len(tags) > 0 {
				versionInfoTagged = true
			}
		default:
			t.Fatalf("unexpected measurement %q", key.Measurement)
		}
	})
	return build, versionInfoTagged
}

func TestStatsBuildInfoCarriesReadableIdentity(t *testing.T) {
	t.Parallel()

	v, err := version.NewSemVer("v3.14.15")
	require.NoError(t, err)

	info := version.Info{
		Version:    v,
		CommitHash: "7077c772a20dc5581ac53802fec947fa166e1cf6",
		Timestamp:  time.Now(),
	}

	build, versionInfoTagged := buildInfoTags(t, &info)

	require.Equal(t, "7077c77", build["commit"], "commit should be the 7-char short hash")
	require.Equal(t, "v3.14.15", build["version"])
	require.Equal(t, "false", build["modified"])

	// The whole point of a separate series: an upgrade must not mint a new
	// series for each of version_info's eleven numeric fields.
	require.False(t, versionInfoTagged,
		"version_info must stay untagged so its series survive a version change")
}

func TestStatsBuildInfoOmitsWhatItDoesNotKnow(t *testing.T) {
	t.Parallel()

	// A binary built outside a git checkout has neither. An empty tag value is
	// worse than an absent one: it becomes a real label every query must match.
	build, _ := buildInfoTags(t, &version.Info{Timestamp: time.Now()})

	require.NotContains(t, build, "commit")
	require.NotContains(t, build, "version")
	require.Equal(t, "false", build["modified"])
}
