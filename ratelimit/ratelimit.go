// Package ratelimit limits how often one identity - an account, an address,
// an email - may do one thing, across every replica of a service.
//
// Most of our limiters today are a map in one process: each replica keeps its
// own count, so four replicas allow four times the limit, and a restart
// forgets everyone. This one keeps its state in Redis, falls back to memory
// when Redis is down, and answers every request with an exact Retry-After.
//
// # A delay, not a lockout
//
// A denied request consumes nothing. An attacker hammering a victim's account
// cannot push the victim's refill further out, so there is no lockout to hand
// out: a long window (20 per hour) behind a short one (5 per minute) is the
// progressive delay OWASP's Credential Stuffing and Denial of Service cheat
// sheets ask for.
//
// # Keys
//
// A Key is a Rule and a value. The value is hashed at once and never stored
// or logged; Rule.IPKey limits an IPv6 client by its /64. Build the address
// with the clientip package: X-Forwarded-For read any other way lets a client
// pick the address it is limited by.
//
// # What this package does not do
//
// It limits; it does not decide what a denial looks like. The caller answers
// 429 with RetryAfterSeconds, and 503 when Allow returns an error.
//
// It needs a single Redis node: the windows of one call can live in different
// Cluster slots. And it needs a Redis that does not evict: every limiter key
// has a TTL, so allkeys-* and volatile-* policies alike drop counts under
// memory pressure. Run it with noeviction, apart from any cache that needs
// eviction.
package ratelimit

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/spacemonkeygo/monkit/v3"

	"gitlab.internal/tuan.quang.tran/aioz-common/redis"
	"gitlab.internal/tuan.quang.tran/aioz-common/time2"
)

// What Allow does when Redis fails or does not answer within Timeout.
const (
	// OnErrorLocal limits in this replica's memory until Redis is back:
	// protection continues, at the replica count times the limit.
	OnErrorLocal = "local"
	// OnErrorOpen allows every request: availability over protection.
	OnErrorOpen = "open"
	// OnErrorClosed denies with an error, which the caller answers with 503.
	OnErrorClosed = "closed"
)

// Config describes one Limiter. The zero value is the defaults.
type Config struct {
	Prefix   string        `help:"namespace for this service's keys in a shared redis; give each service its own unless they should share one budget"                                                    default:"ratelimit"`
	OnError  string        `help:"when redis fails: local (limit in this replica's memory), open (allow), or closed (deny with an error)" default:"local"`
	Timeout  time.Duration `help:"bound on each redis call; past it the on-error mode applies"                                            default:"200ms"`
	Capacity int           `help:"windows the in-memory store and the local fallback remember; the least recently used are forgotten"     default:"100000"`
}

const (
	defaultPrefix   = "ratelimit"
	defaultTimeout  = 200 * time.Millisecond
	defaultCapacity = 100000
)

// Limiter checks and counts requests. It is safe for concurrent use.
type Limiter struct {
	log     *slog.Logger
	config  Config
	memory  *memoryStore
	redis   *redisStore // nil: memory only
	failing atomic.Bool
}

// Option configures New.
type Option func(*options)

type options struct {
	redis    *redis.Client
	redisSet bool
}

// WithRedis keeps the counts in Redis, shared by every replica - and every
// service - that uses the same server and Prefix. Without it the Limiter
// counts in this process's memory, which is right for a single replica or a
// test.
func WithRedis(client *redis.Client) Option {
	return func(o *options) { o.redis, o.redisSet = client, true }
}

