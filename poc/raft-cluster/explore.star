# Fault-TIMING exploration against hashicorp/raft main @ 4c8f61ac.
#
#   sudo faultbox test poc/raft-cluster/explore.star --explore all
#
# Companion to faultbox.star, which is the fast regression suite: five tests,
# one fault schedule each, ~7s total. This file is the opposite trade — two
# tests, many leaves, minutes not seconds.
#
# WHY THIS EXISTS
#
# The v0.14.0 Raft write-up reported "45 runs, not reproduced" for Antithesis
# bugs 2 and 3. That was true and almost meaningless: --runs N varies a seed,
# faultbox.star consumes no seed, so all 45 runs executed ONE fault schedule
# and differed only in OS scheduling. See
# docs/design/2026-07-28-raft-mesh-results.md §4.2.
#
# Both target bugs are races. What has to vary is not the seed but *when the
# fault lands relative to what the cluster is doing*:
#
#   bug 2  the transfer goroutine must be mid-replication exactly as the
#          leader steps down for lack of quorum
#   bug 3  the follower's log must be in a particular state when the
#          snapshot arrives
#
# HOW, WITH NO NEW PRIMITIVES
#
# choose() + --explore already fan a test into one leaf per combination
# (RFC-043 §5.2). The two knobs that matter are expressible today:
#
#   work volume   submit(choose(...))                      — cluster state
#   wall clock    await_stable(quiescence_window=choose(…)) — elapsed time
#
# await_stable is not a sleep; it blocks until the event log is quiet for the
# window, so it is a *floor* on elapsed time, not an exact delay. That is the
# right shape here anyway — the question is whether the leader has noticed it
# lost quorum yet, and that is what quiescence measures.

determinism(runtime = "gvisor")

BIN = "/tmp/raft-node"


def node(name, raft_port, admin_port, bootstrap = False):
    env = {
        "NODE_ID":   name,
        "PEER_IDS":  "node1,node2,node3",
        "RAFT_BIND": "127.0.0.1:%d" % raft_port,
        "HTTP_BIND": "127.0.0.1:%d" % admin_port,
    }
    if bootstrap:
        env["BOOTSTRAP"] = "1"
    return service(name, BIN,
        interface("raft", "tcp", raft_port),
        interface("admin", "http", admin_port),
        env = env,
        healthcheck = http("localhost:%d/health" % admin_port, timeout = "20s"),
        observe = [observe.stdout(decoder = decoder("json"))],
    )


node1 = node("node1", 8301, 8401, bootstrap = True)
node2 = node("node2", 8302, 8402)
node3 = node("node3", 8303, 8403)

NODES = [node1, node2, node3]


def _record(event, state):
    if event.count in state:
        return state
    new = dict(state)
    new[event.count] = event.hash
    return new


monitor("state_machine_safety",
    on         = match.event(type = "stdout", event = "fsm.apply"),
    state_init = {},
    update     = _record,
    check      = lambda event, state: state[event.count] == event.hash,
)


def wait_leader():
    for n in NODES:
        if n.admin.get(path = "/wait_leader").status == 200:
            return True
    return False


def submit(n):
    accepted = 0
    for i in range(n):
        done = False
        for attempt in range(4):
            for nd in NODES:
                if nd.admin.post(path = "/apply", body = "cmd-%d-%d" % (i, attempt)).status == 200:
                    accepted += 1
                    done = True
                    break
            if done:
                break
    return accepted


def converged():
    seen = {}
    for n in NODES:
        r = n.admin.get(path = "/state")
        if r.status != 200:
            return False
        seen[r.data.get("hash", "?")] = True
    return len(seen) == 1


