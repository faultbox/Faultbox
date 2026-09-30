package star

import (
	"context"
	"net/url"
	"time"

	"go.starlark.net/starlark"
)

func (rt *Runtime) executionContext(thread *starlark.Thread) context.Context {
	if thread != nil {
		if ctx, ok := thread.Local("faultbox.context").(context.Context); ok {
			return ctx
		}
	}
	if rt.inTest.Load() {
		return rt.testContext()
	}
	return context.Background()
}

// Startup callbacks get cancellation independently of the later test body.
func callbackContext(thread *starlark.Thread, parents ...context.Context) func() {
	parent := context.Background()
	if len(parents) > 0 && parents[0] != nil {
		parent = parents[0]
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	thread.SetLocal("faultbox.context", ctx)
	stop := context.AfterFunc(ctx, func() { thread.Cancel("startup callback deadline or cancellation") })
	return func() { stop(); cancel() }
}

func redactedHealthcheck(check string) string {
	u, err := url.Parse(check)
	if err != nil {
		return "<invalid healthcheck>"
	}
	return u.Redacted()
}
