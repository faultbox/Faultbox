// Package faultbox is the module root. It exists primarily to host the
// embedded standard recipe library, which gets compiled into the faultbox
// binary so users' specs can load("@faultbox/recipes/<name>.star", ...)
// without needing the Faultbox source tree locally.
//
// The actual runtime, CLI, and protocol code lives under cmd/ and internal/;
// this file only exposes the embedded FS.
package faultbox

import "embed"

// Recipes is the embedded standard recipe library. Each file is a Starlark
// module that exports a single namespace struct (see RFC-018 for the
// pattern and RFC-019 for the distribution convention).
//
// Access is mediated by the runtime's load() resolver: specs reference
// recipes as "@faultbox/recipes/<protocol>.star", not by raw path.
//
//go:embed recipes/*.star recipes/README.md
var Recipes embed.FS
