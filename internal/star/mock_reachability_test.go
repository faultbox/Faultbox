package star

import (
	"strings"
	"testing"
)

// mockTopology is the shape that had no coverage and therefore shipped
// broken: a containerized SUT alongside a mock_service.
const mockTopology = `
grpcmocks = mock_service("grpcmocks",
    interface("main", "grpc", 9095),
)
api = service("api",
    interface("public", "http", 8080),
    image = "alpine",
    env = {
        "MOCKS_ADDR": "localhost:9095",
    },
)
`

// TestMockAddrIsReachableFromAContainer covers F-2.
//
// A mock is an in-process listener on the host: no container, no DNS
// name. The env builder classified it as "not a container" and fell
// through to `localhost`, so a containerized SUT received
// FAULTBOX_GRPCMOCKS_MAIN_ADDR=localhost:9095 — which inside its own
// network namespace resolves to the SUT itself. There was no spelling a
// user could supply to reach a built-in mock from a container, so
// mock_service() was unusable for containerized SUTs entirely.
func TestMockAddrIsReachableFromAContainer(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", mockTopology); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	envMap := envToMap(rt.buildContainerEnv(rt.services["api"]))

	const wantHost = "host.docker.internal"
	if got := envMap["FAULTBOX_GRPCMOCKS_MAIN_HOST"]; got != wantHost {
		t.Errorf("FAULTBOX_GRPCMOCKS_MAIN_HOST = %q, want %q", got, wantHost)
	}
	if got := envMap["FAULTBOX_GRPCMOCKS_MAIN_ADDR"]; got != wantHost+":9095" {
		t.Errorf("FAULTBOX_GRPCMOCKS_MAIN_ADDR = %q, want %q", got, wantHost+":9095")
	}

	// The specific regression: "localhost" from inside a container is the
	// container itself, so this value silently pointed the SUT at itself.
	if got := envMap["FAULTBOX_GRPCMOCKS_MAIN_ADDR"]; strings.HasPrefix(got, "localhost:") {
		t.Errorf("mock addr %q resolves to the SUT's own container", got)
	}
}

// TestMockAddrSubstitutionIsNotFaultGated asserts a mock is rewritten for
// a container consumer whether or not any fault targets it.
//
// The RFC-035 gate skips substitution for unfaulted services because a
// real service is still reachable over Docker's DNS — rewriting it would
// be churn. That reasoning does not transfer: an unfaulted mock is not
// reachable by any other route, so the gate must not apply to it.
func TestMockAddrSubstitutionIsNotFaultGated(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", mockTopology); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	// No fault scenarios registered at all — the gate's "skip" branch.
	subs := rt.proxyAddrSubstitutionsFor(containerConsumer)

	want := "host.docker.internal:9095"
	for _, spelling := range []string{"localhost:9095", "127.0.0.1:9095", "grpcmocks:9095"} {
		if got := subs[spelling]; got != want {
			t.Errorf("substitution for %q = %q, want %q", spelling, got, want)
		}
	}
}

// TestMockAddrStaysLoopbackForBinaryConsumers guards the other half: a
// host-binary SUT shares the host's loopback, so rewriting its mock
// address to host.docker.internal would break a topology that works.
func TestMockAddrStaysLoopbackForBinaryConsumers(t *testing.T) {
	rt := New(testLogger())
	if err := rt.LoadString("test.star", mockTopology); err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	subs := rt.proxyAddrSubstitutionsFor(binaryConsumer)

	if got, ok := subs["localhost:9095"]; ok && strings.Contains(got, "host.docker.internal") {
		t.Errorf("binary consumer had its mock address rewritten to %q", got)
	}
}

func envToMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		out[k] = v
	}
	return out
}
