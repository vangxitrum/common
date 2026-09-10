package version_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"gitlab.internal/tuan.quang.tran/aioz-common/version"

	"github.com/stretchr/testify/require"
)

// Parsing is tolerant because internal services write their version in
// whatever shape their Makefile, CI variable or install path produced.
func TestNewSemVerAcceptsWhatInternalBuildsProduce(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		"v1.2.3":       "v1.2.3",
		"1.2.3":        "v1.2.3",
		"v1.2":         "v1.2.0",
		"v1":           "v1.0.0",
		"v1.2.3-rc1":   "v1.2.3-rc1",
		"v1.2.3+dirty": "v1.2.3+dirty",
		"v0.0.7":       "v0.0.7",
	} {
		t.Run(in, func(t *testing.T) {
			got, err := version.NewSemVer(in)
			require.NoError(t, err)
			require.Equal(t, want, got.String())
		})
	}
}

func TestNewSemVerRejectsNonsense(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"", "not-a-version", "v1.2.3.4"} {
		_, err := version.NewSemVer(in)
		require.Error(t, err, "input %q", in)
	}
}

func TestSemVerCompare(t *testing.T) {
	t.Parallel()

	mustParse := func(s string) version.SemVer {
		v, err := version.NewSemVer(s)
		require.NoError(t, err)
		return v
	}

	require.Negative(t, mustParse("v1.2.3").Compare(mustParse("v1.2.4")))
	require.Zero(t, mustParse("v1.2.3").Compare(mustParse("1.2.3")))
	require.Positive(t, mustParse("v2.0.0").Compare(mustParse("v1.9.9")))
	require.Negative(t, mustParse("v1.2.3-rc1").Compare(mustParse("v1.2.3")),
		"a prerelease sorts below its release")
}

func TestSemVerZeroValue(t *testing.T) {
	t.Parallel()

	var zero version.SemVer
	require.True(t, zero.IsZero())
	require.Empty(t, zero.String(), "no version must not render as v0.0.0")

	b, err := json.Marshal(zero)
	require.NoError(t, err)
	require.JSONEq(t, `""`, string(b))
}

// The embedded blang Version has its own String and MarshalJSON that omit the
// "v". Both must be shadowed, or the printed and marshaled forms of the same
// value disagree.
func TestSemVerRendersConsistently(t *testing.T) {
	t.Parallel()

	v, err := version.NewSemVer("v1.2.3")
	require.NoError(t, err)

	require.Equal(t, "v1.2.3", v.String())
	// %v rather than %s: both go through Stringer, and staticcheck rightly
	// flags Sprintf("%s") on a Stringer as a roundabout String() call.
	require.Equal(t, "v1.2.3", fmt.Sprintf("%v", v), "value receiver, so fmt uses it")

	b, err := json.Marshal(v)
	require.NoError(t, err)
	require.JSONEq(t, `"v1.2.3"`, string(b))

	// And in a struct field, which is how Info carries it.
	b, err = json.Marshal(struct {
		V version.SemVer `json:"v"`
	}{v})
	require.NoError(t, err)
	require.JSONEq(t, `{"v":"v1.2.3"}`, string(b))
}

func TestSemVerJSONRoundTrip(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"v1.2.3", "v1.2.3-rc1", ""} {
		var got version.SemVer
		require.NoError(t, json.Unmarshal([]byte(`"`+in+`"`), &got))
		require.Equal(t, in, got.String())
	}

	var got version.SemVer
	require.Error(t, json.Unmarshal([]byte(`42`), &got))
	require.Error(t, json.Unmarshal([]byte(`"garbage"`), &got))
}

