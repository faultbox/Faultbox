package star

import (
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// Options belong to each compiled file, never process-wide resolve globals.
func specFileOptions() *syntax.FileOptions {
	options := syntax.LegacyFileOptions()
	options.Recursion = true
	return options
}

// Check depth periodically so runaway recursion fails as a normal spec error
// before exhausting the Go stack, including load-time and mock callbacks.
func boundedThread(thread *starlark.Thread) *starlark.Thread {
	thread.SetMaxExecutionSteps(128)
	thread.OnMaxSteps = func(t *starlark.Thread) {
		if t.CallStackDepth() > 512 {
			t.Cancel("Starlark recursion depth limit exceeded (512 frames)")
			return
		}
		t.SetMaxExecutionSteps(t.ExecutionSteps() + 128)
	}
	return thread
}
