# Filesystem observation: the I/O-surface audit (RFC-056).
#
#     sudo faultbox setup-trace          # once, then restart Docker
#     sudo faultbox test poc/gvisor-rfc056/faultbox.star
#
# Requires Linux, Docker, and runsc. On macOS: limactl shell faultbox-dev.
#
# WHAT watch() IS FOR
#
# Faultbox can already say "this service made 4,000 write calls". It could not
# say WHICH FILE — a syscall carries a file descriptor, and turning that into a
# path meant reading /proc out-of-band, which races the SUT and truncates.
#
# watch() gets the path from inside the sandbox, at the moment of the call.
# That makes a class of question answerable that previously was not:
#
#   - did this service write outside its data directory?
#   - did the WAL reach disk before the commit was acknowledged?
#   - which files does a cold start touch that a warm one does not?
#
# It is OBSERVATION ONLY. A trace point fires after the syscall completed, so
# short writes, torn writes and fsync lies need a datapath that can change what
# the SUT sees — that is FUSE, and it is deliberately out of scope here.

determinism(runtime = "gvisor")

pg = service("pg",
    interface("sql", "postgres", 5432),
    image = "postgres:16-alpine",
    env = {
        "POSTGRES_PASSWORD": "faultbox",
        "POSTGRES_DB":       "app",
    },
    healthcheck = tcp("localhost:5432", timeout = "60s"),
)


def io_events(**where):
    """file_io events, optionally filtered by field."""
    def keep(e):
        if e.type != "file_io":
            return False
        for k, v in where.items():
            if e.fields.get(k, "") != v:
                return False
        return True
    return events(where = keep)


def paths_touched():
    seen = {}
    for e in io_events():
        seen[e.fields.get("path", "?")] = True
    return sorted(seen.keys())


def wait_ready():
    """Give Postgres time to finish booting before opening a watch window.

    A fixed settle, which is uglier than a readiness probe and is the honest
    option here. Two things rule out the alternatives:

    The tcp healthcheck cannot do it. It probes the port Docker published, and
    Docker's proxy binds that port the moment the container starts — before
    Postgres is listening, let alone accepting queries. Measured: ready in 0 ms
    against a backend that needed ~10 s, so the body ran and tore the sink down
    mid-boot; the trace held 18 points where a standalone sink over the same
    boot saw 29,426.

    Probing with SQL does not work either. An exec against a backend that is
    not yet accepting takes ~30 s to fail, so a retry loop burns the test
    deadline: measured at 3m5s, INCONCLUSIVE.

    Faultbox has no exec- or log-line-based readiness gate, which is the real
    gap. Until it does, a settle is what a spec can honestly express.
    """
    sleep("25s")


def workload():
    """Real SQL over the network — the shape that v0.14.0 could not observe.

    This is the distinction the whole release turns on. `runsc trace create`
    instrumented only tasks created after it attached, so a query against an
    already-running backend produced 2 trace points. The session now comes
    from --pod-init-config at sandbox boot: 236 for the same work.
    """
    pg.sql.exec(sql = "CREATE TABLE IF NOT EXISTS t (id int, payload text)")
    for i in range(6):
        pg.sql.exec(sql = "INSERT INTO t VALUES (%d, 'row-%d')" % (i, i))
    pg.sql.exec(sql = "CHECKPOINT")


# --- 1. The audit ------------------------------------------------------

def test_io_surface_audit():
    """Postgres must not write outside its data directory.

    The canonical use, and the one that motivates the honesty guards: this is
    a NEGATIVE assertion, so it is only as strong as the completeness of what
    was observed. A dropped point, an unattributed point, or a sandbox that
    never connected all fail the test rather than letting it pass on a partial
    trace.
    """
    wait_ready()
    watch(pg, files = ["/**"], ops = ["write"], run = workload)

    outside = []
    for e in io_events(op = "write"):
        p = e.fields.get("path", "")
        if p != "" and not p.startswith("/var/lib/postgresql") and not p.startswith("/tmp"):
            outside.append(p)

    assert_true(len(io_events()) > 0,
        "no file_io events at all — the watch observed nothing")
    assert_true(len(outside) == 0,
        "postgres wrote outside its data directory: %s" % outside[:5])


# --- 2. Which files, not just how many ---------------------------------

def test_paths_are_resolved():
    """The point of watch(): a path, not a file descriptor number."""
    wait_ready()
    watch(pg, files = ["/var/lib/postgresql/**"], run = workload)

    paths = paths_touched()
    print("observed %d distinct paths" % len(paths))
    for p in paths[:8]:
        print("  ", p)

    assert_true(len(paths) > 0,
        "no paths were resolved; watch() exists to answer 'which file'")
    for p in paths:
        assert_true(p.startswith("/"),
            "path %r is not absolute — fd/dirfd resolution failed" % p)


# --- 3. Write ordering (not durability) --------------------------------

def test_wal_written_during_workload():
    """The WAL is written while the workload runs.

    NOT a durability check. gVisor has no fsync trace point, so "the bytes
    reached the WAL file" is provable and "the WAL was flushed to stable
    storage" is not. RFC-054 withdrew its durability scenario for exactly this
    reason, and calling this one a durability audit would be the same
    overclaim in a new place.
    """
    wait_ready()
    watch(pg, files = ["**/pg_wal/**"], ops = ["write"], run = workload)

    wal = io_events(op = "write")
    print("wal writes observed:", len(wal))
    assert_true(len(wal) > 0,
        "no writes to pg_wal during the insert workload and CHECKPOINT")


# --- 4. Scoping actually scopes ----------------------------------------

def test_files_filter_excludes_the_rest():
    """files= must bound what reaches the event log.

    This is what keeps a report readable: unmatched operations are discarded
    before they become events, so cost is proportional to what the spec asked
    about rather than to what the service did.
    """
    wait_ready()
    watch(pg, files = ["/var/lib/postgresql/**/pg_wal/**"], run = workload)

    for e in io_events():
        p = e.fields.get("path", "")
        assert_true("pg_wal" in p,
            "files= did not scope the trace: %r leaked through" % p)
