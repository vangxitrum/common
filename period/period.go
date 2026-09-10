// Package period is the monthly billing/payout period shared by both ledgers.
//
// It lives on its own because the worker payout side (internal/compensation) and
// the client billing side (internal/billing) must agree on exactly what "2026-07"
// means — the same YYYY-MM string, the same UTC start, and the same exclusive end.
// If the two disagreed by so much as an hour, a period boundary would be paid out
// on one side and not billed on the other. Neither package can own the type without
// the other importing it, so it is a leaf of its own.
package period

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"time"
)

// A Period crosses a database boundary as its YYYY-MM string.
var (
	_ driver.Valuer = Period{}
	_ sql.Scanner   = (*Period)(nil)
)

// Period represents a monthly payment period.
type Period struct {
	Year  int
	Month time.Month
}

// IsZero reports whether the Period is the zero value.
//
// Year zero rather than month zero, because a Period built from a bad month still
// has a year and would be worth catching elsewhere; the zero value is the only thing
// this answers for. Callers use it to mean "no period", which is a real state - there
// is nothing outstanding to close.
func (p Period) IsZero() bool {
	return p.Year == 0 && p.Month == 0
}

// String outputs the YYYY-MM form of the payment period.
func (p Period) String() string {
	return fmt.Sprintf("%04d-%02d", p.Year, p.Month)
}

// StartDate returns a time.Time that is less than or equal to any time in the period.
func (p Period) StartDate() time.Time {
	return time.Date(p.Year, p.Month, 1, 0, 0, 0, 0, time.UTC)
}

// EndDateExclusive returns a time.Time that is greater than any time in the period.
//
// Exclusive, not inclusive: paired with StartDate it forms the half-open range the
// usage queries take, so the hour on a month boundary belongs to exactly one period.
func (p Period) EndDateExclusive() time.Time {
	return time.Date(p.Year, p.Month+1, 1, 0, 0, 0, 0, time.UTC)
}

// UnmarshalCSV reads the Period in CSV form.
func (p *Period) UnmarshalCSV(s string) error {
	v, err := FromString(s)
	if err != nil {
		return err
	}
	*p = v
	return nil
}

// MarshalCSV returns the CSV form of the Period.
func (p Period) MarshalCSV() (string, error) {
	return p.String(), nil
}

// FromString parses the YYYY-MM string into a Period.
func FromString(s string) (Period, error) {
	t, err := time.Parse("2006-01", s)
	if err != nil {
		return Period{}, err
	}
	return FromTime(t), nil
}

// FromTime takes a time.Time and returns a Period that contains it.
func FromTime(t time.Time) Period {
	year, month, _ := t.UTC().Date()
	return Period{
		Year:  year,
		Month: month,
	}
}

// Value implements driver.Valuer, writing the YYYY-MM form.
//
// Without it a Period cannot be stored at all: database/sql refuses a struct it
// has no converter for, and the failure arrives at the first query rather than
// at compile time. A zero Period stores as NULL, because "no period" is a real
// state here and "0000-00" is not a month.
func (p Period) Value() (driver.Value, error) {
	if p.IsZero() {
		return nil, nil
	}
	return p.String(), nil
}

// Scan implements sql.Scanner, reading the YYYY-MM form back.
//
// It accepts the string a text column returns and the []byte some drivers
// return for the same column, and reads NULL back as the zero Period, which is
// what Value wrote.
func (p *Period) Scan(value any) error {
	switch v := value.(type) {
	case nil:
		*p = Period{}
		return nil
	case string:
		parsed, err := FromString(v)
		if err != nil {
			return err
		}
		*p = parsed
		return nil
	case []byte:
		parsed, err := FromString(string(v))
		if err != nil {
			return err
		}
		*p = parsed
		return nil
	default:
		return fmt.Errorf("cannot scan %T into period.Period", value)
	}
}
