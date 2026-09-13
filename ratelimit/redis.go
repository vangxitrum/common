package ratelimit

import (
	"context"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"gitlab.internal/tuan.quang.tran/aioz-common/redis"
)

// gcraScript is step over every window, then commit only if all allowed, in
// one atomic call. KEYS are the windows; ARGV[1] is now, then interval and
// limit for each window, all in microseconds. It returns {wait, denied index}
// with a 0-based index, or {0, -1}.
//
// string.format('%d') writes the TAT: tostring would switch to exponent
// notation for numbers this size and lose the low digits.
const gcraScript = `
local now = tonumber(ARGV[1])
local next = {}
local wait, denied = 0, -1
for i = 1, #KEYS do
  local interval = tonumber(ARGV[2 * i])
  local limit = tonumber(ARGV[2 * i + 1])
  local tat = tonumber(redis.call('GET', KEYS[i]) or now)
  if tat < now then tat = now end
  next[i] = tat + interval
  local allow_at = next[i] - limit
  if allow_at > now and allow_at - now > wait then
    wait, denied = allow_at - now, i - 1
  end
end
if denied >= 0 then
  return {wait, denied}
end
for i = 1, #KEYS do
  redis.call('SET', KEYS[i], string.format('%d', next[i]), 'PX', math.ceil((next[i] - now) / 1000))
end
return {0, -1}
`

// redisStore is the GCRA in Redis. A key expires when its window would be
// full again, so Redis holds nothing for a client that stopped trying.
type redisStore struct {
	client *redis.Client
	script *goredis.Script
}

func newRedisStore(client *redis.Client) *redisStore {
	return &redisStore{client: client, script: goredis.NewScript(gcraScript)}
}

// allow runs the script bounded by timeout. go-redis applies a context
// deadline to socket reads only when ContextTimeoutEnabled is set, which Open
// does not do, so a server that accepts and never answers would hold the
// caller for the whole read timeout on every retry. The call runs in its own
// goroutine instead, and the caller stops waiting at timeout; the goroutine
// ends at the socket's own timeout.
func (s *redisStore) allow(
	ctx context.Context,
	timeout time.Duration,
	now int64,
	windows []window,
) (decision, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	keys := make([]string, len(windows))
	args := make([]any, 0, 1+2*len(windows))
	args = append(args, now)
	for i, w := range windows {
		keys[i] = w.key
		args = append(args, w.interval, w.limit)
	}

	type reply struct {
		values []int64
		err    error
	}
	done := make(chan reply, 1)
	go func() {
		values, err := s.script.Run(ctx, s.client, keys, args...).Int64Slice()
		done <- reply{values, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			return decision{}, r.err
		}
		if len(r.values) != 2 || r.values[1] < -1 ||
			r.values[1] >= int64(len(windows)) {
			return decision{}, Error.Errorf(
				"unexpected script reply %v",
				r.values,
			)
		}
		return decision{wait: r.values[0], denied: int(r.values[1])}, nil
	case <-ctx.Done():
		return decision{}, ctx.Err()
	}
}

func (s *redisStore) reset(
	ctx context.Context,
	timeout time.Duration,
	keys []string,
) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- s.client.Del(ctx, keys...).Err() }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
