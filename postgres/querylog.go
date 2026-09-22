package postgres

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// maxStatementLen caps a logged statement. A generated query - an IN list with
// a thousand placeholders, a migration - is not made more readable by its tail,
// and a log line is not the place to store one.
const maxStatementLen = 2048

// truncated marks a statement that was cut, so nobody reads a prefix as the
// whole query.
const truncated = " ...[truncated]"

// sqlKey carries the statement from TraceQueryStart to TraceQueryEnd.
// pgx.TraceQueryEndData has the CommandTag and the error but not the SQL.
type sqlKey struct{}

func withSQL(ctx context.Context, sql string) context.Context {
	return context.WithValue(ctx, sqlKey{}, sql)
}

func sqlOf(ctx context.Context) string {
	sql, _ := ctx.Value(sqlKey{}).(string)
	return sql
}

// wantsQueryLog reports whether anything would be logged for this config, so
// the tracer is installed for logging as well as for metrics.
func (config Config) wantsQueryLog() bool {
	return config.LogQueries || config.SlowQuery > 0
}

// logQuery writes the line for one finished query, or nothing.
//
// Three cases, in order: a failure is an error whatever it took, a query at or
// over SlowQuery is a warning, and LogQueries makes the rest debug. A slow
// query is therefore reported once, as a warning, rather than twice.
//
// The statement is logged; its arguments never are. pgx sends parameters
// separately, so the statement text is the parameterized form - "WHERE email =
// $1" - and the value that would identify a person stays out of the log. That
// is the whole reason this logs what it logs.
func (t *tracer) logQuery(
	ctx context.Context,
	duration time.Duration,
	tag pgconn.CommandTag,
	err error,
) {
	if t.log == nil {
		return
	}

	level := slog.LevelDebug
	switch {
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		// ErrNoRows is an empty result, not a failure.
		level = slog.LevelError
	case t.slowQuery > 0 && duration >= t.slowQuery:
		level = slog.LevelWarn
	case !t.logQueries:
		return
	}

	statement := cleanStatement(sqlOf(ctx))

	attrs := []any{
		slog.String("group", "db"),
		slog.String("name", string(t.name.Val)),
		slog.String("statement", statement),
		slog.String("operation", operationFromSQL(statement)),
		slog.Duration("duration", duration),
	}
	if op := operationOf(ctx); op != "" {
		attrs = append(attrs, slog.String("op", op))
	}
	if err != nil {
		attrs = append(attrs, slog.Any("err", err))
	} else {
		attrs = append(attrs, slog.Int64("rows", tag.RowsAffected()))
	}
	if level == slog.LevelWarn {
		attrs = append(attrs, slog.Duration("threshold", t.slowQuery))
	}

	t.log.LogAttrs(ctx, level, queryMessage(level), attrsOf(attrs)...)
}

func queryMessage(level slog.Level) string {
	switch level {
	case slog.LevelError:
		return "query failed"
	case slog.LevelWarn:
		return "slow query"
	default:
		return "query"
	}
}

// attrsOf narrows the []any built above to the []slog.Attr LogAttrs takes.
func attrsOf(args []any) []slog.Attr {
	out := make([]slog.Attr, 0, len(args))
	for _, a := range args {
		if attr, ok := a.(slog.Attr); ok {
			out = append(out, attr)
		}
	}
	return out
}

// cleanStatement makes a Go raw-string query one line and bounds it. A query
// written across fifteen indented lines otherwise becomes fifteen log lines,
// which no log store and no reader thanks you for.
func cleanStatement(sql string) string {
	statement := strings.Join(strings.Fields(sql), " ")
	if len(statement) > maxStatementLen {
		statement = statement[:maxStatementLen] + truncated
	}
	return statement
}

// operationFromSQL is the leading keyword, upper-cased: SELECT, INSERT, UPDATE,
// DELETE, and so on. It is a dimension worth splitting a dashboard by, which
// the statement itself is not.
func operationFromSQL(statement string) string {
	word, _, _ := strings.Cut(statement, " ")
	return strings.ToUpper(word)
}
