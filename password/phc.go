package password

import (
	"encoding/base64"
	"strconv"
	"strings"
)

// Bounds on a stored hash. They are not a floor - a weak hash still verifies,
// so its owner can log in once and be upgraded - but a ceiling, so a tampered
// row cannot make one Verify allocate gigabytes or run for hours.
const (
	maxMemoryKiB  = 1 << 20 // 1 GiB
	maxIterations = 64
	minSaltLen    = 8
	maxSaltLen    = 64
	minKeyLen     = 16
	maxKeyLen     = 64
)

// Strict rejects non-zero padding bits, so one key has one encoding.
var b64 = base64.RawStdEncoding.Strict()

// phc is an argon2id hash in the PHC string format:
// $argon2id$v=19$m=19456,t=2,p=1$<salt>$<key>, base64 without padding.
type phc struct {
	memory  uint32 // KiB
	time    uint32
	threads uint8
	salt    []byte
	key     []byte
}

func (p phc) encode() string {
	return "$argon2id$v=19" +
		"$m=" + strconv.FormatUint(uint64(p.memory), 10) +
		",t=" + strconv.FormatUint(uint64(p.time), 10) +
		",p=" + strconv.FormatUint(uint64(p.threads), 10) +
		"$" + b64.EncodeToString(p.salt) +
		"$" + b64.EncodeToString(p.key)
}

// decodePHC accepts exactly what encode produces: argon2id only, version 19,
// m, t and p in that order with no leading zeros or signs, and canonical
// base64. Anything laxer is a second spelling of a hash, and a second spelling
// is somewhere for a tampered row to hide. The errors never quote the hash.
func decodePHC(stored string) (phc, error) {
	// The base64 decoder skips CR and LF, which would make them invisible.
	if strings.ContainsAny(stored, "\r\n") {
		return phc{}, Error.Errorf("malformed hash: line break")
	}
	rest, ok := strings.CutPrefix(stored, "$argon2id$v=19$")
	if !ok {
		return phc{}, Error.Errorf("malformed hash: want $argon2id$v=19$")
	}
	fields := strings.Split(rest, "$")
	if len(fields) != 3 {
		return phc{}, Error.Errorf(
			"malformed hash: want parameters, salt and key",
		)
	}
	params := strings.Split(fields[0], ",")
	if len(params) != 3 {
		return phc{}, Error.Errorf("malformed hash: want m, t and p")
	}

	var p phc
	var err error
	if p.memory, err = param(params[0], "m", maxMemoryKiB); err != nil {
		return phc{}, err
	}
	if p.time, err = param(params[1], "t", maxIterations); err != nil {
		return phc{}, err
	}
	threads, err := param(params[2], "p", 255)
	if err != nil {
		return phc{}, err
	}
	p.threads = uint8(threads)

	if p.salt, err = b64.DecodeString(fields[1]); err != nil {
		return phc{}, Error.Errorf("malformed hash: salt is not base64")
	}
	if len(p.salt) < minSaltLen || len(p.salt) > maxSaltLen {
		return phc{}, Error.Errorf(
			"malformed hash: salt of %d bytes",
			len(p.salt),
		)
	}
	if p.key, err = b64.DecodeString(fields[2]); err != nil {
		return phc{}, Error.Errorf("malformed hash: key is not base64")
	}
	if len(p.key) < minKeyLen || len(p.key) > maxKeyLen {
		return phc{}, Error.Errorf(
			"malformed hash: key of %d bytes",
			len(p.key),
		)
	}
	return p, nil
}

// param reads name=<decimal> with a value from 1 to max.
func param(field, name string, max uint64) (uint32, error) {
	value, ok := strings.CutPrefix(field, name+"=")
	if !ok || value == "" || value[0] < '1' || value[0] > '9' {
		return 0, Error.Errorf("malformed hash: parameter %s", name)
	}
	n, err := strconv.ParseUint(value, 10, 32)
	if err != nil || n > max {
		return 0, Error.Errorf(
			"malformed hash: parameter %s out of range",
			name,
		)
	}
	return uint32(n), nil
}
