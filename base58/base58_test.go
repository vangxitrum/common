package base58

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEncodeDecodeRoundTrip covers the raw codec across the cases that break
// naive base58 implementations.
//
// Leading zero bytes are the classic one: base58 is a big-integer encoding, so
// a leading 0x00 carries no numeric weight and is only preserved because the
// encoder emits a '1' per leading zero byte. An implementation that dropped
// them would round-trip most inputs correctly and silently corrupt the rest.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input []byte
	}{
		{"empty", []byte{}},
		{"single zero byte", []byte{0x00}},
		{"several leading zeros", []byte{0x00, 0x00, 0x00, 0x2a}},
		{"single byte", []byte{0x2a}},
		{"all zeros", bytes.Repeat([]byte{0x00}, 8)},
		{"max bytes", bytes.Repeat([]byte{0xff}, 8)},
		{"ascii", []byte("hello depin")},
		{"32 bytes", bytes.Repeat([]byte{0xab, 0xcd}, 16)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded := Encode(tc.input)
			require.Equal(
				t, tc.input, Decode(encoded),
				"encoded as %q", encoded,
			)
		})
	}
}

// TestEncodePreservesLeadingZeros pins the '1'-per-leading-zero rule directly,
// so a regression names the cause rather than just failing a round trip.
func TestEncodePreservesLeadingZeros(t *testing.T) {
	for zeros := range 5 {
		input := append(bytes.Repeat([]byte{0x00}, zeros), 0x2a)
		encoded := Encode(input)

		require.Equal(
			t, zeros, len(encoded)-len(strings.TrimLeft(encoded, "1")),
			"each leading zero byte must encode as one '1': %q", encoded,
		)
	}
}

// TestDecodeRejectsInvalidCharacters covers the alphabet guard. base58 omits
// 0, O, I and l precisely because they are visually ambiguous, so accepting
// them would let a mistyped token decode to different bytes.
func TestDecodeRejectsInvalidCharacters(t *testing.T) {
	for _, invalid := range []string{"0", "O", "I", "l", "hello world", "+/="} {
		require.Empty(
			t, Decode(invalid),
			"%q is not valid base58 and must not decode", invalid,
		)
	}
}

// TestCheckEncodeDecodeRoundTrip covers the checksummed form, which is what
// certificate/authorization uses for signing-service auth tokens.
func TestCheckEncodeDecodeRoundTrip(t *testing.T) {
	payload := []byte("an auth token payload")

	for _, version := range []byte{0, 1, 42, 255} {
		encoded := CheckEncode(payload, version)

		decoded, gotVersion, err := CheckDecode(encoded)
		require.NoError(t, err)
		require.Equal(t, payload, decoded)
		require.Equal(
			t, version, gotVersion,
			"the version byte must survive; it selects how the token is read",
		)
	}
}

// TestCheckDecodeRejectsTamperedInput is the reason the checksummed form
// exists. An auth token is attacker-supplied, so a corrupted or forged one must
// be rejected rather than decoded into plausible bytes.
func TestCheckDecodeRejectsTamperedInput(t *testing.T) {
	encoded := CheckEncode([]byte("payload"), 1)

	t.Run("flipped character fails the checksum", func(t *testing.T) {
		// Swap one character for another valid base58 character, so the failure
		// comes from the checksum rather than the alphabet guard.
		runes := []rune(encoded)
		if runes[0] == 'z' {
			runes[0] = 'y'
		} else {
			runes[0] = 'z'
		}

		_, _, err := CheckDecode(string(runes))
		require.ErrorIs(t, err, ErrChecksum)
	})

	t.Run("truncated input is rejected", func(t *testing.T) {
		_, _, err := CheckDecode(encoded[:3])
		require.Error(t, err)
	})

	t.Run("empty input is rejected", func(t *testing.T) {
		_, _, err := CheckDecode("")
		require.ErrorIs(t, err, ErrInvalidFormat)
	})

	t.Run("non-base58 input is rejected", func(t *testing.T) {
		_, _, err := CheckDecode("!!!not base58!!!")
		require.Error(t, err)
	})
}

// TestCheckEncodeIsDeterministic guards against a checksum that varies between
// calls, which would make previously issued tokens unverifiable.
func TestCheckEncodeIsDeterministic(t *testing.T) {
	payload := []byte("stable")
	require.Equal(t, CheckEncode(payload, 7), CheckEncode(payload, 7))

	require.NotEqual(
		t, CheckEncode(payload, 7), CheckEncode(payload, 8),
		"the version byte is covered by the checksum",
	)
}
