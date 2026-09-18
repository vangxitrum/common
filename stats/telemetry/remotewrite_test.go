package telemetry

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// decodeWriteRequest parses the wire format produced by marshalWriteRequest.
// It is a deliberately independent implementation: the encoder is hand-written
// precisely so the package does not depend on generated protobuf code, so a
// round trip through the same helper would prove nothing. This decoder is
// written against the protobuf spec, not against the encoder.
func decodeWriteRequest(t *testing.T, data []byte) []timeSeries {
	t.Helper()

	var out []timeSeries
	forEachField(t, data, func(field int, wire byte, payload []byte) {
		require.Equal(t, 1, field, "WriteRequest has only field 1")
		require.Equal(t, byte(2), wire)
		out = append(out, decodeTimeSeries(t, payload))
	})
	return out
}

func decodeTimeSeries(t *testing.T, data []byte) timeSeries {
	t.Helper()

	var ts timeSeries
	forEachField(t, data, func(field int, wire byte, payload []byte) {
		require.Equal(t, byte(2), wire)
		switch field {
		case 1:
			ts.Labels = append(ts.Labels, decodeLabel(t, payload))
		case 2:
			ts.Samples = append(ts.Samples, decodeSample(t, payload))
		default:
			t.Fatalf("unexpected TimeSeries field %d", field)
		}
	})
	return ts
}

func decodeLabel(t *testing.T, data []byte) label {
	t.Helper()

	var l label
	forEachField(t, data, func(field int, wire byte, payload []byte) {
		require.Equal(t, byte(2), wire)
		switch field {
		case 1:
			l.Name = string(payload)
		case 2:
			l.Value = string(payload)
		default:
			t.Fatalf("unexpected Label field %d", field)
		}
	})
	return l
}

func decodeSample(t *testing.T, data []byte) sample {
	t.Helper()

	var s sample
	for len(data) > 0 {
		tag, n := binary.Uvarint(data)
		require.Positive(t, n)
		data = data[n:]
		field, wire := int(tag>>3), byte(tag&7)

		switch {
		case field == 1 && wire == 1:
			s.Value = math.Float64frombits(binary.LittleEndian.Uint64(data))
			data = data[8:]
		case field == 2 && wire == 0:
			v, n := binary.Uvarint(data)
			require.Positive(t, n)
			s.Timestamp = int64(v)
			data = data[n:]
		default:
			t.Fatalf("unexpected Sample field %d wire %d", field, wire)
		}
	}
	return s
}

// forEachField walks length-delimited fields, which is every field the
// encoder emits outside Sample.
func forEachField(t *testing.T, data []byte, fn func(field int, wire byte, payload []byte)) {
	t.Helper()

	for len(data) > 0 {
		tag, n := binary.Uvarint(data)
		require.Positive(t, n, "truncated tag")
		data = data[n:]

		field, wire := int(tag>>3), byte(tag&7)
		require.Equal(t, byte(2), wire, "only length-delimited fields expected here")

		size, n := binary.Uvarint(data)
		require.Positive(t, n, "truncated length")
		data = data[n:]
		require.LessOrEqual(t, int(size), len(data), "length runs past the buffer")

		fn(field, wire, data[:size])
		data = data[size:]
	}
}

func TestMarshalWriteRequest_RoundTrip(t *testing.T) {
	in := []timeSeries{
		{
			Labels: []label{
				{Name: "__name__", Value: "requests_total"},
				{Name: "app", Value: "backend"},
				{Name: "field", Value: "count"},
			},
			Samples: []sample{{Value: 42.5, Timestamp: 1700000000000}},
		},
		{
			Labels:  []label{{Name: "__name__", Value: "gauge"}},
			Samples: []sample{{Value: -1.25, Timestamp: 1700000000001}},
		},
	}

	got := decodeWriteRequest(t, marshalWriteRequest(in))
	require.Equal(t, in, got)
}

func TestMarshalWriteRequest_ZeroValueOmitsTheField(t *testing.T) {
	// proto3 omits zero scalars, as prompb's own generated marshaler does,
	// and a decoder has to read the field back as 0.
	in := []timeSeries{{
		Labels:  []label{{Name: "__name__", Value: "idle"}},
		Samples: []sample{{Value: 0, Timestamp: 1700000000000}},
	}}

	encoded := marshalWriteRequest(in)
	got := decodeWriteRequest(t, encoded)
	require.Equal(t, in, got)

	// And the zero really is absent from the wire, not encoded as 8 bytes.
	withValue := marshalWriteRequest([]timeSeries{{
		Labels:  in[0].Labels,
		Samples: []sample{{Value: 1, Timestamp: 1700000000000}},
	}})
	require.Equal(t, 9, len(withValue)-len(encoded), "tag plus 8 bytes of float")
}

func TestMarshalWriteRequest_LargeMessageLengthPrefix(t *testing.T) {
	// A series whose encoded body exceeds 127 bytes needs a multi-byte length
	// varint, which is where the shift-right in appendMessageField earns its
	// keep.
	long := make([]byte, 300)
	for i := range long {
		long[i] = 'a'
	}
	in := []timeSeries{{
		Labels:  []label{{Name: "__name__", Value: string(long)}},
		Samples: []sample{{Value: 1, Timestamp: 1700000000000}},
	}}

	require.Equal(t, in, decodeWriteRequest(t, marshalWriteRequest(in)))
}

func TestMarshalWriteRequest_Empty(t *testing.T) {
	require.Empty(t, marshalWriteRequest(nil))
}
