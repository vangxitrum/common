// Package uuid provides identifiers that can be generated anywhere.
//
// A UUID is a 128-bit number, generated so that two of them are for practical
// purposes never equal. The point is that nobody has to hand them out: a
// service, a worker, a test fixture or a client can each mint one without
// asking a database for the next value and without coordinating with anyone.
//
// That is the difference from an auto-increment integer, and it decides several
// things at once. An id can be assigned before the row is written, so a caller
// knows the identifier of a thing it has only just started creating. Records
// created on separate machines can be merged without renumbering. And an id in
// a URL or a log leaks nothing about how many exist, which a sequential integer
// does.
//
// The cost is size and ordering: sixteen bytes rather than eight, and no
// natural sequence unless the id carries a timestamp. New returns version 7
// ids, which carry one - see New.
//
//	id, err := uuid.New()
//	fmt.Println(id)                     // 0195a4c7-8f3e-7b21-9c4d-1f2e3a4b5c6d
//	id.IsNil()                          // false
//
//	var absent uuid.UUID                // the zero value is the nil UUID
//	absent == uuid.Nil                  // true
//
// The type is an array, so it compares with ==, works as a map key, and cannot
// be mutated by whoever it was handed to. See the README for why the more
// obvious []byte and string representations each give one of those up.
package uuid

import (
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	googleuuid "github.com/google/uuid"
	"github.com/zeebo/errs/v2"
)

// Len is the number of bytes in a UUID.
const Len = 16

// Error tags every failure this package produces.
var Error = errs.Tag("uuid")

// UUID is a 128-bit identifier.
//
// The zero value is the nil UUID, so a UUID is usable without construction and
// `id == uuid.Nil` is the emptiness check.
type UUID [Len]byte

// Nil is the all-zero UUID.
//
// It is an array, so this is a copy rather than a window onto shared memory:
// assigning to Nil[0] is a compile error on a constant expression and, where it
// is not, affects only that copy.
var Nil UUID

// New returns a new version 7 UUID: random, but with a millisecond timestamp in
// the leading bits, so ids sort by creation time. That keeps a B-tree index on
// an id column appending to one page instead of writing all over it, which is
// the difference a v4 primary key makes to a busy table.
func New() (UUID, error) {
	id, err := googleuuid.NewV7()
	if err != nil {
		return Nil, Error.Wrap(err)
	}
	return UUID(id), nil
}

// Must returns a new version 7 UUID and panics if one cannot be made. For
// tests, fixtures and package-level variables, where an error has nowhere to go.
func Must() UUID {
	id, err := New()
	if err != nil {
		panic(err)
	}
	return id
}

// FromGoogle converts a github.com/google/uuid value.
func FromGoogle(id googleuuid.UUID) UUID { return UUID(id) }

// Google converts to a github.com/google/uuid value.
func (id UUID) Google() googleuuid.UUID { return googleuuid.UUID(id) }

// FromBytes builds a UUID from exactly Len bytes. The bytes are copied, so the
// caller may reuse the slice.
func FromBytes(b []byte) (UUID, error) {
	if len(b) != Len {
		return Nil, Error.Errorf("expected %d bytes, got %d", Len, len(b))
	}
	var id UUID
	copy(id[:], b)
	return id, nil
}

// FromString parses the canonical dashed form and the undashed form. Both are
// accepted because the dashes carry no information and databases, URLs and
// hand-written fixtures disagree about them.
func FromString(s string) (UUID, error) {
	switch len(s) {
	case 36:
		if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
			return Nil, Error.Errorf("invalid UUID format: %q", s)
		}
		return fromHex(s[0:8] + s[9:13] + s[14:18] + s[19:23] + s[24:36])
	case 32:
		return fromHex(s)
	default:
		return Nil, Error.Errorf("invalid UUID length %d: %q", len(s), s)
	}
}

func fromHex(s string) (UUID, error) {
	var id UUID
	if _, err := hex.Decode(id[:], []byte(s)); err != nil {
		return Nil, Error.Errorf("invalid UUID %q: %v", s, err)
	}
	return id, nil
}

// IsNil reports whether id is the all-zero UUID.
func (id UUID) IsNil() bool { return id == Nil }

// Bytes returns a copy of the id's bytes. A copy, not a window: the caller
// cannot reach back into the id through it.
func (id UUID) Bytes() []byte {
	b := make([]byte, Len)
	copy(b, id[:])
	return b
}

// Version returns the UUID version nibble - 7 for ids from New.
func (id UUID) Version() int { return int(id[6] >> 4) }

// String returns the canonical dashed lowercase form.
func (id UUID) String() string {
	var buf [36]byte
	hex.Encode(buf[0:8], id[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], id[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], id[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], id[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], id[10:16])
	return string(buf[:])
}

// Compare returns -1, 0 or 1 as id sorts before, with, or after other. The
// ordering is bytewise, which for version 7 ids is also creation order.
func (id UUID) Compare(other UUID) int {
	for i := range Len {
		switch {
		case id[i] < other[i]:
			return -1
		case id[i] > other[i]:
			return 1
		}
	}
	return 0
}

