package depin

import (
	"log/slog"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"gitlab.internal/tuan.quang.tran/aioz-common/logger/zapbridge"
)

// This is the one file in this package that imports zap, and .golangci.yml's
// enforce-slog rule excludes it by name for that reason. The depin go-sdk
// offers exactly one logging hook, WithLogger(*zap.Logger), so either its logs
// are bridged here or they are thrown away. They are worth keeping: the SDK
// reports its retries, its stalled pieces and its per-transfer summary through
// that logger, and those lines explain a slow download.

// newSDKLogger returns the *zap.Logger the SDK is constructed with, writing
// into log.
//
// It lives here rather than at the call site so that this stays the only file
// in the package naming zap, which is what lets the enforce-slog depguard rule
// be waived for one file instead of the whole package. The caller takes the
// result with := and hands it straight to the SDK, so it never names the type.
//
// The core is enabled at DebugLevel so the caller's slog handler is the single
// place a level is decided. Filtering in both would mean the debug server's
// /logging endpoint could not turn the SDK's lines back on at runtime.
func newSDKLogger(log *slog.Logger) *zap.Logger {
	return zap.New(zapbridge.Core(log.Handler(), zapcore.DebugLevel))
}
