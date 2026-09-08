package monitor_test

import (
	"testing"

	"github.com/spacemonkeygo/monkit/v3"
	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/stats/monitor"
)

// collect walks a registry the way the debug server's /metrics endpoint does,
// counting how many times each series name is reported.
func collect(registry *monkit.Registry) map[string]int {
	counts := map[string]int{}
	registry.Stats(func(key monkit.SeriesKey, field string, _ float64) {
		counts[key.Measurement+"."+field]++
	})
	return counts
}

// TestRegisterAddsProcessStats is the reason this package exists: before it,
// a freshly started service served an empty /metrics, because monkit only
// knows the series something has already incremented.
func TestRegisterAddsProcessStats(t *testing.T) {
	registry := monkit.NewRegistry()
	require.Empty(t, collect(registry), "a fresh registry reports nothing")

	monitor.Register(registry)

	stats := collect(registry)
	require.NotEmpty(t, stats)

	// The names monkit's environment package actually uses. They are not
	// prefixed "environment_", which is easy to assume and wrong.
	require.Contains(t, stats, "goroutines.count")
	for _, measurement := range []string{"runtime_memstats", "process", "rusage"} {
		require.True(t, hasMeasurement(stats, measurement),
			"no %s series in %v", measurement, keys(stats))
	}
}

// TestRegisterIsIdempotent guards the double-counting bug: environment.Register
// chains its sources onto the registry on every call, so registering twice
// would report every process series twice.
func TestRegisterIsIdempotent(t *testing.T) {
	registry := monkit.NewRegistry()

	monitor.Register(registry)
	once := collect(registry)

	monitor.Register(registry)
	monitor.Register(registry)
	thrice := collect(registry)

	require.Equal(t, once, thrice, "process stats were registered more than once")
	for name, count := range thrice {
		require.Equal(t, 1, count, "%s is reported %d times", name, count)
	}
}

// TestRegisterIsPerRegistry: the guard is keyed by registry, so a test's own
// registry still gets the stats even after production's has been registered.
func TestRegisterIsPerRegistry(t *testing.T) {
	first, second := monkit.NewRegistry(), monkit.NewRegistry()

	monitor.Register(first)
	monitor.Register(second)

	require.NotEmpty(t, collect(second))
	require.Equal(t, collect(first), collect(second))
}

func hasMeasurement(stats map[string]int, measurement string) bool {
	for name := range stats {
		if len(name) > len(measurement) && name[:len(measurement)+1] == measurement+"." {
			return true
		}
	}
	return false
}

func keys(stats map[string]int) []string {
	out := make([]string, 0, len(stats))
	for name := range stats {
		out = append(out, name)
	}
	return out
}
