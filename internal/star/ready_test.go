package star

import (
	"strings"
	"testing"
)

// ready() exists because tcp() answers the wrong question. It probes the port
// Docker published, and Docker's proxy binds that the moment the container
// starts — before the service is up. Measured: ready in 0 ms against a
// Postgres needing ~10 s, after which every test ran against a backend that
// was not there and, with no assertion on the result, passed.
func TestReadyResolvesToProtocolURLWithCredentials(t *testing.T) {
	rt := New(testLogger())
	svc := &ServiceDef{
		Name:  "pg",
		Image: "postgres:16-alpine",
		Env: map[string]string{
			"POSTGRES_PASSWORD": "s3cret",
			"POSTGRES_DB":       "app",
		},
		Interfaces: map[string]*InterfaceDef{
			"sql": {Name: "sql", Protocol: "postgres", Port: 5432, HostPort: 32768},
		},
		Healthcheck: &HealthcheckDef{Test: readyScheme},
	}

	got := rt.resolveHealthcheck(svc)

	// The mapped port, not the declared one: the check runs on the host.
	if !strings.Contains(got, "32768") {
		t.Errorf("should target the mapped host port: %s", got)
	}
	if !strings.HasPrefix(got, "postgres://") {
		t.Errorf("should name the protocol so the dispatcher can reach its plugin: %s", got)
	}
	// Credentials the spec already declared — without them the check
	// authenticates as the OS user and fails on every real server.
	for _, want := range []string{"postgres:s3cret@", "/app"} {
		if !strings.Contains(got, want) {
			t.Errorf("resolved check is missing %q: %s", want, got)
		}
	}
}

// A protocol with no richer notion of readiness must still work — ready()
// should never be worse than the tcp() it replaces.
func TestReadyFallsBackForPlainProtocols(t *testing.T) {
	rt := New(testLogger())
	svc := &ServiceDef{
		Name: "svc",
		Interfaces: map[string]*InterfaceDef{
			"main": {Name: "main", Protocol: "tcp", Port: 9000},
		},
		Healthcheck: &HealthcheckDef{Test: readyScheme},
	}
	got := rt.resolveHealthcheck(svc)
	if !strings.Contains(got, "9000") {
		t.Errorf("lost the port: %s", got)
	}
	if !strings.Contains(got, "://") {
		t.Errorf("should still be a scheme URL the dispatcher understands: %s", got)
	}
}

// ready() takes no address on purpose — it uses the service's own interface.
// Accepting one would invite the port-vs-mapped-port confusion it exists to
// remove.
func TestReadyRejectsPositionalArgs(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("spec.star", `
db = service("db", "/bin/true",
    interface("m", "tcp", 5432),
    healthcheck = ready("localhost:5432"),
)
`)
	if err == nil {
		t.Fatal("ready() must reject a positional address")
	}
	if !strings.Contains(err.Error(), "no positional") {
		t.Errorf("error should explain why: %v", err)
	}
}
