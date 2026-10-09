package httpapi

import (
	"encoding/base64"
	"net/url"
	"strconv"

	"gitlab.internal/tuan.quang.tran/aioz-common/apierr"
)

// The limits a list endpoint applies when its PageConfig leaves them zero.
const (
	DefaultLimit = 20
	MaxLimit     = 100
)

// PageConfig bounds one list endpoint. The zero value is the standard's
// default of 20 per page and at most 100.
type PageConfig struct {
	DefaultLimit int
	MaxLimit     int
}

// Page is the window a client asked for: offset and limit, or a cursor and a
// limit. Never both an offset and a cursor.
type Page struct {
	Offset int
	Limit  int
	Cursor string
}

// ParsePage reads offset, limit and cursor from a query string.
//
// A limit above the maximum is refused rather than clamped: a client that
// asked for 1000 and silently got 100 believes it has the whole list.
func ParsePage(q url.Values, config PageConfig) (Page, error) {
	if config.DefaultLimit <= 0 {
		config.DefaultLimit = DefaultLimit
	}
	if config.MaxLimit <= 0 {
		config.MaxLimit = MaxLimit
	}

	page := Page{Limit: config.DefaultLimit, Cursor: q.Get("cursor")}

	if raw := q.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 {
			return Page{}, apierr.BadRequest(
				"invalid-limit",
				"limit must be a positive integer",
			)
		}
		if limit > config.MaxLimit {
			return Page{}, apierr.BadRequest(
				"limit-too-large",
				"limit must be at most %d",
				config.MaxLimit,
			)
		}
		page.Limit = limit
	}

	if raw := q.Get("offset"); raw != "" {
		if page.Cursor != "" {
			return Page{}, apierr.BadRequest(
				"offset-with-cursor",
				"use offset or cursor, not both",
			)
		}
		offset, err := strconv.Atoi(raw)
		if err != nil || offset < 0 {
			return Page{}, apierr.BadRequest(
				"invalid-offset",
				"offset must be a non-negative integer",
			)
		}
		page.Offset = offset
	}

	if page.Cursor != "" {
		if _, err := DecodeCursor(page.Cursor); err != nil {
			return Page{}, err
		}
	}
	return page, nil
}

// Metadata is the metadata of this page of an offset list.
func (p Page) Metadata(total int64) Metadata {
	return Metadata{Offset: p.Offset, Limit: p.Limit, Total: total}
}

// EncodeCursor makes an opaque cursor from whatever the store needs to resume:
// usually the sort key and id of the last row sent. Opaque on purpose, so a
// client cannot build one and the store can change what it holds.
func EncodeCursor(
	state []byte,
) string {
	return base64.RawURLEncoding.EncodeToString(state)
}

// DecodeCursor reverses EncodeCursor. A cursor that does not decode is the
// client's mistake, so it is a 400.
func DecodeCursor(cursor string) ([]byte, error) {
	state, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(state) == 0 {
		return nil, apierr.BadRequest(
			"invalid-cursor",
			"cursor is not one this API issued",
		)
	}
	return state, nil
}
