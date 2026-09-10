package postgres_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/currency"
	"gitlab.internal/tuan.quang.tran/aioz-common/memory"
	"gitlab.internal/tuan.quang.tran/aioz-common/period"
	"gitlab.internal/tuan.quang.tran/aioz-common/stats/testcontext"
	"gitlab.internal/tuan.quang.tran/aioz-common/uuid"
	"gitlab.internal/tuan.quang.tran/aioz-common/version"
)

// The shared value types have to survive a real column, not a unit test's idea
// of one. This is the test that would have caught a UUID written as sixteen raw
// bytes into a text column - which Postgres rejects outright, because those
// bytes are not valid UTF-8.
func TestSharedTypesRoundTripThroughColumns(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	db := open(t, ctx, containerConfig(t))

	_, err := db.Exec(ctx, `CREATE TABLE shared_types (
		id        uuid    PRIMARY KEY,
		id_text   text    NOT NULL,
		bill      text    NOT NULL,
		release   text,
		amount    numeric NOT NULL,
		quota     bigint  NOT NULL
	)`)
	require.NoError(t, err)

	id, err := uuid.New()
	require.NoError(t, err)

	bill, err := period.FromString("2026-07")
	require.NoError(t, err)

	release, err := version.NewSemVer("v1.2.3")
	require.NoError(t, err)

	amount := currency.MustParse[currency.AIOZUnit]("12.5")
	quota := 32 * memory.MiB

	// The same id into a uuid column and a text column. The text column is the
	// one that used to fail.
	_, err = db.Exec(ctx,
		`INSERT INTO shared_types (id, id_text, bill, release, amount, quota) VALUES ($1, $2, $3, $4, $5, $6)`,
		id, id, bill, release, amount, quota)
	require.NoError(t, err)

	var (
		gotID      uuid.UUID
		gotIDText  uuid.UUID
		gotBill    period.Period
		gotRelease version.SemVer
		gotAmount  currency.AIOZ
		gotQuota   memory.Size
	)
	require.NoError(t, db.QueryRow(ctx,
		`SELECT id, id_text, bill, release, amount, quota FROM shared_types`).
		Scan(&gotID, &gotIDText, &gotBill, &gotRelease, &gotAmount, &gotQuota))

	require.Equal(t, id, gotID)
	require.Equal(t, id, gotIDText, "the text column must hold the canonical string")
	require.Equal(t, bill, gotBill)
	require.Equal(t, release.String(), gotRelease.String())
	require.True(t, amount.Equal(gotAmount))

	// memory.Size needs nothing added: its underlying type is int64, which
	// every driver already converts. This asserts that rather than assuming it.
	require.Equal(t, quota, gotQuota)

	// And what the text column actually holds is readable, rather than sixteen
	// bytes nobody can grep for.
	var stored string
	require.NoError(t, db.QueryRow(ctx, `SELECT id_text FROM shared_types`).Scan(&stored))
	require.Equal(t, id.String(), stored)
}

// A zero value is not "the zero of the type" in a column, it is NULL: "no
// period" and "no version" are real states, and a column that stored them as
// 0000-00 or v0.0.0 could not tell them from a real one.
func TestZeroValuesStoreAsNull(t *testing.T) {
	t.Parallel()

	ctx := testcontext.New(t)
	defer ctx.Cleanup()

	db := open(t, ctx, containerConfig(t))

	_, err := db.Exec(ctx, `CREATE TABLE nullable_types (bill text, release text)`)
	require.NoError(t, err)

	_, err = db.Exec(ctx, `INSERT INTO nullable_types (bill, release) VALUES ($1, $2)`,
		period.Period{}, version.SemVer{})
	require.NoError(t, err)

	var billNull, releaseNull bool
	require.NoError(t, db.QueryRow(ctx,
		`SELECT bill IS NULL, release IS NULL FROM nullable_types`).
		Scan(&billNull, &releaseNull))
	require.True(t, billNull)
	require.True(t, releaseNull)

	// And NULL reads back as the zero value it was written from.
	var (
		bill    period.Period
		release version.SemVer
	)
	require.NoError(t, db.QueryRow(ctx,
		`SELECT bill, release FROM nullable_types`).Scan(&bill, &release))
	require.True(t, bill.IsZero())
	require.True(t, release.IsZero())
}
