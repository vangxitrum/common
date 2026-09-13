// Package password stores passwords the way OWASP's Password Storage cheat
// sheet and NIST SP 800-63B say to: argon2id with a per-password salt, at a
// cost an attacker with the database has to pay again for every guess.
//
// Four services hash passwords today, each its own way: bcrypt at assorted
// costs, one with no length check, one that returns early when the account
// does not exist and so tells anyone which emails have accounts. This package
// is the one way.
//
//	stored, err := hasher.Hash(ctx, pw)          // on signup and password change, after Check
//	result, err := hasher.Verify(ctx, pw, stored) // on login
//
// # What Verify guarantees
//
// Every path costs one derivation. An account that does not exist is checked
// against a dummy hash (pass "" as stored), so the response takes as long as
// a wrong password. The key comparison is constant-time.
//
// Hashes from before this package - bcrypt, $2a$, $2b$ or $2y$ - verify
// against the input exactly as it was hashed, and report NeedsRehash, as do
// argon2id hashes made with other parameters. Rehash on that login, when the
// plain password is in hand:
//
//	UPDATE users SET password = $new WHERE id = $id AND password = $old
//
// The WHERE on the old hash makes it a compare-and-swap: two concurrent
// logins cannot overwrite a password changed in between.
//
// # Characters, not bytes
//
// Passwords are normalized to NFC before hashing, so é typed as one code point
// or as e plus a combining accent is the same password. Lengths count code
// points after NFC. The minimum is 15, NIST's for a password that is the only
// factor; there are no composition rules and no expiry, which NIST forbids.
//
// # What this package does not do
//
// It does not check passwords against breach corpora, which NIST requires
// (the HIBP k-anonymity range API is the planned route, through httpclient).
// It uses no pepper: there is no secret store to keep one apart from the
// database. And it does not limit guessing - put ratelimit in front of the
// login, before the user lookup.
package password

import (
	"context"
	"crypto/subtle"
	"errors"
	"runtime"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/text/unicode/norm"

	"gitlab.internal/tuan.quang.tran/aioz-common/memory"
	"gitlab.internal/tuan.quang.tran/aioz-common/random"
)

// Config sets the argon2id cost and the length policy. The zero value is the
// defaults.
type Config struct {
	Memory        memory.Size `help:"argon2id memory per hash; OWASP's floor is 19MiB at 2 iterations, 46MiB at 1"             default:"19MiB"`
	Iterations    int         `help:"argon2id passes over memory"                                                              default:"2"`
	Parallelism   int         `help:"argon2id lanes"                                                                           default:"1"`
	MaxConcurrent int         `help:"hashes computed at once, each holding memory; 0 is GOMAXPROCS"                            default:"0"`
	MinLength     int         `help:"shortest password Check accepts, in characters; 15 is NIST's minimum for a single factor" default:"15"`
	MaxLength     int         `help:"longest password accepted, in characters"                                                 default:"256"`
}

const (
	defaultMemory      = 19 * memory.MiB
	defaultIterations  = 2
	defaultParallelism = 1
	defaultMinLength   = 15
	defaultMaxLength   = 256

	saltLen = 16
	keyLen  = 32

	// maxBcryptCost bounds a stored legacy hash. Cost 31 is two billion
	// rounds: a tampered row would hold a slot for hours.
	maxBcryptCost = 16
)

// floorKiB is OWASP's argon2id memory for a number of iterations: each pair
// is an equal defense. Beyond 5 iterations the floor stays at 5's.
func floorKiB(iterations int) uint32 {
	switch iterations {
	case 1:
		return 47104
	case 2:
		return 19456
	case 3:
		return 12288
	case 4:
		return 9216
	default:
		return 7168
	}
}

