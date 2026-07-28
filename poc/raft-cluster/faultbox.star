# Chain-of-blocks Raft workload against hashicorp/raft main @ 4c8f61ac.
#
# Port of the Antithesis Raft study harness
# (https://antithesis.com/blog/2026/finding-bugs-in-raft-implementations/)
# to Faultbox. The raft pin is the commit their fork branched from, not the
# v1.7.3 tag — see the comment in go.mod.
#
# NOTE ON --runs: the fault schedule here is unconditional, so the run seed
# reaches nothing. `--runs 25` is 25 repetitions of one schedule that differ
# only in OS scheduling, NOT 25 samples of a fault-timing space. Searching
# that space needs choose() over partition onset/duration; see
# docs/design/2026-07-28-raft-mesh-results.md §4.
#
#   sudo faultbox test poc/raft-cluster/faultbox.star
#
# Requires Linux with CAP_NET_ADMIN (the packet gateway needs a TUN device).
# On macOS: make env-start && limactl shell faultbox-dev.
#
# Topology: three peers, each with a `raft` tcp interface (peer traffic) and
# an `admin` http interface (client workload + state inspection). Peers find
# each other through the auto-injected FAULTBOX_NODEn_RAFT_ADDR env vars.
#
# There is deliberately NO startup ordering here. Before v0.14.0's mesh fix
# this spec had to start node3 → node2 → node1 with node1 as sole bootstrapper
# to get *any* mediation — and even that left node1's own inbound link
# unmediated, because a mesh is a cycle and no ordering covers a cycle.
# Gateway addresses are now allocated from the real upstream, so every link is
# mediated regardless of start order.

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


# --- Properties -------------------------------------------------------
#
# State machine safety, checked continuously: two nodes must never report a
# different chain hash at the same prefix length. The first node to report a
# given length fixes the expected hash; any node that later disagrees for the
# same length fails the test.

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


# --- Workload ---------------------------------------------------------

def wait_leader():
    """Block until some node reports a leader."""
    for n in NODES:
        if n.admin.get(path = "/wait_leader").status == 200:
            return True
    return False


def submit(n):
    """Submit n commands, retrying across nodes until one accepts.

    Any node may be the leader (or none, mid-election), so a rejection is
    expected rather than a failure. Only a command no node accepts within the
    retry budget counts as unavailability.
    """
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


def applies():
    """How many commands the cluster has applied, across all nodes."""
    return len(events(where = lambda e: e.type == "stdout" and
        e.fields.get("event") == "fsm.apply"))


def counts():
    """Per-node (count, hash) fingerprints."""
    return [n.admin.get(path = "/state").body for n in NODES]


def converged():
    """True when every node reports the same chain hash."""
    seen = {}
    for n in NODES:
        r = n.admin.get(path = "/state")
        if r.status != 200:
            return False
        seen[r.data.get("hash", "?")] = True
    return len(seen) == 1


# --- Baseline ---------------------------------------------------------

def test_baseline_replication():
    """No faults: all three nodes converge on the same chain."""
    assert_true(wait_leader(), "cluster never elected a leader")
    assert_true(submit(20) > 0, "no command was accepted by any node")
    for fp in counts():
        print("baseline:", fp)


# --- The fault the old partition() could not deliver -------------------

def test_isolate_leader_from_quorum():
    """Cut the leader off from BOTH followers at once.

    Raft's guarantee: a leader that cannot reach a majority must not commit.
    Before v0.14.0 this test passed while committing 30 more commands, because
    partition() denied connect() and the peers had long since stopped calling
    it. A packet drop is silence, which is what a partition actually is.
    """
    assert_true(wait_leader(), "cluster never elected a leader")
    before = applies()

    partition_start(node1, node2)
    partition_start(node1, node3)
    submit(30)
    partition_stop(node1, node2)
    partition_stop(node1, node3)

    during = applies() - before
    print("isolate: applies while leader had no quorum =", during)
    assert_true(during == 0,
        "leader committed %d commands with no quorum — Raft safety violated" % during)


def test_one_way_partition():
    """Cut node1 → node2 only, leaving node2 → node1 intact.

    The asymmetric case, where a leader keeps its lease while a follower times
    out. Inexpressible before source= reached rule installation.
    """
    assert_true(wait_leader(), "cluster never elected a leader")
    def scenario():
        submit(20)
    partition(node1, node2, direction = "a_to_b", run = scenario)
    assert_true(wait_leader(), "cluster never recovered a leader")


# --- Antithesis bug 2: leadership-transfer deadlock --------------------

def test_transfer_during_partition():
    """Leadership transfer while the leader loses quorum.

    Antithesis bug 2: the transfer goroutine blocks on replication that aborts
    when the leader steps down, and the global "transfer in progress" flag is
    never cleared — so the node refuses client commands forever after, even
    once it is re-elected.

    Liveness, not safety: nothing diverges, the cluster just stops serving.
    """
    assert_true(wait_leader(), "cluster never elected a leader")
    assert_true(submit(10) > 0, "cluster was not accepting commands before the fault")

    partition_start(node1, node2)
    partition_start(node1, node3)
    node1.admin.post(path = "/transfer")
    partition_stop(node1, node2)
    partition_stop(node1, node3)

    assert_true(wait_leader(), "cluster never recovered a leader after the transfer")
    # Let the cluster settle so a slow re-election is not mistaken for the
    # stuck transfer flag.
    await_stable(quiescence_window = "1s")

    accepted = submit(10)
    print("transfer: accepted after recovery =", accepted)
    assert_true(accepted > 0,
        "cluster accepted no commands after a transfer that raced a partition — " +
        "the transfer-in-progress flag never cleared (Antithesis bug 2)")


# --- Antithesis bug 3: snapshot-install livelock -----------------------

def test_snapshot_install_livelock():
    """Isolate a follower long enough that it needs InstallSnapshot.

    Antithesis bug 3: InstallSnapshot must discard the follower's entire log
    (Raft rule 7). hashicorp/raft retains stale entries, so the follower fails
    the next AppendEntries prevLogIndex check and the leader re-sends a
    snapshot forever — the follower never catches up.

    SnapshotThreshold is 16, so 200 commands during isolation put node3 far
    enough behind that a snapshot is the only way to catch it up.
    """
    assert_true(wait_leader(), "cluster never elected a leader")

    partition_start(node1, node3)
    partition_start(node2, node3)
    submit(200)
    node1.admin.post(path = "/snapshot")
    partition_stop(node1, node3)
    partition_stop(node2, node3)

    assert_true(wait_leader(), "cluster never recovered a leader")

    # Give the follower real time to catch up before judging.
    #
    # This distinction is the whole test. A follower that is merely *behind*
    # converges once replication runs; a follower stuck in the InstallSnapshot
    # livelock never does, however long you wait. HeartbeatTimeout is 300ms and
    # SnapshotInterval is 2s, so a few seconds of quiescence is several
    # replication rounds — far more than catching up needs.
    for round in range(6):
        submit(2)
        await_stable(quiescence_window = "1s")
        if converged():
            print("snapshot: converged after round", round)
            break

    for fp in counts():
        print("snapshot:", fp)

    assert_true(converged(),
        "node3 never caught up after rejoining, across 6 rounds with 1s of " +
        "quiescence each — the leader is re-sending snapshots in a loop " +
        "(Antithesis bug 3)")
