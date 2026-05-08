//go:build !linux

package debug

import (
	"context"
	"sync/atomic"
)

func capturePackets(ctx context.Context, stop *atomic.Bool) {}
