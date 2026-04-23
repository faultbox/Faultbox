package star

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.starlark.net/starlark"
	"gopkg.in/yaml.v3"
)

// RFC-026: three spec-load-time file readers for Starlark. Users who
// want to ship seed SQL, OpenAPI specs, or JSON fixtures into a spec
// reach for these instead of code-generating multi-hundred-line string
// constants. Security posture documented in the RFC:
//
//   - No network schemes (http:// etc. refused).
//   - Paths resolve relative to the spec's directory, not cwd.
//   - Size cap (default 50 MB, `FAULTBOX_LOAD_FILE_MAX_BYTES` override).
//   - Hermetic mode (`FAULTBOX_HERMETIC=1`) rejects symlinks that
//     escape the spec dir.
//
// All three fail at spec-load time so bad paths surface before any
// test runs. RFC-026 / v0.9.8.

// defaultLoadFileMaxBytes is the per-file size cap before reads are
// refused. Tuned at 50 MB to cover SQL dumps and protobuf descriptor
// sets comfortably; CI callers that need more override via env var.
const defaultLoadFileMaxBytes = 50 * 1024 * 1024

// builtinLoadFile implements load_file("path") → string. Opens the
// file at a spec-relative (or absolute) path and returns its bytes as
// a Starlark string. Go strings are byte-safe so binary payloads
// (protobuf descriptor sets, CA bundles) round-trip intact.
func (rt *Runtime) builtinLoadFile(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var path string
	if err := starlark.UnpackArgs("load_file", args, kwargs, "path", &path); err != nil {
		return nil, err
	}
	data, err := rt.readLoadFile(path)
	if err != nil {
		return nil, err
	}
	return starlark.String(string(data)), nil
}

// builtinLoadYAML implements load_yaml("path") → Starlark value.
// YAML scalars, sequences, and mappings decode into Starlark str/int/
// float/bool/None/list/dict. Custom tags and non-string map keys
// error at load time — if a user wanted them they'd be using
// load_file and parsing themselves.
func (rt *Runtime) builtinLoadYAML(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var path string
	if err := starlark.UnpackArgs("load_yaml", args, kwargs, "path", &path); err != nil {
		return nil, err
	}
	data, err := rt.readLoadFile(path)
	if err != nil {
		return nil, err
	}
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("load_yaml %q: %w", path, err)
	}
	return goToStarlarkValue(raw, path, "load_yaml")
}

// builtinLoadJSON implements load_json("path") → Starlark value.
// Same shape as load_yaml but without YAML's alias/tag complexity;
// errors are bounded to "malformed JSON" territory.
func (rt *Runtime) builtinLoadJSON(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var path string
	if err := starlark.UnpackArgs("load_json", args, kwargs, "path", &path); err != nil {
		return nil, err
	}
	data, err := rt.readLoadFile(path)
	if err != nil {
		return nil, err
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("load_json %q: %w", path, err)
	}
	return goToStarlarkValue(raw, path, "load_json")
}

