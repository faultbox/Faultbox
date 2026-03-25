package star

import (
	"log/slog"
	"os"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestLoadAndDiscoverTests(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("test.star", `
db = service("db", "/tmp/mock-db",
    interface("main", "tcp", 5432),
)

api = service("api", "/tmp/mock-api",
    interface("public", "http", 8080),
    depends_on = [db],
)

def test_happy():
    pass

def test_slow():
    pass

def helper_func():
    pass
`)
	if err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	tests := rt.DiscoverTests()
	if len(tests) != 2 {
		t.Fatalf("expected 2 tests, got %d: %v", len(tests), tests)
	}
	if tests[0] != "test_happy" || tests[1] != "test_slow" {
		t.Fatalf("unexpected test names: %v", tests)
	}
}

func TestServiceRegistration(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("test.star", `
db = service("db", "/tmp/mock-db",
    interface("main", "tcp", 5432),
    env = {"PORT": "5432"},
    healthcheck = tcp("localhost:5432"),
)

api = service("api", "/tmp/mock-api",
    interface("public", "http", 8080),
    interface("internal", "grpc", 9090),
    env = {"PORT": "8080", "DB_ADDR": db.main.addr},
    depends_on = [db],
    healthcheck = http("localhost:8080/health"),
)
`)
	if err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	services := rt.Services()
	if len(services) != 2 {
		t.Fatalf("expected 2 services, got %d", len(services))
	}

	// db should come first (dependency order).
	if services[0].Name != "db" {
		t.Fatalf("expected db first, got %s", services[0].Name)
	}

	db := services[0]
	if db.Binary != "/tmp/mock-db" {
		t.Fatalf("db.Binary = %q", db.Binary)
	}
	if db.Interfaces["main"].Protocol != "tcp" {
		t.Fatalf("db.main.Protocol = %q", db.Interfaces["main"].Protocol)
	}
	if db.Healthcheck == nil || db.Healthcheck.Test != "tcp://localhost:5432" {
		t.Fatalf("db.Healthcheck = %v", db.Healthcheck)
	}

	api := services[1]
	if len(api.Interfaces) != 2 {
		t.Fatalf("api should have 2 interfaces, got %d", len(api.Interfaces))
	}
	if api.Env["DB_ADDR"] != "localhost:5432" {
		t.Fatalf("api.Env[DB_ADDR] = %q, want localhost:5432", api.Env["DB_ADDR"])
	}
	if len(api.DependsOn) != 1 || api.DependsOn[0] != "db" {
		t.Fatalf("api.DependsOn = %v", api.DependsOn)
	}
}

func TestInterfaceAddrAccess(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("test.star", `
db = service("db", "/tmp/mock-db",
    interface("main", "tcp", 5432),
)
addr = db.main.addr
port = db.main.port
`)
	if err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	// Check that db.main.addr resolved correctly.
	addr := rt.globals["addr"]
	if addr.String() != `"localhost:5432"` {
		t.Fatalf("addr = %s, want localhost:5432", addr)
	}
	port := rt.globals["port"]
	if port.String() != "5432" {
		t.Fatalf("port = %s, want 5432", port)
	}
}

func TestFaultBuiltins(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("test.star", `
d = delay("500ms")
dn = deny("ECONNREFUSED", probability="50%")
a = allow()
`)
	if err != nil {
		t.Fatalf("LoadString: %v", err)
	}

	d := rt.globals["d"].(*FaultDef)
	if d.Action != "delay" || d.Delay.Milliseconds() != 500 {
		t.Fatalf("delay = %+v", d)
	}

	dn := rt.globals["dn"].(*FaultDef)
	if dn.Action != "deny" || dn.Errno != "ECONNREFUSED" || dn.Probability != 0.5 {
		t.Fatalf("deny = %+v", dn)
	}

	a := rt.globals["a"].(*FaultDef)
	if a.Action != "allow" {
		t.Fatalf("allow = %+v", a)
	}
}

func TestAssertBuiltins(t *testing.T) {
	rt := New(testLogger())

	// assert_true passing.
	err := rt.LoadString("test.star", `assert_true(1 == 1)`)
	if err != nil {
		t.Fatalf("assert_true(true): %v", err)
	}

	// assert_true failing.
	err = rt.LoadString("test.star", `assert_true(1 == 2, "one != two")`)
	if err == nil {
		t.Fatal("expected assert_true to fail")
	}

	// assert_eq passing.
	err = rt.LoadString("test.star", `assert_eq(42, 42)`)
	if err != nil {
		t.Fatalf("assert_eq(42, 42): %v", err)
	}

	// assert_eq failing.
	err = rt.LoadString("test.star", `assert_eq(1, 2)`)
	if err == nil {
		t.Fatal("expected assert_eq to fail")
	}
}

func TestEventLog(t *testing.T) {
	log := NewEventLog()
	log.Emit("test_event", "svc1", map[string]string{"key": "val"})
	log.Emit("test_event2", "svc2", nil)

	events := log.Events()
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].Type != "test_event" || events[0].Service != "svc1" {
		t.Fatalf("event[0] = %+v", events[0])
	}
	if events[0].Fields["key"] != "val" {
		t.Fatalf("event[0].Fields = %v", events[0].Fields)
	}
	if events[1].Seq != 2 {
		t.Fatalf("event[1].Seq = %d, want 2", events[1].Seq)
	}
}
