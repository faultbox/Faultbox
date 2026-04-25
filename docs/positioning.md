# Where Faultbox fits

A 3-minute read for engineers asking *"is this a replacement for my integration tests, or for production chaos, or… what?"*. Short answer: it's neither — Faultbox occupies the gap between them.

## The four layers Faultbox injects at

Most chaos and fault tools operate at one layer. Faultbox composes four:

| Layer | Example | When it matters |
|---|---|---|
| **Syscall** | `write=deny("EIO")` on Postgres' write path | Disk failure, ENOSPC, EMFILE, partial writes — the OS-level failure modes you can't induce from above. |
| **Protocol (request side)** | Drop every 3rd `GetUser` gRPC call | Retry policies, circuit breakers, idempotency. Tests the client-side resilience code most teams write but never exercise. |
| **Protocol (response side)** | Rewrite HTTP `200 OK` → `503 Service Unavailable` for `/api/v1/orders` | Status-code handling, parser robustness, fallback behavior on degraded responses. |
| **Mock service behavior** | Inject 800 ms of latency in a mock OAuth issuer | Token-refresh flows, deadline propagation — without spinning up real auth infra. |

A single `faultbox.star` spec composes any combination of the four. That's the architectural difference from single-layer tools.

## Faultbox vs integration tests

**Use integration tests** for business-logic correctness, happy-path coverage, and end-to-end flows against real dependencies. *"Does order creation compute the right commission?"* That's an integration test.

**Use Faultbox** for resilience behavior under failure. *"What does my service return when Postgres refuses the connection, when gRPC upstream returns UNAVAILABLE, when Redis adds 500 ms of latency?"* That's what integration tests struggle with — you can't ask a real Postgres to refuse connections on cue without surgery on the test harness.

The two are **complementary**. A typical service has both: a CI suite of integration tests that prove correctness on the happy path, and a Faultbox spec that proves the same service degrades gracefully under each named failure mode.

## Faultbox vs load testers (k6, Gatling, Goose, Locust)

Load tests find **throughput ceilings and tail latency**: "at 5 k req/s, the p99 jumps from 50 ms to 4 s." Different question than Faultbox.

Faultbox runs scenarios serially and asks *"under failure X, does the service still uphold invariant Y?"* The answer doesn't depend on QPS — it depends on what your error-handling code does.

In a mature pipeline you'll run both. Load tests catch a class of regressions Faultbox can't (resource exhaustion under volume); Faultbox catches a class load tests can't (correctness under specific failures).

## Faultbox vs production chaos (Gremlin, Chaos Mesh, Litmus)

Production chaos validates **runbooks, blast radius, and human response** in real environments. Faultbox runs in pre-prod / local / CI and validates **code paths**. The natural pipeline:

1. Engineer writes a Faultbox spec while building the feature. Catches missing retries, missing timeouts, missing circuit breakers before the PR lands.
2. Staging environment runs production chaos against the deployed service. Catches what Faultbox missed: cross-service interactions, network partitions, real-world LB/k8s failure modes.
3. Production chaos drills exercise the runbooks the staging chaos surfaced.

If you skip step 1, you push every code-path failure all the way to step 2 — slow feedback loop, and the chaos tooling becomes the bottleneck. Faultbox shifts that work left.

## When NOT to use Faultbox

- **Pure UI tests** — Playwright / Cypress are the right tool.
- **Performance tuning** — pprof, flamegraphs, and a load tester.
- **Contract testing** — Pact does it better.
- **In production** — Faultbox uses seccomp-notify; running it on a prod process is a footgun. Use real chaos tooling.

## Tagline

> Faultbox finds the failure modes integration tests can't reach, before production chaos has to.
