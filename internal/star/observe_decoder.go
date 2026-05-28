package star

import (
	"fmt"
	"os"
	"sync"

	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
)

// RFC-044 §8.6 / §8.7 — unify event-source and decoder builtins
// under namespace surfaces:
//
//   - observe.stdout / observe.stderr  (was: stdout, stderr)
//   - decoder("json", ...) / decoder("logfmt", ...) /
//     decoder("regex", pattern=...)
//     (was: json_decoder, logfmt_decoder, regex_decoder)
//
// Old names stay registered as deprecated aliases that emit a
// one-time-per-spec stderr warning. Removal is scheduled for
// v0.14.0 — same migration cadence as faultbox generate (§8.3).

// deprecationWarnings tracks which deprecated builtins have
// already warned in this process, so a spec that calls
// `stdout()` ten times surfaces the migration hint once instead
// of ten times.
//
// Per-process (not per-spec) is intentional: most CI runs load
// exactly one spec, so per-process matches the user mental model.
// A test harness that LoadStrings multiple specs in sequence sees
// one warning total — which is fine because the migration message
// is the same regardless of which spec triggered it.
var (
	deprecationWarningsMu sync.Mutex
	deprecationWarnings   = map[string]bool{}
)

// warnDeprecated emits a one-time stderr line for an old builtin
// name, naming the replacement. Returns nil so callers can chain
// it before the actual builtin work.
func warnDeprecated(oldName, replacement string) {
	deprecationWarningsMu.Lock()
	already := deprecationWarnings[oldName]
	if !already {
		deprecationWarnings[oldName] = true
	}
	deprecationWarningsMu.Unlock()
	if already {
		return
	}
	fmt.Fprintf(os.Stderr, "warning: %s() is deprecated (RFC-044); use %s instead. Removal in v0.14.0.\n", oldName, replacement)
}

// resetDeprecationWarnings is exposed for tests so each test can
// observe the first-warn behavior without process-level state
// bleeding across runs.
func resetDeprecationWarnings() {
	deprecationWarningsMu.Lock()
	deprecationWarnings = map[string]bool{}
	deprecationWarningsMu.Unlock()
}

// makeObserveModule constructs the `observe` Starlark value
// (`starlarkstruct.Struct`) exposing event-source factories as
// attributes. Today: observe.stdout, observe.stderr. Future
// factories (topic, tail, wal_stream, poll) plug in here as
// they're implemented.
//
// The struct constructor name is "observe" so error messages
// reading `<observe.stdout>` are clear about the namespace.
func makeObserveModule() *starlarkstruct.Struct {
	return starlarkstruct.FromStringDict(
		starlarkstruct.Default,
		starlark.StringDict{
			"stdout": starlark.NewBuiltin("observe.stdout", builtinStdoutSource),
			"stderr": starlark.NewBuiltin("observe.stderr", builtinStderrSource),
		},
	)
}

// builtinStdoutDeprecated is the legacy top-level `stdout()` —
// behaves identically to observe.stdout but emits the deprecation
// warning on first call.
func builtinStdoutDeprecated(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	warnDeprecated("stdout", "observe.stdout")
	return builtinStdoutSource(thread, fn, args, kwargs)
}

// builtinStderrDeprecated mirrors builtinStdoutDeprecated for
// stderr.
func builtinStderrDeprecated(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	warnDeprecated("stderr", "observe.stderr")
	return builtinStderrSource(thread, fn, args, kwargs)
}

// builtinDecoder is RFC-044 §8.7's unified dispatcher.
// `decoder("json")` / `decoder("logfmt")` / `decoder("regex",
// pattern=...)` route to the same DecoderVal-producing logic the
// three legacy builtins used.
//
// Why dispatch by name rather than separate builtins: the three
// decoders share an identical surface modulo one kwarg
// (`pattern=` for regex). A single `decoder(name, ...)` builtin
// reads better in specs (`decoder("regex", pattern="...")`) and
// keeps RFC-045 (Protobuf decoder) one-line to add.
func builtinDecoder(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("decoder(name, ...): takes exactly one positional argument (the decoder name)")
	}
	name, ok := starlark.AsString(args[0])
	if !ok {
		return nil, fmt.Errorf("decoder(name, ...): name must be a string, got %s", args[0].Type())
	}
	switch name {
	case "json":
		if len(kwargs) > 0 {
			return nil, fmt.Errorf("decoder(\"json\"): takes no kwargs")
		}
		return &DecoderVal{Name: "json"}, nil
	case "logfmt":
		if len(kwargs) > 0 {
			return nil, fmt.Errorf("decoder(\"logfmt\"): takes no kwargs")
		}
		return &DecoderVal{Name: "logfmt"}, nil
	case "regex":
		params := make(map[string]string)
		for _, kv := range kwargs {
			k, _ := starlark.AsString(kv[0])
			v, _ := starlark.AsString(kv[1])
			params[k] = v
		}
		if _, ok := params["pattern"]; !ok {
			return nil, fmt.Errorf("decoder(\"regex\", pattern=...): requires pattern= kwarg")
		}
		return &DecoderVal{Name: "regex", Params: params}, nil
	}
	return nil, fmt.Errorf("decoder(%q): unknown decoder; known names: \"json\", \"logfmt\", \"regex\"", name)
}

// builtinJSONDecoderDeprecated etc. — legacy decoder builtins.
// Each emits the deprecation warning and delegates to the unified
// decoder() dispatcher (so the migration path is also the
// reference implementation — only one place to fix decoder bugs).
func builtinJSONDecoderDeprecated(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	warnDeprecated("json_decoder", `decoder("json")`)
	return builtinDecoder(thread, fn, starlark.Tuple{starlark.String("json")}, kwargs)
}

func builtinLogfmtDecoderDeprecated(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	warnDeprecated("logfmt_decoder", `decoder("logfmt")`)
	return builtinDecoder(thread, fn, starlark.Tuple{starlark.String("logfmt")}, kwargs)
}

func builtinRegexDecoderDeprecated(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	warnDeprecated("regex_decoder", `decoder("regex", pattern=...)`)
	return builtinDecoder(thread, fn, starlark.Tuple{starlark.String("regex")}, kwargs)
}
