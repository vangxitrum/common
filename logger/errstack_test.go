package logger

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/errs/v2"
	"golang.org/x/sync/errgroup"

	"gitlab.internal/tuan.quang.tran/aioz-common/apierr"
	"gitlab.internal/tuan.quang.tran/aioz-common/errs2"
)

var userError = errs.Tag("user")

const failure = `user: relation "refresh_tokens" does not exist`

func failing() error {
	return userError.Wrap(
		errors.New(`relation "refresh_tokens" does not exist`),
	)
}

// zeebo/errs's error has no exported fields, so without the conversion the
// JSON output and every sink got {} for it.
func TestAnErrorIsLoggedAsItsMessageWithItsStack(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	sink := &recordingSink{}
	slog.New(testConsole(&buf, nil, sink)).
		Error("chore failed", "err", failing())

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	require.Equal(t, failure, line["err"])
	require.Contains(t, line[stackKey], "aioz-common/logger.failing:")

	require.Len(t, sink.lines, 1)
	require.Equal(t, failure, sink.lines[0]["err"])
	require.Equal(t, line[stackKey], sink.lines[0][stackKey])
}

// The text console prints the message on the line and the stack as a block
// below it, never the escaped %+v that slog.TextHandler writes.
func TestTextPrintsTheStackBelowTheLine(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	console := testConsole(&buf, nil)
	console.format = FormatText
	slog.New(console).Error("chore failed", "err", failing())

	out := buf.String()
	head, _, ok := strings.Cut(out, "\n")
	require.True(t, ok)
	require.Contains(t, head, "err="+failure)
	require.NotContains(t, out, `\n\t`)
	require.Regexp(t, `\n  stack:\n    aioz-common/logger\.failing:\d+\n`, out)
}

// apierr.Error is a LogValuer that resolves to its message, so its cause's
// stack is taken before the value is resolved.
func TestAnAPIErrorKeepsItsCausesStack(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	slog.New(testConsole(&buf, nil)).
		Error("request failed", "err", apierr.Internal("db", failing()))

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	require.IsType(t, "", line["err"])
	require.Contains(t, line[stackKey], "aioz-common/logger.failing:")
}

func TestAPlainErrorHasNoStack(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	slog.New(testConsole(&buf, nil)).Error("failed", "err", errors.New("plain"))

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	require.Equal(t, "plain", line["err"])
	require.NotContains(t, line, stackKey)
}

// Critical's call-site stack, which has file paths, wins over the error's.
func TestCriticalKeepsItsOwnStack(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	Critical(slog.New(testConsole(&buf, nil)), "lost", "err", failing())

	var line map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &line))
	require.Equal(t, failure, line["err"])
	require.Contains(t, line[stackKey], "errstack_test.go:")
}

// The aioz-stream symptom, reproduced on a plain slog.TextHandler, and gone
// with ErrorAttr.
func TestErrorAttrStopsTheTextHandlerDumpingTheStack(t *testing.T) {
	t.Parallel()

	var before, after bytes.Buffer
	slog.New(slog.NewTextHandler(&before, nil)).
		Error("failed", "err", failing())
	slog.New(slog.NewTextHandler(&after, &slog.HandlerOptions{ReplaceAttr: ErrorAttr})).
		Error("failed", "err", failing())

	require.Contains(t, before.String(), `\n\t`)
	require.NotContains(t, after.String(), `\n\t`)
	require.Contains(
		t,
		after.String(),
		`err="user: relation \"refresh_tokens\" does not exist"`,
	)
}

func TestErrorAttrLeavesOtherValuesAlone(t *testing.T) {
	t.Parallel()

	for _, a := range []slog.Attr{slog.String("k", "v"), slog.Int("n", 1), slog.Any("s", struct{ A int }{1})} {
		require.True(t, a.Equal(ErrorAttr(nil, a)), a.Key)
	}
}

func TestErrorStackDropsPlumbing(t *testing.T) {
	t.Parallel()

	var err error
	var group errgroup.Group
	group.Go(func() error {
		err = failing()
		return nil
	})
	require.NoError(t, group.Wait())

	require.Contains(t, strings.Join(errs2.Frames(err), "\n"), "errgroup")
	stack := ErrorStack(err)
	require.NotEmpty(t, stack)
	require.NotContains(t, strings.Join(stack, "\n"), "errgroup")
}

func TestTrimFrame(t *testing.T) {
	t.Parallel()

	for frame, want := range map[string]string{
		"10.0.0.50/tuan.quang.tran/vms-v2/internal/app/user.(*Service).DeleteExpiredRefreshTokens:254": "vms-v2/internal/app/user.(*Service).DeleteExpiredRefreshTokens:254",
		"gitlab.internal/tuan.quang.tran/aioz-common/cycle.(*Cycle).Run:84":                            "aioz-common/cycle.(*Cycle).Run:84",
		"main.(*Service).Delete:30":     "main.(*Service).Delete:30",
		"net/http.(*conn).serve:2092":   "net/http.(*conn).serve:2092",
		"example.com/pkg.Func:1":        "example.com/pkg.Func:1",
		"gopkg.in/yaml.v3.Unmarshal:10": "gopkg.in/yaml.v3.Unmarshal:10",
	} {
		require.Equal(t, want, trimFrame(frame), frame)
	}
}

func TestIsPlumbing(t *testing.T) {
	t.Parallel()

	for frame, want := range map[string]bool{
		"runtime.goexit:1700":                             true,
		"runtime/pprof.Do:51":                             true,
		"golang.org/x/sync/errgroup.(*Group).Go.func1:93": true,
		"gitlab.internal/tuan.quang.tran/aioz-common/lifecycle.(*Group).Run.func1:110": true,
		"gitlab.internal/tuan.quang.tran/aioz-common/cycle.(*Cycle).Run:84":            false,
		"10.0.0.50/tuan.quang.tran/vms-v2/internal/utils/chore.(*Chore).Run:76":        false,
	} {
		require.Equal(t, want, isPlumbing(frame), frame)
	}
}

func TestFormatStack(t *testing.T) {
	t.Parallel()

	require.Empty(t, FormatStack(nil))
	require.Equal(
		t,
		"\n  stack:\n    a:1\n    b:2",
		FormatStack([]string{"a:1", "b:2"}),
	)
}
