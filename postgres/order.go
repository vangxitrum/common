package postgres

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ErrInvalidSort is a sort key or direction the endpoint does not offer: the
// caller's mistake, answered with 400. Test with errors.Is.
var ErrInvalidSort = errors.New("invalid sort")

// SortColumns maps the sort keys an endpoint documents to the columns they
// order by.
//
// ORDER BY cannot take a bind parameter, so a sort chosen by the request is
// where SQL injection survives in code that parameterizes everything else.
// Building the clause from an allowlist closes it:
//
//	var mediaSort = postgres.SortColumns{
//		"title":   {"media", "title"},
//		"created": {"media", "created_at"},
//	}
//
//	orderBy, err := mediaSort.OrderBy(q.Get("sort"), q.Get("order"))
//	if errors.Is(err, postgres.ErrInvalidSort) {
//		// 400
//	}
//	sql := "SELECT ... FROM media ORDER BY " + orderBy + ", media.id LIMIT $1 OFFSET $2"
//
// Paginating with OFFSET needs a unique last column, like the id above:
// rows that tie on the sort column otherwise come back in any order, and a
// page can repeat or skip them. NULLs sort last for ASC and first for DESC.
type SortColumns map[string]pgx.Identifier

// OrderBy returns the ORDER BY expression for key, such as
// "media"."title" DESC. The key is matched exactly; the direction is asc or
// desc in any case, and empty is ASC. Anything else is ErrInvalidSort, with
// no part of the request in the message.
//
// A key that maps to an empty identifier is an error that is not
// ErrInvalidSort: the allowlist is wrong, not the request.
func (c SortColumns) OrderBy(key, direction string) (string, error) {
	column, ok := c[key]
	if !ok || key == "" {
		return "", Error.Errorf("%w: unknown sort key", ErrInvalidSort)
	}

	var dir string
	switch strings.ToLower(strings.TrimSpace(direction)) {
	case "", "asc":
		dir = "ASC"
	case "desc":
		dir = "DESC"
	default:
		return "", Error.Errorf(
			"%w: direction must be asc or desc",
			ErrInvalidSort,
		)
	}

	if len(column) == 0 {
		return "", Error.Errorf("sort key %q maps to no column", key)
	}
	for _, part := range column {
		if part == "" {
			return "", Error.Errorf(
				"sort key %q maps to an empty identifier",
				key,
			)
		}
	}
	return column.Sanitize() + " " + dir, nil
}
