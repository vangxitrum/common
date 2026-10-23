package logger

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"gitlab.internal/tuan.quang.tran/aioz-common/logger/logtest"
)

// components returns every component=... value in the console output, in
// order, with its coloring stripped.
func components(out string) []string {
	var found []string
	for _, field := range strings.Fields(stripANSI(out)) {
		if v, ok := strings.CutPrefix(field, "component="); ok {
			found = append(found, v)
		}
	}
	return found
}

// Named names a logger by its component attribute.
func TestNamedSetsTheComponent(t *testing.T) {
	var buf bytes.Buffer
	log, h := newComponentTestLogger(t, &buf, slog.LevelInfo, nil)

	Named(log, "crawler").Info("hello")
	require.NoError(t, h.Shutdown(context.Background()))

	require.Equal(t, []string{"crawler"}, components(buf.String()))
}

// A name under a name is joined to it with a dot, as a nested named logger is,
// and the line carries the full name once rather than every level of it.
func TestNamedJoinsANestedName(t *testing.T) {
	var buf bytes.Buffer
	log, h := newComponentTestLogger(t, &buf, slog.LevelInfo, nil)

	crawler := Named(log, "crawler")
	Named(crawler.With("feed", "x"), "noaa").Info("hello")
	crawler.Info("parent")
	require.NoError(t, h.Shutdown(context.Background()))

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 2)
	require.Equal(t, []string{"crawler.noaa"}, components(lines[0]))
	require.Contains(
		t,
		lines[0],
		"feed=x",
		"attributes added between the names are kept",
	)
	require.Equal(
		t,
		[]string{"crawler"},
		components(lines[1]),
		"naming a child leaves the parent alone",
	)
}

// A component set by With is a name like any other: Named joins to it.
func TestNamedJoinsAComponentSetByWith(t *testing.T) {
	var buf bytes.Buffer
	log, h := newComponentTestLogger(t, &buf, slog.LevelInfo, nil)

	Named(log.With("component", "crawler"), "noaa").Info("hello")
	require.NoError(t, h.Shutdown(context.Background()))

	require.Equal(t, []string{"crawler.noaa"}, components(buf.String()))
}

// With("component", ...) keeps its meaning: it replaces the name rather than
// joining it.
func TestWithComponentReplacesTheName(t *testing.T) {
	var buf bytes.Buffer
	log, h := newComponentTestLogger(t, &buf, slog.LevelInfo, nil)

	Named(log, "crawler").With("component", "grpc").Info("hello")
	require.NoError(t, h.Shutdown(context.Background()))

	require.Equal(t, []string{"grpc"}, components(buf.String()))
}

// A component on the record is innermost of all, and is the only one written.
func TestARecordComponentWinsOverTheName(t *testing.T) {
	var buf bytes.Buffer
	log, h := newComponentTestLogger(t, &buf, slog.LevelInfo, nil)

	Named(log, "crawler").Info("hello", "component", "router")
	require.NoError(t, h.Shutdown(context.Background()))

	require.Equal(t, []string{"router"}, components(buf.String()))
}

// A name set before a group stays the logger's name: it is not moved into the
// group by the attributes that follow.
func TestANameSetBeforeAGroupStaysAtTheTop(t *testing.T) {
	var buf bytes.Buffer
	log, h := newComponentTestLogger(t, &buf, slog.LevelInfo, nil)

	Named(log, "crawler").WithGroup("req").Info("hello", "id", 7)
	require.NoError(t, h.Shutdown(context.Background()))

	out := buf.String()
	require.Equal(t, []string{"crawler"}, components(out))
	require.Contains(t, out, "req.id=7")
}

// Inside a group, "component" names something else, so Named there is an
// ordinary attribute of that group rather than the logger's name.
func TestNamedInsideAGroupIsAnOrdinaryAttribute(t *testing.T) {
	var buf bytes.Buffer
	log, h := newComponentTestLogger(t, &buf, slog.LevelInfo, nil)

	Named(Named(log, "crawler").WithGroup("req"), "noaa").Info("hello")
	require.NoError(t, h.Shutdown(context.Background()))

	out := buf.String()
	require.Equal(t, []string{"crawler"}, components(out))
	require.Contains(t, out, "req.component=noaa")
}

// An empty name leaves the logger as it was.
func TestNamedWithAnEmptyNameIsANoOp(t *testing.T) {
	log, _ := logtest.NewRecorder()
	require.Same(t, log, Named(log, ""))
}

// On a handler that is not this package's, Named still names the logger, but
// cannot see the parent's name to join to.
func TestNamedOnAnotherHandlerSetsTheComponent(t *testing.T) {
	log, rec := logtest.NewRecorder()

	Named(Named(log, "crawler"), "noaa").Info("hello")

	all := rec.All()
	require.Len(t, all, 1)
	require.Equal(t, "noaa", all[0].Attrs["component"])
}

// ComponentLevels matches the full name, and a name it does not list falls
// back to its parent's entry, so one setting covers a whole subtree.
func TestComponentLevelFallsBackToTheParentName(t *testing.T) {
	var buf bytes.Buffer
	log, h := newComponentTestLogger(
		t,
		&buf,
		slog.LevelDebug,
		map[string]slog.Level{
			"crawler":      slog.LevelWarn,
			"crawler.grpc": slog.LevelDebug,
		},
	)

	crawler := Named(log, "crawler")
	Named(crawler, "noaa").Info("noaa chatter")
	Named(crawler, "noaa").Warn("noaa problem")
	Named(crawler, "grpc").Debug("grpc detail")
	crawler.Info("record names a child", "component", "crawler.noaa")
	require.NoError(t, h.Shutdown(context.Background()))

	out := buf.String()
	require.NotContains(
		t,
		out,
		"noaa chatter",
		"crawler.noaa is held to crawler's level",
	)
	require.Contains(t, out, "noaa problem")
	require.Contains(
		t,
		out,
		"grpc detail",
		"an entry for the full name beats the parent's",
	)
	require.NotContains(
		t,
		out,
		"record names a child",
		"a record component falls back the same way",
	)
}

// The name reaches Elasticsearch as log.logger, the ECS field for it, without
// the service having to declare it.
func TestNamedIsIndexedAsLogLogger(t *testing.T) {
	recv := newBulkRecorder(t)
	cfg := testESConfig(recv.URL)
	cfg.BatchSize = 1
	h, err := New(Config{Output: &bytes.Buffer{}, Elasticsearch: &cfg})
	require.NoError(t, err)

	Named(Named(slog.New(h), "crawler"), "noaa").Info("hello")
	require.NoError(t, h.Shutdown(context.Background()))

	require.Eventually(t, func() bool {
		_, _, docs := recv.snapshot()
		return len(docs) == 1
	}, time.Second, 10*time.Millisecond)
	_, _, docs := recv.snapshot()
	doc := docs[0]

	require.Equal(t, "crawler.noaa", doc["log"].(map[string]any)["logger"])
	require.NotContains(
		t,
		doc,
		"labels",
		"the name is not also written as a label",
	)
	require.NotContains(t, doc["log"], "extra")
}
