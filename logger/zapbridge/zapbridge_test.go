package zapbridge_test

import (
	"context"
	"log/slog"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/errs/v2"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"gitlab.internal/tuan.quang.tran/aioz-common/logger/zapbridge"
)

var testError = errs.Tag("test")

// recorder is a slog.Handler that keeps every record it is handed, with the
// attributes it was built with, so a test can read what the bridge produced.
type recorder struct {
	mu      *sync.Mutex
	records *[]record
	attrs   []slog.Attr
}

type record struct {
	slog.Record
	preAttrs []slog.Attr
}

func newRecorder() *recorder {
	return &recorder{mu: &sync.Mutex{}, records: &[]record{}}
}

func (r *recorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *recorder) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	*r.records = append(*r.records, record{Record: rec, preAttrs: r.attrs})
	return nil
}

func (r *recorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *r
	clone.attrs = append(append([]slog.Attr{}, r.attrs...), attrs...)
	return &clone
}

func (r *recorder) WithGroup(string) slog.Handler { return r }

func (r *recorder) all() []record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]record{}, *r.records...)
}

func (rec record) attrMap(t *testing.T) map[string]slog.Value {
	t.Helper()
	out := map[string]slog.Value{}
	for _, a := range rec.preAttrs {
		out[a.Key] = a.Value
	}
	rec.Attrs(func(a slog.Attr) bool {
		out[a.Key] = a.Value
		return true
	})
	return out
}

func TestCoreForwardsEntry(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	log := zap.New(zapbridge.Core(rec, zapcore.DebugLevel))

	log.Warn("disk almost full", zap.String("path", "/data"), zap.Int("free", 12))

	records := rec.all()
	require.Len(t, records, 1)
	require.Equal(t, "disk almost full", records[0].Message)
	require.Equal(t, slog.LevelWarn, records[0].Level)

	attrs := records[0].attrMap(t)
	require.Equal(t, "/data", attrs["path"].String())
	require.Equal(t, int64(12), attrs["free"].Int64())
}

// The entry's own timestamp is what the record carries. A handler that
// buffers records - the async one does - would otherwise stamp them with the
// time they were drained, not the time they happened.
func TestCoreKeepsTheEntryTime(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	when := time.Date(2026, 9, 19, 10, 30, 0, 0, time.UTC)
	core := zapbridge.Core(rec, zapcore.DebugLevel)

	require.NoError(t, core.Write(zapcore.Entry{
		Level:   zapcore.InfoLevel,
		Time:    when,
		Message: "started",
	}, nil))

	records := rec.all()
	require.Len(t, records, 1)
	require.True(t, when.Equal(records[0].Time), "got %v", records[0].Time)
}

// zap.Error must arrive as the error itself. The logger package takes the
// error value to derive its stack, so an error flattened to a string leaves
// error.stack_trace empty for every line.
func TestCoreKeepsTheErrorValue(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	log := zap.New(zapbridge.Core(rec, zapcore.DebugLevel))
	cause := testError.Errorf("boom")

	log.Error("upload failed", zap.Error(cause))

	attrs := rec.all()[0].attrMap(t)
	got, ok := attrs["error"].Any().(error)
	require.True(t, ok, "error attribute is %T", attrs["error"].Any())
	require.Equal(t, cause, got)
	require.NotContains(t, attrs, "errorVerbose")
}

// The component name a process names its loggers with is the most useful
// search dimension it has, so it is an attribute rather than a prefix lost in
// the message.
func TestCoreCarriesTheLoggerName(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	log := zap.New(zapbridge.Core(rec, zapcore.DebugLevel)).Named("piecestore")

	log.Info("ready")

	require.Equal(t, "piecestore", rec.all()[0].attrMap(t)["logger"].String())
}

func TestCoreCarriesTheCaller(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	log := zap.New(zapbridge.Core(rec, zapcore.DebugLevel), zap.AddCaller())

	log.Info("ready")

	records := rec.all()
	require.NotZero(t, records[0].PC)
	frame, _ := runtime.CallersFrames([]uintptr{records[0].PC}).Next()
	require.Contains(t, frame.Function, "TestCoreCarriesTheCaller")
}

func TestCoreWithDoesNotShareBetweenSiblings(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	log := zap.New(zapbridge.Core(rec, zapcore.DebugLevel)).With(zap.String("node", "n1"))

	log.With(zap.String("branch", "left")).Info("one")
	log.With(zap.String("branch", "right")).Info("two")

	records := rec.all()
	require.Len(t, records, 2)
	require.Equal(t, "n1", records[0].attrMap(t)["node"].String())
	require.Equal(t, "left", records[0].attrMap(t)["branch"].String())
	require.Equal(t, "right", records[1].attrMap(t)["branch"].String())
}

func TestCoreRespectsTheLevelEnabler(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	log := zap.New(zapbridge.Core(rec, zapcore.WarnLevel))

	log.Info("quiet")
	log.Error("loud")

	records := rec.all()
	require.Len(t, records, 1)
	require.Equal(t, "loud", records[0].Message)
}

// Tee adds a destination; it does not replace the one the logger has.
func TestTeeKeepsTheOriginalDestination(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	core, logs := observed()
	log := zapbridge.Tee(zap.New(core), rec, zapcore.DebugLevel)

	log.Info("both")

	require.Len(t, rec.all(), 1)
	require.Equal(t, 1, logs.Len())
}

