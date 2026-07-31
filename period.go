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
	"fmt"
	"time"
)

// Period represents a monthly payment period.
type Period struct {
	Year  int
	Month time.Month
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
