package redis

import (
	"context"
	"errors"
	"net"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/spacemonkeygo/monkit/v3"
)

// hook records command latency and errors for one client.
//
// The values are created once per client rather than looked up per command:
// Scope.DurationVal takes a lock and a map lookup, and this runs on every
// redis call there is.
type hook struct {
	name     monkit.SeriesTag
	duration *monkit.DurationVal
	pipeline *monkit.DurationVal
	pipeLen  *monkit.IntVal
}

func newHook(name monkit.SeriesTag) *hook {
	return &hook{
		name:     name,
		duration: mon.DurationVal("redis_command", name),
		pipeline: mon.DurationVal("redis_pipeline", name),
		pipeLen:  mon.IntVal("redis_pipeline_length", name),
	}
}

// DialHook counts failures to establish a connection, which is the one redis
// failure that says the server is gone rather than the command was wrong.
func (h *hook) DialHook(next goredis.DialHook) goredis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := next(ctx, network, addr)
		if err != nil {
			mon.Event("redis_dial_failed", h.name)
		}
		return conn, err
	}
}

// ProcessHook times one command.
//
// redis.Nil is not an error here. It is how a GET reports a key that is not
// there, which is the normal outcome of a cache lookup; counting it as an
// error makes the error rate meaningless, and that is the mistake every
// hand-rolled counter makes.
//
// The command name tags the failure event and never the histogram. Tagging the
// histogram would put a locked map lookup in Scope.DurationVal on every single
// redis call, to produce a per-command latency breakdown nobody reads. The
// per-command error breakdown is the one worth having, and errors are rare.
func (h *hook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmd)
		h.duration.Observe(time.Since(start))

		if err != nil && !errors.Is(err, goredis.Nil) {
			mon.Event("redis_command_failed", h.name,
				monkit.NewSeriesTag("cmd", cmd.Name()))
		}
		return err
	}
}

// ProcessPipelineHook times one pipeline round trip.
//
// A pipeline that fired one sample per member command would drown the command
// histogram - TxPipelined sends dozens at a time - so it gets its own series
// plus a length distribution, and the member commands are not timed
// individually.
func (h *hook) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmds)
		h.pipeline.Observe(time.Since(start))
		h.pipeLen.Observe(int64(len(cmds)))

		if err != nil && !errors.Is(err, goredis.Nil) {
			mon.Event("redis_pipeline_failed", h.name)
		}
		return err
	}
}