# --- Bug 2: leadership transfer racing loss of quorum ------------------
#
# 3 x 3 x 2 = 18 leaves.
#
# The axis that matters is `gap`: how long the leader has been cut off when
# /transfer arrives.
#
#   "0ms"    transfer while the leader still believes it has quorum
#   "400ms"  transfer around LeaderLeaseTimeout — the contended window
#   "1200ms" transfer well after the leader has stepped down
#
# `warmup` sets how much log there is to replicate (an empty log gives the
# transfer nothing to block on). `hold` decides whether the partition outlives
# the transfer attempt or clears under it.

def test_transfer_timing():
    warmup = choose("warmup", [0, 10, 40])
    # NOT "0ms": await_stable rejects a non-positive quiescence window
    # ("window must be > 0"), which aborts the leaf as INCONCLUSIVE rather
    # than running it. 50ms is the shortest honest "immediately".
    gap  = choose("gap", ["50ms", "400ms", "1200ms"])
    hold = choose("hold", ["100ms", "900ms"])

    tag = "warmup=%d gap=%s hold=%s" % (warmup, gap, hold)

    assert_true(wait_leader(), "cluster never elected a leader [%s]" % tag)
    if warmup > 0:
        submit(warmup)

    partition_start(node1, node2)
    partition_start(node1, node3)

    await_stable(quiescence_window = gap)
    node1.admin.post(path = "/transfer")
    await_stable(quiescence_window = hold)

    partition_stop(node1, node2)
    partition_stop(node1, node3)

    recovered = wait_leader()
    await_stable(quiescence_window = "1s")

    accepted = submit(10)
    print("transfer: %s recovered=%s accepted=%d" % (tag, recovered, accepted))

    # A liveness assertion is only meaningful with a convergence window, and
    # "no node accepted a command" has several boring causes. Dump enough to
    # tell them apart before believing this is Antithesis bug 2.
    if accepted == 0:
        for n in NODES:
            r = n.admin.get(path = "/state")
            print("  DIAG %s: status=%d body=%s" % (n.name, r.status, r.body))
        rejects = events(where = lambda e: e.type == "stdout" and
            e.fields.get("event") == "apply.rejected")
        print("  DIAG apply.rejected count=%d" % len(rejects))
        for e in rejects[-5:]:
            print("  DIAG reject: %s" % e.fields.get("error", "?"))

    assert_true(accepted > 0,
        "cluster accepted no commands after a transfer that raced a partition " +
        "[%s] — candidate Antithesis bug 2 (transfer-in-progress flag never cleared)" % tag)


# --- Bug 3: InstallSnapshot leaving stale log entries ------------------
#
# 3 x 2 = 6 leaves.
#
# `behind` sets how far the isolated follower falls; SnapshotThreshold is 16,
# so 25 already forces InstallSnapshot and 400 forces several. `snap_when`
# decides whether the snapshot is taken while the follower is still cut off
# or after it has started catching up — Raft rule 7 is about what the follower
# does with its existing log, so the interesting case is the one where it HAS
# one that overlaps.

def test_snapshot_timing():
    behind    = choose("behind", [25, 120, 400])
    snap_when = choose("snap_when", ["during", "after"])

    assert_true(wait_leader(), "cluster never elected a leader")

    partition_start(node1, node3)
    partition_start(node2, node3)
    submit(behind)

    if snap_when == "during":
        node1.admin.post(path = "/snapshot")

    partition_stop(node1, node3)
    partition_stop(node2, node3)

    if snap_when == "after":
        node1.admin.post(path = "/snapshot")

    assert_true(wait_leader(), "cluster never recovered a leader")

    ok = False
    for round in range(6):
        submit(2)
        await_stable(quiescence_window = "1s")
        if converged():
            print("snapshot: behind=%d snap=%s converged round=%d" % (behind, snap_when, round))
            ok = True
            break

    assert_true(ok,
        "node3 never caught up after rejoining (behind=%d snap=%s), across 6 " % (behind, snap_when) +
        "rounds with 1s quiescence each — the leader is re-sending snapshots " +
        "in a loop (Antithesis bug 3)")
