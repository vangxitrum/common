package uuid_test

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/uuid"
)

// TestZeroValueIsNil: the zero value is the nil UUID, so a UUID needs no
// construction and == is the emptiness check.
func TestZeroValueIsNil(t *testing.T) {
	var id uuid.UUID
	require.True(t, id.IsNil())
	require.Equal(t, uuid.Nil, id)
	require.Equal(t, "00000000-0000-0000-0000-000000000000", id.String())
}

// TestNilCannotBeMutatedThroughAnAlias is why the type is an array. With a
// []byte and a package-level Nil sliced from another library's variable,
// writing through a copy reaches back into the original.
func TestNilCannotBeMutatedThroughAnAlias(t *testing.T) {
	corrupted := uuid.Nil
	corrupted[0] = 0xFF

	require.True(
		t,
		uuid.Nil.IsNil(),
		"Nil must be unaffected by a write to a copy",
	)
	require.False(t, corrupted.IsNil())
}

// TestUsableAsAMapKey: an array is comparable, a slice is not. This is the
// difference that decides whether ids can index a map without a conversion.
func TestUsableAsAMapKey(t *testing.T) {
	first, second := uuid.Must(), uuid.Must()

	seen := map[uuid.UUID]string{first: "first", second: "second"}
	require.Equal(t, "first", seen[first])
	require.Len(t, seen, 2)
}

func TestNewIsVersion7(t *testing.T) {
	id, err := uuid.New()
	require.NoError(t, err)
	require.Equal(t, 7, id.Version())
	require.False(t, id.IsNil())
}

// TestVersion7SortsByCreation is the reason for v7 over v4: ids generated later
// sort later, so a primary-key index appends instead of writing all over itself.
func TestVersion7SortsByCreation(t *testing.T) {
	const count = 50
	ids := make(uuid.List, count)
	for i := range ids {
		ids[i] = uuid.Must()
	}

	shuffled := make(uuid.List, count)
	copy(shuffled, ids)
	sort.Sort(shuffled)

	// Timestamps are millisecond-resolution, so ids made in the same
	// millisecond may tie; the sorted order must still be non-decreasing.
	for i := 1; i < count; i++ {
		require.LessOrEqual(t, shuffled[i-1].Compare(shuffled[i]), 0)
	}
}

func TestFromStringAcceptsBothForms(t *testing.T) {
	const dashed = "0195a4c7-8f3e-7b21-9c4d-1f2e3a4b5c6d"
	const undashed = "0195a4c78f3e7b219c4d1f2e3a4b5c6d"

	fromDashed, err := uuid.FromString(dashed)
	require.NoError(t, err)

	fromUndashed, err := uuid.FromString(undashed)
	require.NoError(t, err)

	require.Equal(t, fromDashed, fromUndashed)
	require.Equal(
		t,
		dashed,
		fromDashed.String(),
		"String always emits the dashed form",
	)
}

func TestFromStringRejectsGarbage(t *testing.T) {
	for _, bad := range []string{
		"",
		"not-a-uuid",
		"0195a4c7-8f3e-7b21-9c4d-1f2e3a4b5c6",   // too short
		"0195a4c7-8f3e-7b21-9c4d-1f2e3a4b5c6dd", // too long
		"0195a4c78f3e-7b21-9c4d-1f2e3a4b5c6d",   // dash in the wrong place
		"zzzzzzzz-8f3e-7b21-9c4d-1f2e3a4b5c6d",  // not hex
	} {
		_, err := uuid.FromString(bad)
		require.Errorf(t, err, "%q should be rejected", bad)
	}
}

// TestBytesIsACopy: handing out the backing array would let a caller mutate an
// id another goroutine holds.
func TestBytesIsACopy(t *testing.T) {
	id := uuid.Must()
	b := id.Bytes()
	b[0] ^= 0xFF

	require.NotEqual(t, b[0], id.Bytes()[0])
}

func TestFromBytesRejectsWrongLength(t *testing.T) {
	_, err := uuid.FromBytes([]byte{1, 2, 3})
	require.Error(t, err)

	id, err := uuid.FromBytes(make([]byte, 16))
	require.NoError(t, err)
	require.True(t, id.IsNil())
}

