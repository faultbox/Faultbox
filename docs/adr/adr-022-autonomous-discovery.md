# ADR-022: Autonomous Discovery (`faultbox explore`)

**Status:** Proposed
**Date:** 2026-03-26

## Context

Faultbox requires users to write test scenarios (.star files) specifying which
faults to inject and what to assert. This is powerful for directed verification
but requires knowing what to test. Antithesis solves this with autonomous
exploration — finding bugs the user didn't think to look for.

With virtual time (Part III Step 1) and exhaustive exploration (Part III Step 2)
in place, Faultbox has the infrastructure for fast, systematic fault injection.
The missing piece: automated scenario generation and anomaly detection.

## Proposal

New CLI command: `faultbox explore <file.star>`

### Exploration Loop

1. **Baseline phase:** Start services, probe each interface, record normal responses
2. **Fault space enumeration:** services × syscalls × actions
   - For each service: write, read, connect, openat, fsync, sendto, recvfrom
   - Actions: deny(EIO), deny(ECONNREFUSED), deny(ETIMEDOUT), delay(100ms), delay(1s)
   - Single faults first, then pairs
3. **Exploration loop** (runs until --duration expires):
   a. Pick next fault combination
   b. Restart services, apply fault
   c. Send workload probes, compare to baseline
   d. If anomaly: record, generate replay .star file
   e. Continue
4. **Output:** directory of auto-generated .star test files

### Anomaly Detection

| Observation | Severity | Meaning |
|---|---|---|
| Process crash | HIGH | Unhandled error → crash |
| HTTP 5xx, fault on different service | HIGH | Cascading failure |
| Timeout (>10s) | HIGH | Deadlock or hang |
| HTTP 5xx, fault on same service | MEDIUM | Expected degradation |
| Status differs from baseline | LOW | Behavioral change |

### Workload Generation

- Auto-detect from .star topology: HTTP healthchecks → GET, TCP → PING
- User override: `--workload "POST /orders {\"sku\":\"widget\"}"`

### CLI

```bash
faultbox explore faultbox.star --duration 5m
faultbox explore faultbox.star --duration 5m --workload "POST /orders ..."
faultbox explore faultbox.star --duration 10m --output explore-results/
faultbox explore faultbox.star --virtual-time  # fast exploration
```

### Output

```
[00:30] explored 47/80 single faults... 2 anomalies found
[05:00] explored 847 combinations, found 3 anomalies

Anomalies:
  1. HIGH: inventory crashes on write=deny(ENOSPC)
     → explore-results/anomaly_001.star
  2. HIGH: order-svc hangs on connect=delay(2s)
     → explore-results/anomaly_002.star
  3. MEDIUM: order-svc returns 500 on fsync=deny(EIO)
     → explore-results/anomaly_003.star
```

## Dependencies

- Virtual time (Part III Step 1) — makes exploration fast
- Exhaustive exploration (Part III Step 2) — can be combined for interleaving bugs
- Code-to-spec extraction (ADR-018) — LLM-powered variant generates smarter probes

## Decision

Deferred. Build after Part III Steps 1-2 are validated with real users.
The infrastructure is ready; this is an application layer on top.

## Future: LLM-Powered Discovery

Level 2 of autonomous discovery uses LLM agents:
1. LLM reads service source code
2. Identifies failure points (network calls, DB writes, file operations)
3. Generates targeted .star scenarios
4. Runs tests, reads failures, iterates

This bridges ADR-018 (code-to-spec) with autonomous discovery.
