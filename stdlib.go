// Package faultbox is the module root. It exists primarily to host the
// embedded standard library — recipes (RFC-018, RFC-019) and mock service
// constructors (RFC-017) — compiled into the faultbox binary so users'
// specs can load("@faultbox/<area>/<name>.star", ...) without needing
// the Faultbox source tree locally.
//
// The actual runtime, CLI, and protocol code lives under cmd/ and internal/;
// this file only exposes the embedded FS.
package faultbox

import "embed"

// Stdlib is the embedded standard library. It contains:
//
//   - recipes/<protocol>.star — curated fault recipes (RFC-018)
//   - mocks/<protocol>.star — mock service constructors (RFC-017)
//
// Access is mediated by the runtime's load() resolver: specs reference
// stdlib modules as "@faultbox/<area>/<name>.star", not by raw path.
//
// The variable is named Stdlib; the legacy Recipes alias is retained for
// call sites that predate the mock library and will be removed once those
// are updated.
//
//go:embed recipes/*.star recipes/README.md mocks/*.star
var Stdlib embed.FS

// Recipes is a backward-compatible alias for Stdlib. Same FS, same paths.
// New code should use Stdlib.
var Recipes = Stdlib
