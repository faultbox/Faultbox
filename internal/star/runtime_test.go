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

func TestEventLogReset(t *testing.T) {
	log := NewEventLog()
	log.Emit("evt1", "svc", nil)
	log.Emit("evt2", "svc", nil)
	if log.Len() != 2 {
		t.Fatalf("expected 2 events, got %d", log.Len())
	}

	log.Reset()
	if log.Len() != 0 {
		t.Fatalf("expected 0 events after reset, got %d", log.Len())
	}

	// Seq resets too.
	log.Emit("evt3", "svc", nil)
	events := log.Events()
	if events[0].Seq != 1 {
		t.Fatalf("expected seq=1 after reset, got %d", events[0].Seq)
	}
}

func TestWriteTraceResults(t *testing.T) {
	result := &SuiteResult{
		DurationMs: 1000,
		Pass:       1,
		Fail:       0,
		Tests: []TestResult{
			{
				Name:       "test_example",
				Result:     "pass",
				DurationMs: 500,
				Events: []Event{
					{Seq: 1, Type: "service_started", Service: "db"},
					{Seq: 2, Type: "syscall", Service: "db", Fields: map[string]string{
						"syscall": "write", "decision": "allow", "pid": "42",
					}},
					{Seq: 3, Type: "syscall", Service: "db", Fields: map[string]string{
						"syscall": "write", "decision": "delay(500ms)", "pid": "42", "latency_ms": "500",
					}},
				},
			},
		},
	}

	tmpFile := t.TempDir() + "/trace.json"
	if err := WriteTraceResults(tmpFile, "test.star", result); err != nil {
		t.Fatalf("WriteTraceResults: %v", err)
	}

	data, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}

	// Basic checks on JSON structure.
	s := string(data)
	if !contains(s, `"test_example"`) {
		t.Error("missing test name in output")
	}
	if !contains(s, `"syscall"`) {
		t.Error("missing syscall events in output")
	}
	if !contains(s, `"delay(500ms)"`) {
		t.Error("missing fault decision in output")
	}
}

func TestEventLogVectorClocks(t *testing.T) {
	log := NewEventLog()

	// Service db emits two events.
	log.Emit("syscall", "db", map[string]string{"syscall": "write"})
	log.Emit("syscall", "db", map[string]string{"syscall": "read"})

	// Service api emits one event.
	log.Emit("syscall", "api", map[string]string{"syscall": "connect"})

	events := log.Events()

	// db's clock: {db: 1}, {db: 2}
	if events[0].VectorClock["db"] != 1 {
		t.Fatalf("event[0] db clock = %d, want 1", events[0].VectorClock["db"])
	}
	if events[1].VectorClock["db"] != 2 {
		t.Fatalf("event[1] db clock = %d, want 2", events[1].VectorClock["db"])
	}

	// api's clock: {api: 1} (no merge yet)
	if events[2].VectorClock["api"] != 1 {
		t.Fatalf("event[2] api clock = %d, want 1", events[2].VectorClock["api"])
	}
	if _, ok := events[2].VectorClock["db"]; ok {
		t.Fatalf("api should not know about db before merge")
	}

	// Now merge db's clock into api.
	log.MergeClock("api", "db")
	log.Emit("syscall", "api", map[string]string{"syscall": "write"})

	events = log.Events()
	lastEvt := events[len(events)-1]

	// api's clock after merge: {api: 2, db: 2}
	if lastEvt.VectorClock["api"] != 2 {
		t.Fatalf("api clock after merge = %d, want 2", lastEvt.VectorClock["api"])
	}
	if lastEvt.VectorClock["db"] != 2 {
		t.Fatalf("db in api's clock after merge = %d, want 2", lastEvt.VectorClock["db"])
	}
}

func TestEventLogPObserveFields(t *testing.T) {
	log := NewEventLog()

	log.Emit("service_started", "db", nil)
	log.Emit("syscall", "api", map[string]string{"syscall": "connect"})

	events := log.Events()

	// Lifecycle events get dotted event_type.
	if events[0].EventType != "lifecycle.started" {
		t.Fatalf("event[0].EventType = %q, want lifecycle.started", events[0].EventType)
	}
	if events[0].PartitionKey != "db" {
		t.Fatalf("event[0].PartitionKey = %q, want db", events[0].PartitionKey)
	}

	// Syscall events get syscall-specific event_type.
	if events[1].EventType != "syscall.connect" {
		t.Fatalf("event[1].EventType = %q, want syscall.connect", events[1].EventType)
	}
}

func TestShiVizFormat(t *testing.T) {
	log := NewEventLog()

	log.Emit("service_started", "db", nil)
	log.Emit("syscall", "db", map[string]string{"syscall": "write", "decision": "allow"})
	log.Emit("syscall", "api", map[string]string{"syscall": "connect", "decision": "deny(ECONNREFUSED)"})

	shiviz := log.FormatShiViz()

	// Should contain the regex line.
	if !contains(shiviz, `(?<host>\S+) (?<clock>\{.*\})`) {
		t.Error("missing regex header")
	}

	// Should contain host entries.
	if !contains(shiviz, "db {") {
		t.Error("missing db host entry")
	}
	if !contains(shiviz, "api {") {
		t.Error("missing api host entry")
	}

	// Should contain event descriptions.
	if !contains(shiviz, "lifecycle.started") {
		t.Error("missing lifecycle event")
	}
	if !contains(shiviz, "deny(ECONNREFUSED)") {
		t.Error("missing deny decision")
	}

	// Vector clocks should have deterministic key ordering.
	if !contains(shiviz, `"db": `) {
		t.Error("missing db in vector clock")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