func TestJSONRoundTrip(t *testing.T) {
	type row struct {
		ID uuid.UUID `json:"id"`
	}

	original := row{ID: uuid.Must()}
	encoded, err := json.Marshal(original)
	require.NoError(t, err)
	require.Contains(t, string(encoded), original.ID.String())

	var back row
	require.NoError(t, json.Unmarshal(encoded, &back))
	require.Equal(t, original.ID, back.ID)
}

func TestJSONNullIsNil(t *testing.T) {
	var id uuid.UUID
	require.NoError(t, json.Unmarshal([]byte("null"), &id))
	require.True(t, id.IsNil())
}

// TestScanAcceptsWhatDriversReturn: a uuid column comes back as 16 raw bytes, a
// text column as a string, and a NULL as nil.
func TestScanAcceptsWhatDriversReturn(t *testing.T) {
	original := uuid.Must()

	var fromBytes uuid.UUID
	require.NoError(t, fromBytes.Scan(original.Bytes()))
	require.Equal(t, original, fromBytes)

	var fromString uuid.UUID
	require.NoError(t, fromString.Scan(original.String()))
	require.Equal(t, original, fromString)

	var fromTextBytes uuid.UUID
	require.NoError(t, fromTextBytes.Scan([]byte(original.String())))
	require.Equal(t, original, fromTextBytes)

	var fromNull uuid.UUID
	require.NoError(t, fromNull.Scan(nil))
	require.True(t, fromNull.IsNil())

	var bad uuid.UUID
	require.Error(t, bad.Scan(42))
}

// This asserted the raw bytes until a UUID written to a text column took the
// whole statement down with `invalid byte sequence for encoding "UTF8"`.
// Sixteen arbitrary bytes are not a string, and Value has no idea which kind of
// column it is being sent to.
func TestValueIsTheCanonicalString(t *testing.T) {
	id := uuid.Must()
	value, err := id.Value()
	require.NoError(t, err)
	require.Equal(t, id.String(), value)

	var back uuid.UUID
	require.NoError(t, back.Scan(value))
	require.Equal(t, id, back, "Value and Scan must round trip")
}

func TestProtoMethods(t *testing.T) {
	id := uuid.Must()
	require.Equal(t, uuid.Len, id.Size())

	buf := make([]byte, uuid.Len)
	n, err := id.MarshalTo(buf)
	require.NoError(t, err)
	require.Equal(t, uuid.Len, n)

	var back uuid.UUID
	require.NoError(t, back.Unmarshal(buf))
	require.Equal(t, id, back)

	_, err = id.MarshalTo(make([]byte, 4))
	require.Error(t, err, "a short buffer must not be written past")
}

func TestListUniqueAndContains(t *testing.T) {
	first, second := uuid.Must(), uuid.Must()
	list := uuid.List{first, second, first}

	require.True(t, list.Contains(second))
	require.Len(t, list.Unique(), 2)
	require.Equal(t, first, list.Unique()[0], "first-seen order is preserved")
}

func TestListSetAndString(t *testing.T) {
	first, second := uuid.Must(), uuid.Must()

	var list uuid.List
	require.NoError(t, list.Set(first.String()+", "+second.String()))
	require.Equal(t, uuid.List{first, second}, list)
	require.Equal(t, first.String()+","+second.String(), list.String())

	// An empty value clears the list rather than producing one empty element.
	require.NoError(t, list.Set(""))
	require.Empty(t, list)

	require.Error(t, list.Set("not-a-uuid"))
}

// Scan still accepts the raw bytes a uuid column may return: a driver that
// hands back sixteen bytes has not stopped existing just because Value no
// longer sends them.
func TestScanAcceptsBytesAndStrings(t *testing.T) {
	t.Parallel()

	id, err := uuid.FromString("0199322d-4c0a-7000-8000-000000000001")
	require.NoError(t, err)

	for name, value := range map[string]any{
		"the raw sixteen bytes": id.Bytes(),
		"the dashed string":     id.String(),
		"the dashed bytes":      []byte(id.String()),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var got uuid.UUID
			require.NoError(t, got.Scan(value))
			require.Equal(t, id, got)
		})
	}

	var nilled uuid.UUID
	require.NoError(t, nilled.Scan(nil))
	require.True(t, nilled.IsNil())

	require.Error(t, nilled.Scan(42))
}
