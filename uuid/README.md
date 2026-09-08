# uuid

Identifiers that can be generated anywhere.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/uuid"
```

## What a UUID is, and when to reach for one

A UUID is a 128-bit number, generated so that two of them are for practical
purposes never equal. The point is that **nobody has to hand them out**: a
service, a worker, a test fixture or a client can each mint one without asking a
database for the next value and without coordinating with anyone.

That is the whole difference from an auto-increment integer, and it decides
several things at once:

- **An id exists before the row does.** A caller can know the identifier of a
  thing it has only just started creating, which is what lets it write related
  rows in one go, or return the id from a request that queues the real work.
- **Records from separate machines merge without renumbering.** Two shards, two
  regions, or an offline client and a server cannot collide.
- **An id leaks nothing.** `/orders/1834` tells a stranger roughly how many
  orders exist and lets them guess the next one; a UUID tells them nothing.

The cost is size and ordering: sixteen bytes rather than eight, and no natural
sequence unless the id carries a timestamp. Which is why these are version 7.

**Use one for anything crossing a boundary** - a row's primary key, a request
id, a job id. **Do not use one** where a small dense integer is genuinely
better, such as an enum or an index into an in-memory table.

## Why a `[16]byte`

Every alternative gives something up, and the array form gives up none of it:

| | `string` | `[]byte` | `[16]byte` |
| --- | --- | --- | --- |
| validated | no | no | **yes** |
| comparable with `==` | yes | **no** | **yes** |
| usable as a map key | yes | **no** | **yes** |
| nil vs zero unambiguous | n/a | **no** | **yes** |
| immutable once held | yes | **no** | **yes** |

The mutability one is the sharp edge. A slice-backed UUID with a package-level
`Nil` sliced out of another library's variable shares memory with it, so writing
through any copy corrupts that library's `Nil` process-wide. An array copy is a
copy.

## Usage

```go
id, err := uuid.New()   // version 7
id := uuid.Must()       // for tests and fixtures

if id.IsNil() { ... }   // or: id == uuid.Nil
```

**Version 7, not 4.** A v7 id is random but carries a millisecond timestamp in
its leading bits, so ids sort by creation time. A B-tree index on an id column
then appends to one page instead of writing all over the index, which is the
difference a v4 primary key makes to a busy table.

## Parsing and rendering

`FromString` accepts both the dashed and undashed forms - the dashes carry no
information, and databases, URLs and hand-written fixtures disagree about them.
`String` always emits the canonical dashed lowercase form.

```go
id, err := uuid.FromString("0195a4c7-8f3e-7b21-9c4d-1f2e3a4b5c6d")
id, err := uuid.FromString("0195a4c78f3e7b219c4d1f2e3a4b5c6d")   // same id
id, err := uuid.FromBytes(raw)                                    // copies raw
```

`Bytes()` returns a copy, so a caller cannot reach back into an id through it.

## Storage and encoding

`Value` writes the 16 raw bytes, which is what a Postgres `uuid` column holds -
no hex round trip per row. `Scan` accepts raw bytes, the string form a `text`
column returns, and `nil`.

`MarshalText`/`UnmarshalText` are implemented, which is what lets a UUID be a
JSON map key and a URL path parameter. JSON `null` decodes to `Nil` rather than
failing, so an optional field needs no special case.

`Marshal`, `MarshalTo`, `Unmarshal` and `Size` satisfy the gogo/protobuf custom
type interface. They are plain byte copies and import no protobuf package: a
service using gogo can wire the type in as-is, and one that does not pays
nothing for them.

## List

`List` is a `[]UUID` that sorts, deduplicates and parses from a flag:

```go
var ids uuid.List
flag.Var(&ids, "workers", "worker ids")   // "id1,id2,id3"

ids.Sort()
ids.Unique()          // first-seen order preserved
ids.Contains(id)
ids.Strings()         // for a query
ids.Bytes()           // for a protobuf repeated field
```

It implements `sort.Interface`, `flag.Value` and pflag's `Type()`, so it appears
correctly in generated help text. An empty flag value clears the list rather
than producing one empty element.

## API

| | |
| --- | --- |
| `New() (UUID, error)` / `Must() UUID` | a new version 7 id |
| `FromString` / `FromBytes` / `FromGoogle` | parse or convert |
| `Nil`, `Len` | the zero id; 16 |
| `IsNil`, `Version`, `Bytes`, `String`, `Google` | inspect |
| `Compare`, `Less` | bytewise order, which for v7 is creation order |
| `Value` / `Scan` | `database/sql` |
| `MarshalText` / `MarshalJSON` (+ Unmarshal) | encoding |
| `Marshal` / `MarshalTo` / `Unmarshal` / `Size` | gogo/protobuf custom type |
| `List` | sortable, dedupable, flag-parsable slice |
