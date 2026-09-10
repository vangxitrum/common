package period_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/period"
)

// A Period could not be stored at all before it had these: database/sql has no
// converter for a struct, and the failure arrived at the first query rather
// than at compile time.
func TestValueAndScanRoundTrip(t *testing.T) {
	t.Parallel()

	p, err := period.FromString("2026-07")
	require.NoError(t, err)

	value, err := p.Value()
	require.NoError(t, err)
	require.Equal(t, "2026-07", value)

	for name, stored := range map[string]any{
		"a text column":  "2026-07",
		"a bytes driver": []byte("2026-07"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var got period.Period
			require.NoError(t, got.Scan(stored))
			require.Equal(t, p, got)
		})
	}
}

// "No period" is a real state - there is nothing outstanding to close - so it
// is NULL rather than "0000-00", which is not a month.
func TestTheZeroPeriodIsNull(t *testing.T) {
	t.Parallel()

	value, err := period.Period{}.Value()
	require.NoError(t, err)
	require.Nil(t, value)

	var got period.Period
	require.NoError(t, got.Scan(nil))
	require.True(t, got.IsZero())
}

func TestScanRejectsWhatItCannotRead(t *testing.T) {
	t.Parallel()

	var got period.Period
	require.Error(t, got.Scan(42))
	require.Error(t, got.Scan("not-a-period"))
}
