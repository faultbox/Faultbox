# RFC-040 §8.1 end-to-end golden — rand leak detection.
#
# Same shape as determinism_clock_read.star but the leaker triggers
# raw getrandom on /trigger?leak=rand. We tolerate "rand" at the
# service level so exit stays 0; the unmediated_io[rand] event in
# the trace is what the golden locks down.

leaker = service("leaker", "/tmp/faultbox-leaker",
    interface("main", "http", 8094),
    healthcheck = http("localhost:8094/healthz"),
    nondeterministic_ok = ["rand"],
    env = {"PORT": "8094"},
)

def test_rand_read_trips_strict():
    """Raw getrandom triggers unmediated_io[rand] under strict mode."""
    def scenario():
        leaker.main.get(path = "/trigger?leak=rand")
    fault(leaker, write=allow(), run = scenario)
