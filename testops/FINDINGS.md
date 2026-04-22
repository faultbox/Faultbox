# FINDINGS

Product bugs and surprises uncovered while building the testops
harness. Each finding should link to (or turn into) a tracked issue and
graduate off this list once fixed.

---

## #1 — NormalizeTrace: service-block ordering within a test is non-deterministic [RESOLVED 2026-04-21]

**Discovered:** 2026-04-21 while scaffolding Phase 0 harness.
**Resolved:** 2026-04-21 — [internal/star/results.go](../internal/star/results.go) `NormalizeTrace` now sorts service names alphabetically before emitting blocks. Verified stable across 5 consecutive runs of `poc/mock-demo/faultbox.star`. First golden committed at `goldens/mock_demo.norm`.

**Symptom:** Two back-to-back runs of

```
./bin/faultbox test poc/mock-demo/faultbox.star --seed 1 \
    --normalize /tmp/trace.norm --format json
```

produce normalized traces that differ. The `--- cache ---` block floats
relative to other per-service blocks inside each `=== test_X ===`
section; the diff is pure reordering of otherwise-identical content.

**Reproducer:**

```
./bin/faultbox test poc/mock-demo/faultbox.star --seed 1 --normalize a.norm --format json
./bin/faultbox test poc/mock-demo/faultbox.star --seed 1 --normalize b.norm --format json
diff a.norm b.norm   # non-empty
```

**Impact:** Blocks committing goldens for any spec that starts ≥2 mock
services. Every spec in `poc/mock-demo/`, `mocks/*.star`, and most of
the multi-service tutorials is affected.

**Suspected cause:** `internal/star.NormalizeTrace` appears to iterate
the service map in insertion-or-arrival order rather than sorted order,
and the mock-service goroutines can register their `service_started`
events in any order because port binds are concurrent.

**Fix direction:** Inside `NormalizeTrace`, when emitting per-service
blocks within a test, sort service names alphabetically (or by the
order they are declared in the spec, if that is reliably captured).
Event ordering *within* a block is already stable.

**Harness workaround until fixed:** affected `Case` entries carry
`Skip:` pointing to this finding. They are not removed from the
registry so the gap stays visible.

---

## #2 — poc/demo/faultbox.star: `assert_eventually(openat on /tmp/inventory.wal)` never fires

**Discovered:** 2026-04-22 while attempting to seed poc_demo golden.

**Symptom:** `test_happy_path` fails deterministically with:

```
reason: assert_eventually: no matching event found
        (filters: service="inventory", syscall="openat", path="/tmp/inventory.wal")
per-service syscall summary:
  inventory    7 total, 0 faulted  [fsync:1 write:6]
```

Reproducible across 5 consecutive runs with the same seed, even after
removing `/tmp/inventory.wal` before each run. No `openat` events
appear in the trace for the `inventory` service at all.

**Suspected cause:** inventory-svc likely opens the WAL with something
other than `openat` (perhaps `open`, or inherits an already-open fd
from a different path), or the assertion path string doesn't match the
actual syscall path form. Syscall-family expansion already covers
write/writev/pwrite64; openat-family expansion (open, openat, openat2,
creat) may need similar breadth — or the spec needs a different path
matcher.

**Harness workaround:** `poc_demo` stays `Skip:` pointing at this
finding. Un-skip once the assertion reliably fires.

---
