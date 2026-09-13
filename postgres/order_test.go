package postgres_test

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/postgres"
)

var sortable = postgres.SortColumns{
	"title":   {"media", "title"},
	"created": {"created_at"},
}

func TestOrderByAnAllowedKey(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		key, direction, want string
	}{
		{"title", "desc", `"media"."title" DESC`},
		{"title", "DESC", `"media"."title" DESC`},
		{"created", "asc", `"created_at" ASC`},
		{"created", "", `"created_at" ASC`},
		{"created", " Asc ", `"created_at" ASC`},
	} {
		got, err := sortable.OrderBy(c.key, c.direction)
		require.NoError(t, err, "%q %q", c.key, c.direction)
		require.Equal(t, c.want, got, "%q %q", c.key, c.direction)
	}
}

// The key is matched exactly. Folding case or trimming would make the
// allowlist larger than what was written down, and a column name is not a
// key: only the public names the endpoint documents are.
func TestAnythingElseIsAnInvalidSort(t *testing.T) {
	t.Parallel()

	for _, c := range []struct{ key, direction string }{
		{"Title", "asc"},
		{" title", "asc"},
		{"title ", "asc"},
		{"", "asc"},
		{"created_at", "asc"},
		{"title; DROP TABLE media", "asc"},
		{`title" DESC, "secret`, "asc"},
		{"title", "descending"},
		{"title", "desc; DROP TABLE media"},
		{"title", "1"},
		{"title", "desc nulls first"},
	} {
		_, err := sortable.OrderBy(c.key, c.direction)
		require.ErrorIs(
			t,
			err,
			postgres.ErrInvalidSort,
			"%q %q",
			c.key,
			c.direction,
		)
	}
}

// A column name is quoted as an identifier, so even a hostile name in the
// allowlist itself cannot end the ORDER BY.
func TestTheColumnIsQuotedAsAnIdentifier(t *testing.T) {
	t.Parallel()

	got, err := postgres.SortColumns{"x": {`we"ird`}}.OrderBy("x", "")
	require.NoError(t, err)
	require.Equal(t, `"we""ird" ASC`, got)
}

// An empty identifier is a mistake in the allowlist, the server's, not a bad
// request: it must not come back as the 400 ErrInvalidSort maps to.
func TestAnEmptyIdentifierIsAServerError(t *testing.T) {
	t.Parallel()

	for name, cols := range map[string]postgres.SortColumns{
		"no parts":      {"x": pgx.Identifier{}},
		"a nil entry":   {"x": nil},
		"an empty part": {"x": {"media", ""}},
	} {
		_, err := cols.OrderBy("x", "asc")
		require.Error(t, err, name)
		require.NotErrorIs(t, err, postgres.ErrInvalidSort, name)
	}
}

// An empty sort parameter means "no sort chosen", which the caller answers
// with its own default. It is refused even from an allowlist that lists "",
// so the default stays in one place.
func TestAnEmptyKeyIsRefusedEvenIfListed(t *testing.T) {
	t.Parallel()

	_, err := postgres.SortColumns{"": {"created_at"}}.OrderBy("", "asc")
	require.ErrorIs(t, err, postgres.ErrInvalidSort)
}
