package password

import "sync/atomic"

// Instrument wraps h's derivations so a test can count them and see the most
// that ran at once. Call it before h is shared.
func Instrument(h *Hasher) (count, peak *atomic.Int64) {
	count, peak = new(atomic.Int64), new(atomic.Int64)
	var active atomic.Int64
	enter := func() func() {
		count.Add(1)
		n := active.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		return func() { active.Add(-1) }
	}

	argon2id, bcrypt := h.argon2id, h.bcrypt
	h.argon2id = func(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte {
		defer enter()()
		return argon2id(password, salt, time, memory, threads, keyLen)
	}
	h.bcrypt = func(hash, password []byte) error {
		defer enter()()
		return bcrypt(hash, password)
	}
	return count, peak
}

// RoundTrip decodes a PHC string and encodes it again.
func RoundTrip(stored string) (string, error) {
	p, err := decodePHC(stored)
	if err != nil {
		return "", err
	}
	return p.encode(), nil
}
