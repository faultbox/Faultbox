package protocol

import (
	"context"
	"fmt"
	"time"
)

// readyProbeInterval is the gap between readiness attempts. Short enough that a
// fast service is not made to wait, long enough not to hammer a booting one.
const readyProbeInterval = 250 * time.Millisecond

// ReadyAfterTCP waits for a port to accept connections and then for the service
// behind it to actually answer, both within a single overall timeout.
//
// # Why this exists
//
// ready() promises to ask the service rather than the port. Several plugins
// only half-kept that promise: they retried the TCP connect (which succeeds the
// instant Docker's port proxy binds) and then made exactly **one**
// protocol-level attempt — at the precise moment the server is least likely to
// be up. MongoDB failed after ~2 s against a `ready(timeout="90s")`, reporting
// "connection reset by peer"; Cassandra, which needs a minute or more, could
// never have passed.
//
// The second bug this closes is a doubled budget. The old shape was
//
//	TCPHealthcheck(ctx, hostPort, timeout)   // may consume all of timeout
//	...then a fresh context.WithTimeout(ctx, timeout)
//
// so `ready(timeout="90s")` could take 180 s before reporting failure. Both
// phases here share one deadline, so the timeout a spec writes is the timeout it
// gets.
//
// probe is called with a per-attempt context and should be a cheap "can you
// serve?" check. It is retried until it succeeds or the budget runs out; the
// last error is reported, because that is the one describing the state the
// service was actually left in.
func ReadyAfterTCP(ctx context.Context, name, hostPort string, timeout time.Duration, probe func(context.Context) error) error {
	deadline := time.Now().Add(timeout)

	if err := TCPHealthcheck(ctx, hostPort, timeout); err != nil {
		return err
	}

	var lastErr error
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			if lastErr == nil {
				lastErr = fmt.Errorf("no attempt completed")
			}
			return fmt.Errorf("%s not ready after %s: %w", name, timeout, lastErr)
		}

		// Cap a single attempt so one hung dial cannot eat the whole budget,
		// but never exceed what is left of it.
		attemptBudget := 5 * time.Second
		if remaining < attemptBudget {
			attemptBudget = remaining
		}
		attemptCtx, cancel := context.WithTimeout(ctx, attemptBudget)
		err := probe(attemptCtx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyProbeInterval):
		}
	}
}

// CredentialsFor resolves the credentials a step should use.
//
// Plugins are handed an address from two places and only one of them carries
// credentials: Healthcheck gets the URL form that ready() builds, while
// ExecuteStep gets a bare "host:port" plus per-step kwargs that the runtime has
// already filled in from the service's env=. A plugin that reads only the
// address therefore authenticates correctly during the healthcheck and
// anonymously during every step — which is how MongoDB could report ready and
// then fail every insert with "Command insert requires authentication".
//
// Merging both here means each plugin has one place to get this right.
func CredentialsFor(addr string, kwargs map[string]any) Addr {
	a := ParseAddr(addr)
	a.User = getStringKwarg(kwargs, "user", a.User)
	a.Password = getStringKwarg(kwargs, "password", a.Password)
	a.Database = getStringKwarg(kwargs, "database", a.Database)
	return a
}
