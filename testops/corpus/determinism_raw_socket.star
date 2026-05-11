# RFC-040 §8.1 end-to-end golden — network-unmediated detection.
#
# The leaker connects to 127.0.0.1:19999 — no listener, no declared
# interface, no Faultbox proxy. The connect attempt fires
# unmediated_io[network-unmediated].
#
# Why port 19999: outside ephemeral-port allocations on most stacks,
# unlikely to collide with anything else on the host.
#
# We tolerate "network-unmediated" at the service level so exit stays
# 0; the unmediated_io event in the trace is what the golden locks down.

leaker = service("leaker", "/tmp/faultbox-leaker",
    interface("main", "http", 8092),
    healthcheck = http("localhost:8092/healthz"),
    nondeterministic_ok = ["network-unmediated"],
    env = {"PORT": "8092"},
)

def test_raw_socket_trips_strict():
    """connect(127.0.0.1:19999) → unmediated_io[network-unmediated] under strict."""
    def scenario():
        leaker.main.get(path = "/trigger?leak=network")
    fault(leaker, write=allow(), run = scenario)
