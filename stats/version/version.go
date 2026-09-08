// Package version is the former home of the build stamp.
//
// Deprecated: use gitlab.internal/tuan.quang.tran/aioz-common/version. The
// package moved to the top level when it grew version comparison and a metrics
// source, neither of which is a stats concern. Everything here forwards to the
// new package, so a value produced by one is the same value as the other.
package version

import (
	v "gitlab.internal/tuan.quang.tran/aioz-common/version"
)

// Info is the build stamp.
//
// Deprecated: use version.Info.
type Info = v.Info

// SemVer is a comparable semantic version.
//
// Deprecated: use version.SemVer.
type SemVer = v.SemVer

// Build is the stamp of the running binary.
//
// Deprecated: use version.Build. This is a distinct variable initialized from
// the same source, not an alias, because Go has no variable aliases. Read it;
// do not assign to it and expect the new package to see the change.
var Build = v.Build

// IsRelease reports whether this binary should use release config defaults.
//
// Deprecated: use version.IsRelease.
func IsRelease() bool { return v.IsRelease() }

// Channel returns the defaults channel this binary was built for.
//
// Deprecated: use version.Channel.
func Channel() string { return v.Channel() }

// NewSemVer parses a semantic version.
//
// Deprecated: use version.NewSemVer.
func NewSemVer(s string) (SemVer, error) { return v.NewSemVer(s) }