// Hasher hashes and verifies passwords. It is safe for concurrent use.
type Hasher struct {
	memory    uint32 // KiB
	time      uint32
	threads   uint8
	minLength int
	maxLength int

	slots chan struct{}
	dummy phc

	// The two derivations, as fields so a test can count them.
	argon2id func(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte
	bcrypt   func(hash, password []byte) error
}

// Result is the outcome of Verify.
type Result struct {
	// OK is true when the password matches.
	OK bool
	// NeedsRehash is true when the password matches a hash that is legacy
	// bcrypt or argon2id with other parameters: Hash the password again and
	// store it.
	NeedsRehash bool
}

// New returns a Hasher, refusing parameters below OWASP's argon2id floor. It
// computes one hash, the dummy that stands in for accounts that do not exist.
func New(config Config) (*Hasher, error) {
	if config.Memory == 0 {
		config.Memory = defaultMemory
	}
	if config.Iterations == 0 {
		config.Iterations = defaultIterations
	}
	if config.Parallelism == 0 {
		config.Parallelism = defaultParallelism
	}
	if config.MinLength == 0 {
		config.MinLength = defaultMinLength
	}
	if config.MaxLength == 0 {
		config.MaxLength = defaultMaxLength
	}

	switch {
	case config.Memory < 0 || config.Memory%memory.KiB != 0:
		return nil, Error.Errorf(
			"memory %s: want a positive whole number of KiB",
			config.Memory,
		)
	case config.Memory > maxMemoryKiB*memory.KiB:
		return nil, Error.Errorf(
			"memory %s: more than 1GiB per hash",
			config.Memory,
		)
	case config.Iterations < 1 || config.Iterations > maxIterations:
		return nil, Error.Errorf(
			"iterations %d: want 1 to %d",
			config.Iterations,
			maxIterations,
		)
	case config.Parallelism < 1 || config.Parallelism > 255:
		return nil, Error.Errorf(
			"parallelism %d: want 1 to 255",
			config.Parallelism,
		)
	case config.MaxConcurrent < 0:
		return nil, Error.Errorf(
			"max concurrent %d: must not be negative",
			config.MaxConcurrent,
		)
	case config.MinLength < 8:
		return nil, Error.Errorf(
			"min length %d: NIST's floor is 8",
			config.MinLength,
		)
	case config.MaxLength < 64:
		return nil, Error.Errorf(
			"max length %d: NIST requires at least 64",
			config.MaxLength,
		)
	case config.MinLength > config.MaxLength:
		return nil, Error.Errorf(
			"min length %d is over max length %d",
			config.MinLength,
			config.MaxLength,
		)
	}

	kib := uint32(config.Memory / memory.KiB)
	if floor := floorKiB(config.Iterations); kib < floor {
		return nil, Error.Errorf(
			"memory %d KiB at %d iterations: below OWASP's floor of %d KiB",
			kib,
			config.Iterations,
			floor,
		)
	}

	concurrent := config.MaxConcurrent
	if concurrent == 0 {
		concurrent = runtime.GOMAXPROCS(0)
	}

	h := &Hasher{
		memory:    kib,
		time:      uint32(config.Iterations),
		threads:   uint8(config.Parallelism),
		minLength: config.MinLength,
		maxLength: config.MaxLength,
		slots:     make(chan struct{}, concurrent),
		argon2id:  argon2.IDKey,
		bcrypt:    bcrypt.CompareHashAndPassword,
	}
	h.dummy = h.derive(random.Bytes(32), random.Bytes(saltLen))
	return h, nil
}

// Hash returns the PHC string to store for password. Call Check first: Hash
// refuses only what is too long to be worth deriving.
func (h *Hasher) Hash(
	ctx context.Context,
	password string,
) (_ string, err error) {
	defer mon.Task()(&ctx)(&err)

	normalized, err := h.normalize(password)
	if err != nil {
		return "", err
	}
	release, err := h.acquire(ctx)
	if err != nil {
		return "", err
	}
	defer release()

	return h.derive([]byte(normalized), random.Bytes(saltLen)).encode(), nil
}

// Verify checks password against stored, which is "" for an account that
// does not exist. A mismatch is a Result that is not OK, never an error; an
// error means the stored hash is malformed, the password is over MaxLength,
// or ctx ended while waiting for a slot.
func (h *Hasher) Verify(
	ctx context.Context,
	password, stored string,
) (_ Result, err error) {
	defer mon.Task()(&ctx)(&err)

	normalized, err := h.normalize(password)
	if err != nil {
		return Result{}, err
	}

	switch {
	case stored == "":
		// No such account. The dummy costs what a real hash costs, so the
		// answer takes as long as a wrong password would.
		_, err := h.verifyArgon2(ctx, normalized, h.dummy)
		return Result{}, err

	case strings.HasPrefix(stored, "$argon2id$"):
		p, err := decodePHC(stored)
		if err != nil {
			return Result{}, err
		}
		ok, err := h.verifyArgon2(ctx, normalized, p)
		if err != nil || !ok {
			return Result{}, err
		}
		return Result{OK: true, NeedsRehash: !h.current(p)}, nil

	case isBcrypt(stored):
		// The raw input, not the normalized one: that is what was hashed.
		return h.verifyBcrypt(ctx, password, stored)

	default:
		return Result{}, Error.Errorf("malformed hash: unrecognized format")
	}
}

// Check applies the length policy to a new password, on signup and on
// change - never on login, or a policy change locks existing users out.
func (h *Hasher) Check(password string) error {
	if !utf8.ValidString(password) {
		return Error.Errorf("not valid UTF-8")
	}
	normalized, err := h.normalize(password)
	if err != nil {
		return err
	}
	if n := utf8.RuneCountInString(normalized); n < h.minLength {
		return Error.Errorf(
			"%w: %d characters, want at least %d",
			ErrTooShort,
			n,
			h.minLength,
		)
	}
	return nil
}

// normalize returns the NFC form of password, refusing one over MaxLength
// characters. The byte check first keeps a megabyte of input from being
// normalized at all: no character is more than 4 bytes.
func (h *Hasher) normalize(password string) (string, error) {
	tooLong := Error.Errorf(
		"%w: want at most %d characters",
		ErrTooLong,
		h.maxLength,
	)
	if len(password) > 4*h.maxLength {
		return "", tooLong
	}
	normalized := norm.NFC.String(password)
	if utf8.RuneCountInString(normalized) > h.maxLength {
		return "", tooLong
	}
	return normalized, nil
}

func (h *Hasher) derive(password, salt []byte) phc {
	return phc{
		memory:  h.memory,
		time:    h.time,
		threads: h.threads,
		salt:    salt,
		key: h.argon2id(
			password,
			salt,
			h.time,
			h.memory,
			h.threads,
			keyLen,
		),
	}
}

// verifyArgon2 derives with the stored parameters and compares in constant
// time. constant_time_test.go reads this function.
func (h *Hasher) verifyArgon2(
	ctx context.Context,
	password string,
	p phc,
) (bool, error) {
	release, err := h.acquire(ctx)
	if err != nil {
		return false, err
	}
	defer release()

	key := h.argon2id(
		[]byte(password),
		p.salt,
		p.time,
		p.memory,
		p.threads,
		uint32(len(p.key)),
	)
	return subtle.ConstantTimeCompare(key, p.key) == 1, nil
}

func (h *Hasher) verifyBcrypt(
	ctx context.Context,
	password, stored string,
) (Result, error) {
	cost, err := bcrypt.Cost([]byte(stored))
	if err != nil {
		return Result{}, Error.Errorf("malformed hash: %w", err)
	}
	if cost > maxBcryptCost {
		return Result{}, Error.Errorf(
			"malformed hash: bcrypt cost %d over %d",
			cost,
			maxBcryptCost,
		)
	}

	release, err := h.acquire(ctx)
	if err != nil {
		return Result{}, err
	}
	defer release()

	// bcrypt reads at most 72 bytes of the password, which is how the hash
	// was made; CompareHashAndPassword does the same.
	switch err := h.bcrypt([]byte(stored), []byte(password)); {
	case err == nil:
		return Result{OK: true, NeedsRehash: true}, nil
	case errors.Is(err, bcrypt.ErrMismatchedHashAndPassword):
		return Result{}, nil
	default:
		return Result{}, Error.Errorf("malformed hash: %w", err)
	}
}

// current reports whether p was made with this Hasher's parameters.
func (h *Hasher) current(p phc) bool {
	return p.memory == h.memory && p.time == h.time && p.threads == h.threads &&
		len(p.salt) == saltLen && len(p.key) == keyLen
}

func isBcrypt(stored string) bool {
	return len(stored) > 4 && stored[0] == '$' && stored[1] == '2' &&
		(stored[2] == 'a' || stored[2] == 'b' || stored[2] == 'y') && stored[3] == '$'
}

// acquire takes a derivation slot. A context already done is refused even if
// a slot is free, so a caller that has gone away never starts a derivation.
func (h *Hasher) acquire(ctx context.Context) (release func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, Error.Wrap(err)
	}
	select {
	case h.slots <- struct{}{}:
		return func() { <-h.slots }, nil
	case <-ctx.Done():
		return nil, Error.Wrap(ctx.Err())
	}
}
