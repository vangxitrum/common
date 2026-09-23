package logger

import "log/slog"

// ErrorAttr is a ReplaceAttr that logs an error as its message.
//
// Without it, slog.TextHandler prints an error with %+v, and a zeebo/errs
// error then appends its whole stack, which the handler quotes onto the same
// line with every newline escaped. The handler in this package logs an error
// as its message itself and needs no ReplaceAttr for it.
func ErrorAttr(_ []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindAny {
		if err, ok := a.Value.Any().(error); ok {
			return slog.String(a.Key, err.Error())
		}
	}
	return a
}
