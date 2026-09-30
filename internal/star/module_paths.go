package star

import (
	"path/filepath"
	"strings"

	"go.starlark.net/starlark"
)

// A helper defined in a loaded module keeps that module's path semantics even
// when called later by a root test, seed callback, or parallel worker.
func (rt *Runtime) resolveCallerPath(thread *starlark.Thread, name string) string {
	if strings.Contains(name, "://") {
		return name
	}
	if filepath.IsAbs(name) {
		return rt.resolveSpecPath(name)
	}
	source := ""
	if thread != nil {
		for depth := 0; depth < thread.CallStackDepth(); depth++ {
			file := thread.CallFrame(depth).Pos.Filename()
			if file != "" && !strings.HasPrefix(file, "<") && !strings.HasPrefix(file, stdlibPrefix) {
				source = file
				break
			}
		}
		if source == "" {
			source, _ = thread.Local("faultbox.module").(string)
		}
	}
	if source == "" || strings.HasPrefix(source, stdlibPrefix) {
		return rt.resolveSpecPath(name)
	}
	if !filepath.IsAbs(source) {
		if filepath.Dir(source) == "." && rt.baseDir != "" {
			source = filepath.Join(rt.baseDir, source)
		} else {
			source, _ = filepath.Abs(source)
		}
	}
	// Replay remaps files individually. Recover the declaring module's original
	// location before resolving its siblings, then map the result into the bundle.
	if rt.replayResources != nil {
		for _, entry := range rt.replayResources.Resources {
			if filepath.Clean(filepath.Join(rt.baseDir, entry.Path)) == filepath.Clean(source) {
				source = entry.Source
				break
			}
		}
	}
	return rt.resolveSpecPath(filepath.Join(filepath.Dir(source), name))
}
