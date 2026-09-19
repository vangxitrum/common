package zapbridge_test

import (
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// observed returns a Core recording everything at or above level, and the log
// it records into.
func observed() (zapcore.Core, *observer.ObservedLogs) {
	return observedAt(zapcore.DebugLevel)
}

func observedAt(level zapcore.Level) (zapcore.Core, *observer.ObservedLogs) {
	return observer.New(level)
}
