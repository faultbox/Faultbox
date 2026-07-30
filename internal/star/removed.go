package star

import (
	"fmt"

	"go.starlark.net/starlark"
)

// Removed builtins (RFC-052 M5 / RFC-044).
//
// These were deprecated in v0.13.0 with a warning naming v0.14.0 as the removal
// version. v0.14.0, v0.14.1, v0.15.0, v0.16.0 and v0.16.1 all shipped with them
// still present — so anyone who read the warning carefully concluded the removal
// had already happened and their spec was fine. That is a worse position than
// never having announced a date.
//
// # Why a stub rather than deleting the name
//
// Deleting the registration outright produces `undefined: stdout`, which is
// true and useless: it does not say the name was removed, when, or what to use
// instead. A spec written against the old API deserves to be told how to move,
// especially now that the primary author is an agent working from documentation
// that may predate the change.
//
// The stub costs one map entry per removed name and can be dropped in a later
// release once the old form has plausibly disappeared from the world.

// removedBuiltin returns a builtin that always fails, explaining the move.
func removedBuiltin(name, replacement, deprecatedIn, removedIn string) *starlark.Builtin {
	return starlark.NewBuiltin(name, func(
		*starlark.Thread, *starlark.Builtin, starlark.Tuple, []starlark.Tuple,
	) (starlark.Value, error) {
		return nil, fmt.Errorf(
			"%s() was removed in %s — use %s instead. "+
				"It was deprecated in %s (RFC-044) and warned on every run since",
			name, removedIn, replacement, deprecatedIn)
	})
}

// removedBuiltins are registered so a spec using the old API gets a legible
// error instead of "undefined".
func removedBuiltins() map[string]*starlark.Builtin {
	return map[string]*starlark.Builtin{
		"stdout":         removedBuiltin("stdout", "observe.stdout", "v0.13.0", "v0.17.0"),
		"stderr":         removedBuiltin("stderr", "observe.stderr", "v0.13.0", "v0.17.0"),
		"json_decoder":   removedBuiltin("json_decoder", `decoder("json")`, "v0.13.0", "v0.17.0"),
		"logfmt_decoder": removedBuiltin("logfmt_decoder", `decoder("logfmt")`, "v0.13.0", "v0.17.0"),
		"regex_decoder":  removedBuiltin("regex_decoder", `decoder("regex", pattern=...)`, "v0.13.0", "v0.17.0"),
	}
}
