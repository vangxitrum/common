package version

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"strings"

	"github.com/blang/semver/v4"
)

// A SemVer crosses a database boundary as its rendered string.
var (
	_ driver.Valuer = SemVer{}
	_ sql.Scanner   = (*SemVer)(nil)
)

// SemVer is a semantic version that can be compared.
//
// It wraps blang/semver rather than reimplementing the parse, and adds the
// two behaviors this codebase needs on top: a leading "v" in the rendered
// form, because that is how every internal tag is written, and a zero value
// that renders and marshals as the empty string rather than as "v0.0.0", so
// "no version" stays distinguishable from "version zero".
type SemVer struct {
	semver.Version
}

// NewSemVer parses v, which may be written with or without the leading "v"
// and may omit the minor or patch component.
//
// Parsing is tolerant on purpose. An internal service's version reaches this
// function from a Makefile, a CI variable, a Docker label or a `go install`
// path, and those disagree about the "v" and about how many components they
// write. What they must not disagree on is the ordering, which is why the
// result is a comparable value rather than the string that came in.
func NewSemVer(v string) (SemVer, error) {
	parsed, err := semver.ParseTolerant(v)
	if err != nil {
		return SemVer{}, Error.Wrap(err)
	}
	return SemVer{Version: parsed}, nil
}

// Compare returns -1 if s is lower than other, 0 if they are equal, and 1 if
// s is higher.
//
// Ordering follows semver, which means a prerelease sorts BELOW the release it
// precedes: v1.2.3-rc1 is older than v1.2.3. See [DevVersion] for why that is
// the property an untagged build wants.
func (s SemVer) Compare(other SemVer) int {
	return s.Version.Compare(other.Version)
}

// IsZero reports whether s is the zero value, meaning no version is known.
func (s SemVer) IsZero() bool {
	return s.Major == 0 && s.Minor == 0 && s.Patch == 0 &&
		len(s.Pre) == 0 && len(s.Build) == 0
}

// String renders the version with a leading "v", or "" if it is the zero
// value.
//
// The receiver is a value, not a pointer, so a SemVer stored in a struct field
// still renders through fmt. blang's own String has a value receiver but omits
// the "v", and its method would otherwise be promoted and silently used.
func (s SemVer) String() string {
	if s.IsZero() {
		return ""
	}

	var sb strings.Builder
	sb.WriteByte('v')
	sb.WriteString(s.Version.String())
	return sb.String()
}

// MarshalJSON renders the version as a string, matching [SemVer.String].
//
// Without this, the embedded blang Version's MarshalJSON is promoted and
// writes the version without its "v", so the JSON and the printed form of the
// same value would disagree.
func (s SemVer) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

// UnmarshalJSON parses a version written as a JSON string.
func (s *SemVer) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err != nil {
		return Error.Wrap(err)
	}
	if str == "" {
		*s = SemVer{}
		return nil
	}

	parsed, err := NewSemVer(str)
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}

// Value implements driver.Valuer, writing the rendered form with its leading
// "v".
//
// Without it a SemVer cannot be stored: database/sql has no converter for a
// struct, and the embedded blang Version would not have supplied one either.
// The zero value stores as NULL rather than as "v0.0.0", for the same reason
// String renders it empty - "no version" and "version zero" are different
// facts, and a column that cannot tell them apart loses one.
func (s SemVer) Value() (driver.Value, error) {
	if s.IsZero() {
		return nil, nil
	}
	return s.String(), nil
}

// Scan implements sql.Scanner, parsing the stored form back.
//
// Parsing is NewSemVer's, so it is as tolerant reading a column as it is
// reading a Makefile variable: a row written by an older release that omitted
// the "v", or the patch component, still reads.
func (s *SemVer) Scan(value any) error {
	switch v := value.(type) {
	case nil:
		*s = SemVer{}
		return nil
	case string:
		return s.scanString(v)
	case []byte:
		return s.scanString(string(v))
	default:
		return Error.Errorf("cannot scan %T into SemVer", value)
	}
}

func (s *SemVer) scanString(v string) error {
	if strings.TrimSpace(v) == "" {
		*s = SemVer{}
		return nil
	}
	parsed, err := NewSemVer(v)
	if err != nil {
		return err
	}
	*s = parsed
	return nil
}