// The reverse direction: a package that takes a *slog.Logger can be given the
// process's zap logger, so its lines get the same format, level and sinks as
// everything else.
func TestHandlerWritesIntoZap(t *testing.T) {
	t.Parallel()

	core, logs := observed()
	log := slog.New(zapbridge.Handler(zap.New(core)))

	log.Info("listening", "addr", ":7777")

	require.Equal(t, 1, logs.Len())
	require.Equal(t, "listening", logs.All()[0].Message)
	require.Equal(t, zapcore.InfoLevel, logs.All()[0].Level)
	require.Equal(t, ":7777", logs.All()[0].ContextMap()["addr"])
}

func TestHandlerKeepsTheRecordTime(t *testing.T) {
	t.Parallel()

	core, logs := observed()
	when := time.Date(2026, 9, 19, 10, 30, 0, 0, time.UTC)
	h := zapbridge.Handler(zap.New(core))

	require.NoError(t, h.Handle(
		context.Background(),
		slog.NewRecord(when, slog.LevelInfo, "started", 0),
	))

	require.True(t, when.Equal(logs.All()[0].Time), "got %v", logs.All()[0].Time)
}

func TestHandlerFlattensGroups(t *testing.T) {
	t.Parallel()

	core, logs := observed()
	log := slog.New(zapbridge.Handler(zap.New(core))).
		With("node", "n1").
		WithGroup("piece").
		With("id", "abc")

	log.Info("stored", slog.Int("size", 42))

	fields := logs.All()[0].ContextMap()
	require.Equal(t, "n1", fields["node"])
	require.Equal(t, "abc", fields["piece.id"])
	require.Equal(t, int64(42), fields["piece.size"])
}

func TestHandlerRespectsTheZapLevel(t *testing.T) {
	t.Parallel()

	core, logs := observedAt(zapcore.WarnLevel)
	log := slog.New(zapbridge.Handler(zap.New(core)))

	require.False(t, log.Enabled(context.Background(), slog.LevelInfo))
	require.True(t, log.Enabled(context.Background(), slog.LevelError))

	log.Info("quiet")
	log.Error("loud")

	require.Equal(t, 1, logs.Len())
}

// An error crossing back into zap stays an error, so the console and the
// sinks on the other side can still read its stack.
func TestHandlerKeepsTheErrorValue(t *testing.T) {
	t.Parallel()

	core, logs := observed()
	log := slog.New(zapbridge.Handler(zap.New(core)))
	cause := testError.Errorf("boom")

	log.Error("failed", "error", cause)

	// Read as a field, not through ContextMap: that encodes an error to its
	// message, which is the very thing being checked against.
	fields := logs.All()[0].Context
	require.Len(t, fields, 1)
	require.Equal(t, "error", fields[0].Key)
	require.Equal(t, zapcore.ErrorType, fields[0].Type)
	require.Equal(t, cause, fields[0].Interface)
}

// zap has three levels above Error and slog has none, so they all arrive as
// Error rather than as a level nothing is configured for.
func TestCoreMapsEveryLevel(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		zap  zapcore.Level
		slog slog.Level
	}{
		{zapcore.DebugLevel, slog.LevelDebug},
		{zapcore.InfoLevel, slog.LevelInfo},
		{zapcore.WarnLevel, slog.LevelWarn},
		{zapcore.ErrorLevel, slog.LevelError},
		{zapcore.DPanicLevel, slog.LevelError},
		{zapcore.PanicLevel, slog.LevelError},
		{zapcore.FatalLevel, slog.LevelError},
	} {
		rec := newRecorder()
		core := zapbridge.Core(rec, zapcore.DebugLevel)

		require.NoError(t, core.Write(zapcore.Entry{Level: tc.zap, Message: "x"}, nil))

		require.Equal(t, tc.slog, rec.all()[0].Level, "zap level %s", tc.zap)
	}
}

// A zap stack trace, which zap.AddStacktrace adds as a field of the entry
// rather than of the fields, keeps the key the console prints as a block.
func TestCoreCarriesTheStack(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	core := zapbridge.Core(rec, zapcore.DebugLevel)

	require.NoError(t, core.Write(zapcore.Entry{
		Level:   zapcore.ErrorLevel,
		Message: "boom",
		Stack:   "goroutine 1 [running]",
	}, nil))

	require.Equal(t, "goroutine 1 [running]", rec.all()[0].attrMap(t)["stacktrace"].String())
}

// With the Core enabled at Debug, as it is meant to be, the handler is the
// one deciding a level, so it has to be asked.
func TestCoreRespectsTheHandlerLevel(t *testing.T) {
	t.Parallel()

	rec := newRecorder()
	log := zap.New(zapbridge.Core(&leveled{Handler: rec, min: slog.LevelWarn}, zapcore.DebugLevel))

	log.Info("quiet")
	log.Warn("loud")

	records := rec.all()
	require.Len(t, records, 1)
	require.Equal(t, "loud", records[0].Message)
}

// leveled is a handler that declines records below min, as a configured one
// does.
type leveled struct {
	slog.Handler
	min slog.Level
}

func (l *leveled) Enabled(_ context.Context, level slog.Level) bool {
	return level >= l.min
}
