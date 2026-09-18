package telemetry

import (
	"encoding/binary"
	"math"
)

// The Prometheus remote_write v1 payload, encoded by hand.
//
// The protocol needs four messages out of prometheus/prompb:
//
//	message WriteRequest { repeated TimeSeries timeseries = 1; }
//	message TimeSeries   { repeated Label labels = 1; repeated Sample samples = 2; }
//	message Label        { string name = 1; string value = 2; }
//	message Sample       { double value = 1; int64 timestamp = 2; }
//
// They are written directly to the wire here rather than generated, so a
// library every service imports does not drag in protoc, a .proto file, and
// the gogo/protobuf fork (which needs a `replace` directive to build) for
// four messages that have not changed since the protocol was published.
// Field numbers match upstream exactly, so any standard receiver parses this.
//
// Zero-valued scalars are omitted, as proto3 requires and as prompb's own
// generated marshaler does, so a sample whose value is 0 carries only its
// timestamp and decodes back to 0.

// label is one name/value pair on a series.
type label struct {
	Name  string
	Value string
}

// sample is one measurement: a value at a millisecond timestamp.
type sample struct {
	Value     float64
	Timestamp int64
}

// timeSeries is a label set with its samples. This client always sends
// exactly one sample per series per push.
type timeSeries struct {
	Labels  []label
	Samples []sample
}

// marshalWriteRequest encodes series as a serialized prompb.WriteRequest.
func marshalWriteRequest(series []timeSeries) []byte {
	buf := make([]byte, 0, estimateSize(series))
	for i := range series {
		buf = appendMessageField(buf, 1, series[i].appendTo)
	}
	return buf
}

// appendTo encodes one TimeSeries into dst.
func (ts timeSeries) appendTo(dst []byte) []byte {
	for _, l := range ts.Labels {
		dst = appendMessageField(dst, 1, l.appendTo)
	}
	for _, s := range ts.Samples {
		dst = appendMessageField(dst, 2, s.appendTo)
	}
	return dst
}

// appendTo encodes one Label into dst.
func (l label) appendTo(dst []byte) []byte {
	if l.Name != "" {
		dst = appendStringField(dst, 1, l.Name)
	}
	if l.Value != "" {
		dst = appendStringField(dst, 2, l.Value)
	}
	return dst
}

// appendTo encodes one Sample into dst.
func (s sample) appendTo(dst []byte) []byte {
	if s.Value != 0 {
		dst = appendTag(dst, 1, wireFixed64)
		dst = binary.LittleEndian.AppendUint64(dst, math.Float64bits(s.Value))
	}
	if s.Timestamp != 0 {
		dst = appendTag(dst, 2, wireVarint)
		dst = binary.AppendUvarint(dst, uint64(s.Timestamp))
	}
	return dst
}

const (
	wireVarint       = 0
	wireFixed64      = 1
	wireLenDelimited = 2
)

// appendTag writes a field header: the field number and its wire type.
func appendTag(dst []byte, field int, wireType byte) []byte {
	return binary.AppendUvarint(dst, uint64(field)<<3|uint64(wireType))
}

// appendStringField writes a length-delimited string field.
func appendStringField(dst []byte, field int, s string) []byte {
	dst = appendTag(dst, field, wireLenDelimited)
	dst = binary.AppendUvarint(dst, uint64(len(s)))
	return append(dst, s...)
}

// appendMessageField writes a length-delimited submessage field. The
// submessage is encoded into a scratch region at the end of dst first,
// because its length has to precede it and is not known until then; the body
// is shifted right by the size of the length varint once that is known. This
// keeps everything in one buffer instead of allocating a new one per message.
func appendMessageField(dst []byte, field int, encode func([]byte) []byte) []byte {
	dst = appendTag(dst, field, wireLenDelimited)
	lenStart := len(dst)
	body := encode(dst)
	size := len(body) - lenStart

	var lenBuf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(lenBuf[:], uint64(size))

	body = append(body, lenBuf[:n]...)          // grow by the varint's width
	copy(body[lenStart+n:], body[lenStart:])    // shift the body right
	copy(body[lenStart:lenStart+n], lenBuf[:n]) // write the length in front
	return body
}

// estimateSize guesses the encoded size, to size the buffer once up front.
// Being wrong only costs a realloc.
func estimateSize(series []timeSeries) int {
	total := 0
	for i := range series {
		total += 8 // the series' own tag and length
		for _, l := range series[i].Labels {
			total += len(l.Name) + len(l.Value) + 8
		}
		total += len(series[i].Samples) * 20
	}
	return total
}
