# password

Passwords stored the way OWASP's Password Storage cheat sheet and NIST SP
800-63B say to: argon2id with a per-password salt, at a cost an attacker with
the database pays again for every guess.

```go
import "gitlab.internal/tuan.quang.tran/aioz-common/password"
```

Four services hash passwords today, each its own way: bcrypt at assorted
costs, one with no length check, one that returns early when the account does
not exist and so tells anyone which emails have accounts. This package is the
one way.

## Usage

```go
hasher, err := password.New(cfg.Password) // computes one dummy hash; do it once
if err != nil {
    return err
}
```

**Signup and password change:**

```go
if err := hasher.Check(form.Password); err != nil {
    // errors.Is(err, password.ErrTooShort) / ErrTooLong: a 400 with the message
}
stored, err := hasher.Hash(ctx, form.Password)
```

**Login** - after the rate limit, and without returning early for an account
that does not exist:

```go
user, err := users.ByEmail(ctx, email)
stored := ""
if err == nil {
    stored = user.PasswordHash
} else if !errors.Is(err, sql.ErrNoRows) {
    return err
}

result, err := hasher.Verify(ctx, form.Password, stored) // "" checks a dummy hash
if err != nil {
    return err // a malformed stored hash, or the context ended
}
if !result.OK || user == nil {
    // the same 401, and the same time taken, whether or not the account exists
}
if result.NeedsRehash {
    if fresh, err := hasher.Hash(ctx, form.Password); err == nil {
        // compare-and-swap: a password changed in between is not overwritten
        _, _ = db.Exec(ctx, `UPDATE users SET password = $1 WHERE id = $2 AND password = $3`,
            fresh, user.ID, stored)
    }
}
```

```yaml
password:
  memory: 19MiB        # argon2id memory per hash
  iterations: 2
  parallelism: 1
  max-concurrent: 0    # hashes at once; 0 is GOMAXPROCS
  min-length: 15
  max-length: 256
```

## What it guarantees

- **One derivation on every path.** An unknown account is checked against a
  dummy hash, so the response takes as long as a wrong password. aioz-map's
  `SignIn` returning before the hash is the bug this closes.
- **Constant-time comparison** of the derived key. A test reads the source to
  hold it there.
- **No parameters below OWASP's floor.** `New` refuses anything under these
  argon2id pairs, each an equal defense:

  | Memory | Iterations |
  | --- | --- |
  | 46 MiB (47104 KiB) | 1 |
  | 19 MiB (19456 KiB) | 2 (the default) |
  | 12 MiB | 3 |
  | 9 MiB | 4 |
  | 7 MiB | 5 or more |

- **Bounded memory.** Each derivation holds `memory`; at most `max-concurrent`
  run at once and the rest wait (honoring the context), so a burst of logins
  queues instead of getting the process OOM-killed. Budget
  `max-concurrent x memory` in the container's limit.
- **Upgrades on login.** A legacy bcrypt hash (`$2a$`, `$2b$`, `$2y$`) or an
  argon2id hash made with other parameters verifies and reports
  `NeedsRehash`. Weak hashes are not refused - their owners could never log in
  to be upgraded - but a stored hash is capped (1 GiB, 64 iterations, bcrypt
  cost 16) so a tampered row cannot stall the server.
- **A strict decoder.** A stored hash is accepted only in exactly the form
  `Hash` writes; a fuzz test holds decode and encode to a round trip.

## Characters, not bytes

Passwords are normalized to **NFC** before hashing: `é` typed as one code
point or as `e` plus a combining accent is the same password. This is
permanent for hashes made here - the PHC string cannot record the form - which
is why it is decided once, in this package.

Lengths count code points after NFC, so an emoji (4 bytes) is one character.
The minimum is **15**, NIST's for a password that is the only factor. There are
no composition rules ("one digit, one symbol") and no expiry: NIST forbids
both. Spaces and every printable character are allowed.

`Check` runs on signup and change only, never on login: raising the minimum
must not lock out the users who already have a shorter password.

Legacy bcrypt hashes verify against the **raw** input, because that is what
they hashed, including bcrypt's own 72-byte cut.

Signup forms across our services check for 8 characters today. Adopting this
package means moving them to 15 in the same change.

## What it does not do

- **Breached-password screening.** NIST requires checking new passwords
  against known breach corpora. The planned route is the HIBP range API
  (k-anonymity: only 5 hex characters of the SHA-1 leave the service) through
  `httpclient`; until then this requirement is unmet.
- **A pepper.** There is no secret store that keeps one apart from the
  database, and a pepper in the same place as the hashes adds nothing.
- **Rate limiting.** Put [`ratelimit`](../ratelimit) in front of the login,
  before the user lookup.

## API

```go
type Config struct {
    Memory        memory.Size
    Iterations    int
    Parallelism   int
    MaxConcurrent int
    MinLength     int
    MaxLength     int
}

func New(config Config) (*Hasher, error)
func (h *Hasher) Hash(ctx context.Context, password string) (string, error)
func (h *Hasher) Verify(ctx context.Context, password, stored string) (Result, error)
func (h *Hasher) Check(password string) error

type Result struct{ OK, NeedsRehash bool }

var ErrTooShort, ErrTooLong error
var Error = errs.Tag("password")
```
