# RFC-040 §8.1 end-to-end golden — DNS leak detection.
#
# The leaker connects to 8.8.8.8:53. Port-53 traffic to a destination
# outside any declared interface trips the dns category (heuristic;
# misses DoH/DoT, documented in docs/determinism.md).
#
# The connect itself is the signal — the test does not require it to
# succeed (Lima may have no internet route). Detection fires on the
# connect attempt regardless.
#
# We tolerate "dns" at the service level so exit stays 0; the
# unmediated_io[dns] event in the trace is what the golden locks
# down.

determinism()

leaker = service("leaker", "/tmp/faultbox-leaker",
    interface("main", "http", 8091),
    healthcheck = http("localhost:8091/healthz", timeout = "30s"),
    nondeterministic_ok = ["dns"],
    env = {"PORT": "8091"},
)

def test_dns_resolution_trips_strict():
    """connect(8.8.8.8:53) triggers unmediated_io[dns] under strict mode."""
    def scenario():
        leaker.main.get(path = "/trigger?leak=dns")
    fault(leaker, write=allow(), run = scenario)