// TestDevVersionSortsBelowEveryRelease is the property the derived version
// exists for. If it ever fails, an updater comparing an untagged build against
// a real tag would conclude it is already up to date and never move.
func TestDevVersionSortsBelowEveryRelease(t *testing.T) {
	t.Parallel()

	older := version.DevVersion("16663b7aaaa", time.Unix(1753901234, 0))
	newer := version.DevVersion("702ee23bbbb", time.Unix(1754300000, 0))

	require.Negative(t, older.Compare(newer), "derived versions order by build time")

	for _, release := range []string{"v0.0.0", "v0.0.1", "v1.0.0", "v99.99.99"} {
		rel, err := version.NewSemVer(release)
		require.NoError(t, err)
		require.Negative(t, newer.Compare(rel),
			"a derived dev version must sort below %s", release)
	}
}

func TestDevVersionShape(t *testing.T) {
	t.Parallel()

	v := version.DevVersion("702ee2312345", time.Unix(1754300000, 0))

	require.Equal(t, "v0.0.0-dev.1754300000.g702ee23", v.String())
	require.True(t, v.IsDev())
	require.False(t, v.IsZero())

	// A round trip through the parser must be lossless, because the value
	// travels as a string through flags, JSON and log lines.
	parsed, err := version.NewSemVer(v.String())
	require.NoError(t, err)
	require.Zero(t, parsed.Compare(v))
}

func TestDevVersionWithMissingParts(t *testing.T) {
	t.Parallel()

	require.Equal(t, "v0.0.0-dev", version.DevVersion("", time.Time{}).String())
	require.Equal(t, "v0.0.0-dev.g702ee23", version.DevVersion("702ee2312345", time.Time{}).String())
	require.True(t, version.DevVersion("", time.Time{}).IsDev())
}

// A real tag is not a dev version, whatever else it carries.
func TestIsDevOnlyMatchesDerived(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"v1.2.3", "v1.2.3-rc1", "v0.0.0", "v1.2.3-development"} {
		v, err := version.NewSemVer(in)
		require.NoError(t, err)
		require.False(t, v.IsDev(), "%s must not read as a derived dev version", in)
	}
}

// The documented trap: `git describe --tags` output parses, but sorts on the
// wrong side of its own tag. This test exists so the claim in DevVersion's doc
// comment is verified rather than asserted.
func TestGitDescribeOutputSortsBelowItsOwnTag(t *testing.T) {
	t.Parallel()

	tag, err := version.NewSemVer("v2.4.1")
	require.NoError(t, err)
	described, err := version.NewSemVer("v2.4.1-3-gabc1234")
	require.NoError(t, err)

	require.Negative(t, described.Compare(tag),
		"three commits past v2.4.1 reads as a prerelease of it; do not feed "+
			"git describe output to NewSemVer as a build version")
}

// A SemVer could not be stored before it had these: database/sql has no
// converter for a struct, and the embedded blang Version supplies none either.
func TestValueAndScanRoundTrip(t *testing.T) {
	t.Parallel()

	v, err := version.NewSemVer("v1.2.3")
	require.NoError(t, err)

	value, err := v.Value()
	require.NoError(t, err)
	require.Equal(t, "v1.2.3", value)

	for name, stored := range map[string]any{
		"a text column":         "v1.2.3",
		"a bytes driver":        []byte("v1.2.3"),
		"a row without the v":   "1.2.3",
		"a row missing a piece": "1.2",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var got version.SemVer
			require.NoError(t, got.Scan(stored))
			require.Equal(t, uint64(1), got.Major)
			require.Equal(t, uint64(2), got.Minor)
		})
	}
}

// "No version" and "version zero" are different facts. A column that stored the
// zero value as v0.0.0 could not tell them apart.
func TestTheZeroVersionIsNull(t *testing.T) {
	t.Parallel()

	value, err := version.SemVer{}.Value()
	require.NoError(t, err)
	require.Nil(t, value, "the zero SemVer must store as NULL")

	for _, stored := range []any{nil, "", "   "} {
		var got version.SemVer
		require.NoError(t, got.Scan(stored))
		require.True(t, got.IsZero(), "Scan(%v) must give the zero value", stored)
	}

	var bad version.SemVer
	require.Error(t, bad.Scan(42))
}
