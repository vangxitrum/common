// Package monitor wires process and runtime statistics into a monkit registry,
// so the debug server's /metrics reports how the process itself is doing and
// not only what the application counts.
//
// Without it a freshly started service serves an empty /metrics: monkit only
// knows the series some package has already incremented, which before the
// first request is none. Registering here adds goroutines, memory, GC, rusage,
// open file descriptors and uptime from the moment the process starts.
package monitor

import (
	"sync"

	"github.com/spacemonkeygo/monkit/v3"
	"github.com/spacemonkeygo/monkit/v3/environment"
)

var (
	// registerMu and registered guard environment.Register per registry.
	//
	// environment.Register is not idempotent: each call chains its stat
	// sources onto the registry again, so registering twice double-counts
	// every process series. Keyed by registry rather than a single sync.Once
	// because production uses monkit.Default while tests construct their own,
	// and each of those deserves the stats exactly once.
	registerMu sync.Mutex
	registered = map[*monkit.Registry]bool{}
)

// Register attaches monkit's process and runtime statistics to registry,
// unless they are already attached to that exact registry. A nil registry
// means monkit.Default, matching environment.Register.
//
// The series land under monkit's top-level "env" scope and are named
// goroutines, runtime_memstats, runtime_gcstats, rusage, fds and process
// (which carries uptime) - note they are not prefixed "environment_".
func Register(registry *monkit.Registry) {
	if registry == nil {
		registry = monkit.Default
	}

	registerMu.Lock()
	defer registerMu.Unlock()

	if registered[registry] {
		return
	}
	environment.Register(registry)
	registered[registry] = true
}
