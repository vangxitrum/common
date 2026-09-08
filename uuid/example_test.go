package uuid_test

import (
	"encoding/json"
	"fmt"

	"gitlab.internal/tuan.quang.tran/aioz-common/uuid"
)

// An id is minted wherever it is needed - no database round trip, no
// coordination - so a caller knows the identifier of a thing it has only just
// started creating.
func Example() {
	id := uuid.Must()

	fmt.Println(id.Version())
	fmt.Println(len(id.String()))
	fmt.Println(id.IsNil())
	// Output:
	// 7
	// 36
	// false
}

// The zero value is the nil UUID, so an absent id needs no pointer and no
// sentinel: it compares equal to uuid.Nil.
func ExampleUUID() {
	var absent uuid.UUID

	fmt.Println(absent == uuid.Nil)
	fmt.Println(absent)
	// Output:
	// true
	// 00000000-0000-0000-0000-000000000000
}

// Because the type is an array it is comparable, which is what lets an id index
// a map directly. A []byte-backed id cannot.
func ExampleUUID_map() {
	first, second := uuid.Must(), uuid.Must()

	names := map[uuid.UUID]string{first: "alice", second: "bob"}

	fmt.Println(names[first], names[second], len(names))
	// Output:
	// alice bob 2
}

// Both the dashed and undashed forms parse to the same id; String always emits
// the dashed one.
func ExampleFromString() {
	dashed, err := uuid.FromString("0195a4c7-8f3e-7b21-9c4d-1f2e3a4b5c6d")
	if err != nil {
		panic(err)
	}
	undashed, err := uuid.FromString("0195a4c78f3e7b219c4d1f2e3a4b5c6d")
	if err != nil {
		panic(err)
	}

	fmt.Println(dashed == undashed)
	fmt.Println(undashed)
	// Output:
	// true
	// 0195a4c7-8f3e-7b21-9c4d-1f2e3a4b5c6d
}

// An id encodes as its canonical string, and a JSON null decodes to Nil rather
// than failing, so an optional field needs no special case.
func ExampleUUID_MarshalJSON() {
	type row struct {
		ID   uuid.UUID `json:"id"`
		Prev uuid.UUID `json:"prev"`
	}

	var decoded row
	err := json.Unmarshal(
		[]byte(`{"id":"0195a4c7-8f3e-7b21-9c4d-1f2e3a4b5c6d","prev":null}`),
		&decoded,
	)
	if err != nil {
		panic(err)
	}

	fmt.Println(decoded.ID)
	fmt.Println(decoded.Prev.IsNil())
	// Output:
	// 0195a4c7-8f3e-7b21-9c4d-1f2e3a4b5c6d
	// true
}

// A List parses from a comma separated flag value and deduplicates in
// first-seen order.
func ExampleList() {
	var workers uuid.List
	err := workers.Set(
		"0195a4c7-8f3e-7b21-9c4d-1f2e3a4b5c6d," +
			"0195a4c7-8f3e-7b21-9c4d-1f2e3a4b5c6d," +
			"0195a4c7-8f3e-7b21-9c4d-000000000001",
	)
	if err != nil {
		panic(err)
	}

	fmt.Println(len(workers), len(workers.Unique()))
	fmt.Println(workers.Unique()[0])
	// Output:
	// 3 2
	// 0195a4c7-8f3e-7b21-9c4d-1f2e3a4b5c6d
}
