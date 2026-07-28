# RFC-054 packet-fault scenario corpus.
#
# Every scenario the RFC lists that survived Milestone 0, as executable spec
# code. Requires Linux with CAP_NET_ADMIN — the packet gateway needs a TUN
# device. Run inside the Lima VM:
#
#     sudo faultbox test poc/gvisor-rfc054/faultbox.star
#
# Scenario 10 (WAL durability audit) is absent on purpose: gVisor has no fsync
# trace point, so it cannot be built. See RFC-054 decision record M0.3.

determinism(runtime = "gvisor")

db = service("db", "/tmp/mock-db",
    interface("main", "tcp", 5432),
    env = {"PORT": "5432"},
    healthcheck = tcp("localhost:5432"),
)

api = service("api", "/tmp/mock-api",
    interface("public", "http", 8080),
    env = {"PORT": "8080", "DB_ADDR": db.main.addr},
    depends_on = [db],
    healthcheck = http("localhost:8080/health"),
)

def hit_api_busy():
    """Same traffic as hit_api, repeated.

    Occurrence selectors like every=3 need at least that many matching
    segments; two requests do not produce three server-to-client data
    segments, so the rule would match nothing and the scenario would be
    vacuous.
    """
    for i in range(8):
        api.post(path = "/data/k%d" % i, body = "value-%d" % i)
        api.get(path = "/data/k%d" % i)

def hit_api():
    """Drive real api -> db traffic across the gateway.

    mock-api only serves /health and /data/, and only /data/ dials the DB —
    a path that never reaches the DB would leave every packet rule matching
    nothing while the test still passed.
    """
    api.post(path = "/data/k", body = "v")
    api.get(path = "/data/k")

def assert_packets_faulted(action):
    """Assert the packet layer actually acted.

    Without this every scenario below would pass simply by not crashing, which
    is the failure mode this whole release is built to avoid: a green test that
    proves nothing because no fault ever fired.

    Uses events() — a scan of the trace already recorded — rather than
    assert_eventually(), which is a temporal operator that waits for future
    events and therefore cannot see what happened inside a fault window that
    has already closed.
    """
    acted = events(where = lambda e: e.type == "packet" and e.fields.get("action") == action)
    assert_true(len(acted) > 0,
        "packet rule with action=%s matched nothing, so no fault fired and this test proves nothing" % action)

# 1. Silent blackhole — the half-open connection.
#    No RST, so the client sits in ESTABLISHED writing into a void. This is
#    the bug class today's proxy drop() cannot reproduce: closing a connection
#    sends a RST that clients handle correctly on the first try.
def test_half_open_blackhole():
    fault(db.main,
        packet_drop(dir = "s2c", flags = "!SYN", label = "blackhole"),
        run = hit_api,
    )
    assert_packets_faulted("drop")

# 2. Gray partition — partial, directional, probabilistic (RFC-050).
def test_gray_partition():
    fault(db.main,
        packet_drop(dir = "c2s", probability = "30%", label = "gray"),
        run = hit_api,
    )

# 3. Connection-pool poisoning — RST mid-stream.
def test_pool_poisoning():
    fault(db.main,
        packet_reset(after = 4, label = "mid-stream-rst"),
        run = hit_api,
    )
    assert_packets_faulted("reset")

# 4. Zero-window stall — does the SUT apply backpressure or buffer to OOM?
def test_zero_window_stall():
    fault(db.main,
        packet_window(size = 0, dir = "s2c", label = "stall"),
        run = hit_api,
    )
    assert_packets_faulted("window")

# 5. Asymmetric latency — breaks naive RTT-based timeout tuning.
def test_asymmetric_latency():
    fault(db.main,
        packet_delay("300ms", dir = "s2c", label = "slow-return"),
        packet_delay("5ms", dir = "c2s", label = "fast-send"),
        run = hit_api,
    )
    assert_packets_faulted("delay")

# 6. Retransmit storm — drop every third data segment.
def test_retransmit_storm():
    fault(db.main,
        packet_drop(dir = "s2c", every = 3, flags = "PSH,ACK", label = "loss"),
        run = hit_api_busy,
    )
    assert_packets_faulted("drop")

# 8. MTU black hole — small requests fine, large ones vanish.
#    An approximation: bandwidth()/mtu() land in v0.14.1, so this drops
#    oversized packets rather than truly failing PMTUD.
def test_mtu_blackhole():
    fault(db.main,
        packet_drop(len_gt = 900, label = "pmtud-blackhole"),
        run = hit_api,
    )

# 9. Payload-triggered fault on a custom binary protocol.
def test_payload_predicate():
    fault(db.main,
        packet_delay("500ms",
            where = lambda p: p.payload.startswith("GET"),
            label = "get-only"),
        run = hit_api,
    )
    assert_packets_faulted("delay")

# 11. I/O surface audit — the SUT never talks anywhere unexpected.
def test_no_unexpected_peers():
    fault(db.main,
        packet_pass(label = "observe-only"),
        run = hit_api,
    )
    assert_packets_faulted("pass")
    dropped = events(where = lambda e: e.type == "packet" and e.fields.get("action") == "drop")
    assert_true(len(dropped) == 0, "packet_pass must not drop anything")

# 12. Silent corruption — receiver accepts the packet with wrong bytes.
def test_silent_corruption():
    fault(db.main,
        packet_corrupt(offset = 0, length = 4, corrupt_mode = "flip",
                       checksum = "fix", label = "silent-corruption"),
        run = hit_api,
    )
    assert_packets_faulted("corrupt")

# Composition: exhaustive probability fan-out, identical semantics to
# syscall faults (RFC-042 §8.9).
def test_exhaustive_loss():
    fault(db.main,
        packet_drop(probability = 0.3, max_fires = 2, mode = "exhaustive"),
        run = hit_api,
    )

# Composition: packet and protocol faults on one interface, different layers.
def test_mixed_layers():
    fault(db.main,
        packet_delay("20ms", dir = "c2s"),
        drop(command = "GET"),
        run = hit_api,
    )
    assert_packets_faulted("delay")