// Less reports whether id sorts before other.
func (id UUID) Less(other UUID) bool { return id.Compare(other) < 0 }

// MarshalText implements encoding.TextMarshaler, which is what makes a UUID
// work as a map key in JSON and as a URL path parameter.
func (id UUID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (id *UUID) UnmarshalText(b []byte) error {
	parsed, err := FromString(string(b))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// MarshalJSON emits the canonical dashed string.
func (id UUID) MarshalJSON() ([]byte, error) { return json.Marshal(id.String()) }

// UnmarshalJSON accepts the dashed or undashed string. A JSON null leaves the
// id nil rather than failing, so an optional field needs no special case.
func (id *UUID) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*id = Nil
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return Error.Wrap(err)
	}
	return id.UnmarshalText([]byte(s))
}

// Value implements driver.Valuer, writing the canonical dashed string.
//
// The raw 16 bytes would be the cheaper thing to send to a uuid column - that
// is what the column holds, and it saves the driver a parse per row - but it is
// wrong everywhere else. Sent to a text or varchar column, those bytes are not
// valid UTF-8 and Postgres rejects the whole statement:
//
//	ERROR: invalid byte sequence for encoding "UTF8": 0xa0 (SQLSTATE 22021)
//
// and lib/pq will not send a []byte to a uuid column at all. The string form
// works for every column type and every driver, and produces the identical
// value in a uuid column. A parse per row is the cheaper mistake.
func (id UUID) Value() (driver.Value, error) { return id.String(), nil }

// Scan implements sql.Scanner, accepting the raw bytes a uuid column returns
// and the string form a text or varchar column returns.
func (id *UUID) Scan(value any) error {
	switch v := value.(type) {
	case nil:
		*id = Nil
		return nil
	case []byte:
		if len(v) == Len {
			parsed, err := FromBytes(v)
			if err != nil {
				return err
			}
			*id = parsed
			return nil
		}
		return id.UnmarshalText(v)
	case string:
		return id.UnmarshalText([]byte(v))
	default:
		return Error.Errorf("cannot scan %T into UUID", value)
	}
}

// Marshal returns the id's bytes.
//
// It, along with MarshalTo, Unmarshal and Size, implements the gogo/protobuf
// custom type interface. They are plain byte copies and pull in no protobuf
// dependency; a service that uses gogo can wire the type in as-is, and one that
// does not pays nothing for them.
func (id UUID) Marshal() ([]byte, error) { return id.Bytes(), nil }

// MarshalTo writes the id into data, which must hold at least Len bytes.
func (id UUID) MarshalTo(data []byte) (int, error) {
	if len(data) < Len {
		return 0, Error.Errorf(
			"buffer too small: need %d bytes, have %d",
			Len,
			len(data),
		)
	}
	return copy(data, id[:]), nil
}

// Unmarshal reads an id from exactly Len bytes.
func (id *UUID) Unmarshal(data []byte) error {
	parsed, err := FromBytes(data)
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

// Size is the marshaled width, always Len.
func (id UUID) Size() int { return Len }

// List is a slice of UUIDs that sorts, deduplicates, and parses from a comma
// separated flag value.
type List []UUID

// Len implements sort.Interface.
func (l List) Len() int { return len(l) }

// Swap implements sort.Interface.
func (l List) Swap(i, j int) { l[i], l[j] = l[j], l[i] }

// Less implements sort.Interface.
func (l List) Less(i, j int) bool { return l[i].Less(l[j]) }

// Sort orders the list ascending, in place.
func (l List) Sort() { sort.Sort(l) }

// Strings renders every id in canonical form.
func (l List) Strings() []string {
	out := make([]string, len(l))
	for i, id := range l {
		out[i] = id.String()
	}
	return out
}

// Bytes renders every id as raw bytes, for a query parameter or a protobuf
// repeated field.
func (l List) Bytes() [][]byte {
	out := make([][]byte, len(l))
	for i, id := range l {
		out[i] = id.Bytes()
	}
	return out
}

// Contains reports whether the list holds id.
func (l List) Contains(id UUID) bool {
	for _, candidate := range l {
		if candidate == id {
			return true
		}
	}
	return false
}

// Unique returns the list with duplicates removed, preserving first-seen order.
func (l List) Unique() List {
	seen := make(map[UUID]struct{}, len(l))
	out := make(List, 0, len(l))
	for _, id := range l {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// String implements flag.Value.
func (l List) String() string { return strings.Join(l.Strings(), ",") }

// Type implements pflag.Value, naming the type in generated help text.
func (List) Type() string { return "uuidList" }

// Set implements flag.Value, parsing a comma separated list. An empty string
// clears the list rather than producing one empty element.
func (l *List) Set(s string) error {
	*l = nil
	if strings.TrimSpace(s) == "" {
		return nil
	}
	for _, part := range strings.Split(s, ",") {
		id, err := FromString(strings.TrimSpace(part))
		if err != nil {
			return fmt.Errorf("%q: %w", part, err)
		}
		*l = append(*l, id)
	}
	return nil
}
