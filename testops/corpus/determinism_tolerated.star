# RFC-040 §8.2 strict-mode end-to-end golden — tolerated leaks pass.
#
# The leaker triggers all four detected leaks (clock, rand, dns,
# network-unmediated). The spec tolerates every category via the two
# documented escape hatches:
#
#   determinism(allow = ["clock", "rand"])     # spec-wide
#   service(..., nondeterministic_ok = [...])  # per-service
#
# Expected outcome: test passes (events appear in the trace as
# warnings rather than failures). This is the "your tests don't
# break when you've explicitly accepted the drift" assertion.

determinism(allow = ["clock", "rand"])

leaker = service("leaker", "/tmp/faultbox-leaker",
    interface("main", "http", 8093),
    healthcheck = http("localhost:8093/healthz"),
    nondeterministic_ok = ["dns", "network-unmediated"],
    env = {"PORT": "8093"},
)

def test_all_leaks_tolerated():
    """All four categories tolerated via allow= + nondeterministic_ok=."""
    def scenario():
        leaker.main.get(path = "/trigger?leak=clock")
        leaker.main.get(path = "/trigger?leak=rand")
        leaker.main.get(path = "/trigger?leak=dns")
        leaker.main.get(path = "/trigger?leak=network")
    fault(leaker, write=allow(), run = scenario)