// readLoadFile resolves path (spec-relative or absolute) and returns
// its bytes, enforcing the RFC-026 guardrails. Also records the file
// into rt.loadedSpecs so RFC-025 Phase 4 bundle capture picks it up
// automatically — no per-builtin plumbing needed in the bundle
// builder.
func (rt *Runtime) readLoadFile(path string) ([]byte, error) {
	if strings.Contains(path, "://") {
		return nil, fmt.Errorf("load_*(%q): network schemes are not supported — inline the content or download separately", path)
	}

	resolved := path
	if !filepath.IsAbs(resolved) && rt.baseDir != "" {
		resolved = filepath.Join(rt.baseDir, path)
	}

	// Hermetic mode: make sure the final path (after symlink resolution)
	// still lives under the spec directory. Off by default locally
	// because it's irritating during dev; CI sets FAULTBOX_HERMETIC=1.
	if os.Getenv("FAULTBOX_HERMETIC") == "1" && rt.baseDir != "" {
		real, err := filepath.EvalSymlinks(resolved)
		if err != nil {
			return nil, fmt.Errorf("load_*(%q): resolve: %w", path, err)
		}
		base, err := filepath.EvalSymlinks(rt.baseDir)
		if err != nil {
			return nil, fmt.Errorf("load_*(%q): resolve base: %w", path, err)
		}
		if !strings.HasPrefix(real, base+string(filepath.Separator)) && real != base {
			return nil, fmt.Errorf("load_*(%q): escapes spec directory %q (FAULTBOX_HERMETIC=1)", path, rt.baseDir)
		}
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("load_*(%q): %w", path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("load_*(%q): path is a directory", path)
	}
	if max := loadFileMaxBytes(); info.Size() > max {
		return nil, fmt.Errorf("load_*(%q): %d bytes exceeds FAULTBOX_LOAD_FILE_MAX_BYTES (%d); override the env var if intentional",
			path, info.Size(), max)
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("load_*(%q): %w", path, err)
	}

	// Capture into the bundle's spec/ section so RFC-025 bundles carry
	// the file bytes alongside the .star tree. Mirrors the transitive
	// load() capture logic in makeLoadFunc; paths outside baseDir land
	// under _external/ via bundleSpecKey() at emit time.
	if rt.loadedSpecs == nil {
		rt.loadedSpecs = make(map[string][]byte)
	}
	if abs, absErr := filepath.Abs(resolved); absErr == nil {
		rt.loadedSpecs[abs] = append([]byte(nil), data...)
	}
	return data, nil
}

// loadFileMaxBytes reads the size cap from the env var, falling back
// to the compile-time default. Cheap to call; evaluated per builtin
// invocation so users can tune mid-session without restarting a
// long-running runner.
func loadFileMaxBytes() int64 {
	if s := os.Getenv("FAULTBOX_LOAD_FILE_MAX_BYTES"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return defaultLoadFileMaxBytes
}

// goToStarlarkValue converts a generic Go value produced by
// yaml.Unmarshal / json.Unmarshal into a Starlark value. The caller's
// label (e.g. "load_yaml") flows into error messages so users see
// which builtin complained when a map key isn't a string.
func goToStarlarkValue(v any, path, label string) (starlark.Value, error) {
	switch x := v.(type) {
	case nil:
		return starlark.None, nil
	case bool:
		return starlark.Bool(x), nil
	case string:
		return starlark.String(x), nil
	case int:
		return starlark.MakeInt(x), nil
	case int64:
		return starlark.MakeInt64(x), nil
	case float64:
		// JSON has no integer type — all numbers decode as float64.
		// YAML does distinguish but the !!int tag ends up as int/int64
		// handled above, so float64 here is always a real float OR a
		// JSON-sourced integer. Collapse integer-valued floats to
		// Starlark int so callers doing arithmetic aren't surprised
		// by `42.0` on a config they wrote `42` in.
		if !isFloatIntegerValued(x) {
			return starlark.Float(x), nil
		}
		return starlark.MakeInt64(int64(x)), nil
	case []any:
		out := make([]starlark.Value, 0, len(x))
		for _, item := range x {
			converted, err := goToStarlarkValue(item, path, label)
			if err != nil {
				return nil, err
			}
			out = append(out, converted)
		}
		return starlark.NewList(out), nil
	case map[string]any:
		// JSON always gives us string keys; YAML may give non-string.
		// Handled identically here because json.Unmarshal decodes into
		// map[string]any for all JSON objects.
		return goMapToStarlark(x, path, label)
	case map[any]any:
		// yaml.v3 defaults to map[string]any, but older callers and
		// non-string-keyed YAML produce map[any]any. Reject non-string
		// keys explicitly rather than coercing.
		normalised := make(map[string]any, len(x))
		for k, val := range x {
			ks, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("%s(%q): non-string map key %v — Starlark dicts require string keys", label, path, k)
			}
			normalised[ks] = val
		}
		return goMapToStarlark(normalised, path, label)
	default:
		return nil, fmt.Errorf("%s(%q): unsupported value type %T", label, path, v)
	}
}

// isFloatIntegerValued reports whether x has no fractional component
// and fits inside int64. Used to unwrap JSON numbers that a user
// plainly meant as integers. We reject ±Inf and NaN because those
// can't round-trip through any integer type.
func isFloatIntegerValued(x float64) bool {
	if x != x { // NaN
		return false
	}
	if x < -9.2233720368547758e18 || x > 9.2233720368547758e18 { // int64 range
		return false
	}
	return x == float64(int64(x))
}

// goMapToStarlark builds a starlark.Dict from a string-keyed Go map.
// Sorted insertion is unnecessary for correctness but keeps iteration
// order deterministic in tests, which matters when users assert on
// Starlark-side JSON re-encoding.
func goMapToStarlark(m map[string]any, path, label string) (starlark.Value, error) {
	d := starlark.NewDict(len(m))
	for k, val := range m {
		converted, err := goToStarlarkValue(val, path, label)
		if err != nil {
			return nil, err
		}
		if err := d.SetKey(starlark.String(k), converted); err != nil {
			return nil, fmt.Errorf("%s(%q): set dict key %q: %w", label, path, k, err)
		}
	}
	return d, nil
}
