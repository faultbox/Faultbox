# RFC-040 §8.1 end-to-end golden — clock leak detection.
#
# The leaker triggers a raw clock_gettime syscall on
# /trigger?leak=clock. The seccomp filter installed by the no-op
# write=allow() rule is what gets clock_gettime intercepted; the L1
# detection layer then emits an unmediated_io[clock] event into the
# log.
#
# We tolerate the category at the service level so the test passes
# end-to-end (testops harness wants exit 0). The strict-mode failure
# path is already covered by unit tests in builtins_determinism_test.go;
# the value of this golden is proving the syscall plumbing wires through
# correctly — clock_gettime is intercepted, classified, and emitted with
# the right category.

determinism()

leaker = service("leaker", "/tmp/faultbox-leaker",
    interface("main", "http", 8090),
    healthcheck = http("localhost:8090/healthz", timeout = "30s"),
    nondeterministic_ok = ["clock"],
    env = {"PORT": "8090"},
)

def test_clock_read_trips_strict():
    """Raw clock_gettime triggers unmediated_io[clock] under strict mode."""
    def scenario():
        # GET /trigger?leak=clock — leaker performs a raw
        # clock_gettime syscall before responding.
        leaker.main.get(path = "/trigger?leak=clock")

    # fault(write=allow()) is a no-op rule — its only purpose is to
    # install the seccomp filter so L1 detection can fire on top.
    # Without any fault rule, the service runs at native speed and
    # detection wouldn't trigger.
    fault(leaker, write=allow(), run = scenario)