// New returns a Limiter: in memory with no options, in Redis with WithRedis.
func New(log *slog.Logger, config Config, opts ...Option) (*Limiter, error) {
	if log == nil {
		return nil, Error.Errorf("logger is nil")
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if o.redisSet && o.redis == nil {
		return nil, Error.Errorf("WithRedis: client is nil; leave the option out for the memory store")
	}

	if config.Prefix == "" {
		config.Prefix = defaultPrefix
	}
	if err := validatePrefix(config.Prefix); err != nil {
		return nil, err
	}
	switch config.OnError {
	case "":
		config.OnError = OnErrorLocal
	case OnErrorLocal, OnErrorOpen, OnErrorClosed:
	default:
		return nil, Error.Errorf("on-error %q: want local, open or closed", config.OnError)
	}
	switch {
	case config.Timeout < 0:
		return nil, Error.Errorf("timeout must not be negative")
	case config.Timeout == 0:
		config.Timeout = defaultTimeout
	}
	switch {
	case config.Capacity < 0:
		return nil, Error.Errorf("capacity must not be negative")
	case config.Capacity == 0:
		config.Capacity = defaultCapacity
	}

	l := &Limiter{
		log:    log,
		config: config,
		memory: newMemoryStore(config.Capacity),
	}
	if o.redis != nil {
		l.redis = newRedisStore(o.redis)
	}
	return l, nil
}

// validatePrefix keeps the prefix to characters that cannot open a Redis
// Cluster hash tag or split a key.
func validatePrefix(prefix string) error {
	if len(prefix) > 32 {
		return Error.Errorf("prefix %q: longer than 32 characters", prefix)
	}
	for _, c := range []byte(prefix) {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' ||
			c == '-' ||
			c == '_' ||
			c == '.'
		if !ok {
			return Error.Errorf(
				"prefix %q: want only letters, digits, - _ and .",
				prefix,
			)
		}
	}
	return nil
}

// Allow counts one request against every key, if every window of every key
// allows it. When any denies, nothing is counted anywhere, and RetryAfter is
// the longest wait.
//
// An error means no answer: an invalid key, or a Redis failure under
// OnErrorClosed. The Result is then not Allowed.
func (l *Limiter) Allow(
	ctx context.Context,
	keys ...Key,
) (result Result, err error) {
	defer mon.Task()(&ctx)(&err)

	windows, err := l.windows(keys)
	if err != nil {
		return Result{}, err
	}
	now := time2.Now(ctx).UnixMicro()

	d, err := l.decide(ctx, now, windows)
	if err != nil {
		return Result{}, err
	}

	if d.denied >= 0 {
		mon.Counter("ratelimit_denied", monkit.NewSeriesTag("rule", windows[d.denied].rule)).
			Inc(1)
		return Result{RetryAfter: time.Duration(d.wait) * time.Microsecond}, nil
	}
	for _, key := range keys {
		mon.Counter("ratelimit_allowed", monkit.NewSeriesTag("rule", key.rule)).
			Inc(1)
	}
	return Result{Allowed: true}, nil
}

func (l *Limiter) decide(
	ctx context.Context,
	now int64,
	windows []window,
) (decision, error) {
	if l.redis == nil {
		return l.memory.allow(now, windows), nil
	}

	d, err := l.redis.allow(ctx, l.config.Timeout, now, windows)
	if err == nil {
		l.recovered()
		return d, nil
	}
	if ctx.Err() != nil {
		// The caller gave up; that says nothing about Redis.
		return decision{}, Error.Wrap(ctx.Err())
	}

	l.failed(err)
	switch l.config.OnError {
	case OnErrorOpen:
		return allowed, nil
	case OnErrorClosed:
		return decision{}, Error.Errorf("store unavailable: %w", err)
	default:
		return l.memory.allow(now, windows), nil
	}
}

// Reset forgets every window of the keys, as after a successful login: reset
// the account's keys then, never the address's, or an attacker with one valid
// account resets the limit on the address they attack from.
func (l *Limiter) Reset(ctx context.Context, keys ...Key) (err error) {
	defer mon.Task()(&ctx)(&err)

	windows, err := l.windows(keys)
	if err != nil {
		return err
	}
	names := make([]string, len(windows))
	for i, w := range windows {
		names[i] = w.key
	}

	// The fallback may hold counts from an outage; they are forgiven too.
	l.memory.reset(names)
	if l.redis == nil {
		return nil
	}
	if err := l.redis.reset(ctx, l.config.Timeout, names); err != nil {
		return Error.Errorf("reset: %w", err)
	}
	return nil
}

// windows validates the keys and lists one window per rate, each key string
// once.
func (l *Limiter) windows(keys []Key) ([]window, error) {
	if len(keys) == 0 {
		return nil, Error.Errorf("no keys")
	}

	var windows []window
	seen := make(map[string]bool)
	for _, key := range keys {
		if err := key.validate(); err != nil {
			return nil, err
		}
		for _, rate := range key.rates {
			name := l.config.Prefix + ":{" + key.rule + ":" + key.digest + "}:" + rate.String()
			if seen[name] {
				continue
			}
			seen[name] = true
			interval := rate.interval()
			windows = append(windows, window{
				key:      name,
				rule:     key.rule,
				interval: interval,
				limit:    interval * int64(rate.Count),
			})
		}
	}
	return windows, nil
}

// failed logs a Redis failure once per outage, not once per request: a login
// storm during an outage would otherwise be a log storm too. The error names
// the server and the failure, never a key.
func (l *Limiter) failed(err error) {
	mon.Counter("ratelimit_store_failed", monkit.NewSeriesTag("mode", l.config.OnError)).
		Inc(1)
	if l.failing.CompareAndSwap(false, true) {
		l.log.Warn(
			"rate limit store failed; applying on-error mode until it answers",
			slog.String("on_error", l.config.OnError),
			slog.String("error", err.Error()),
		)
	}
}

func (l *Limiter) recovered() {
	if l.failing.CompareAndSwap(true, false) {
		l.log.Info("rate limit store answering again")
	}
}
