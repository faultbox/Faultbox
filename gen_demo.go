//go:build ignore

// Generates a richer .fb bundle for visual QA of `faultbox report`.
// Produces a matrix (3 scenarios × 4 faults) with a mix of outcomes
// and realistic per-event fields so the report's event log expansion
// and grouped-detail views have something meaningful to display.
//
// Run with: go run gen_demo.go
package main

import (
	"fmt"
	"os"

	"github.com/faultbox/Faultbox/internal/bundle"
)

func main() {
	w := bundle.NewWriter()

	manifest := bundle.Manifest{
		SchemaVersion:   bundle.SchemaVersion,
		FaultboxVersion: "0.11.0",
		RunID:           "2026-04-23T11-30-00-7",
		CreatedAt:       "2026-04-23T11:30:00Z",
		Seed:            7,
		SpecRoot:        "examples/orders/faultbox.star",
		Tests: []bundle.TestRow{
			{Name: "test_order_flow__none", Outcome: "passed", DurationMs: 82, Seed: 7, Expectation: "expect_success"},
			{Name: "test_order_flow__db_down", Outcome: "passed", DurationMs: 110, Seed: 7, FaultAssumptions: []string{"postgres: connect=ECONNREFUSED"}, Expectation: "expect_success"},
			{Name: "test_order_flow__cache_latency", Outcome: "expectation_violated", DurationMs: 2840, Seed: 7, FaultAssumptions: []string{"redis: read=delay(500ms)"}, Expectation: "expect_error_within"},
			{Name: "test_order_flow__disk_full", Outcome: "passed", DurationMs: 134, Seed: 7, FaultAssumptions: []string{"fs: write=ENOSPC"}, Expectation: "expect_success"},
			{Name: "test_inventory__none", Outcome: "passed", DurationMs: 95, Seed: 7, Expectation: "expect_success"},
			{Name: "test_inventory__db_down", Outcome: "failed", DurationMs: 210, Seed: 7, FaultAssumptions: []string{"postgres: connect=ECONNREFUSED"}, Expectation: "expect_success"},
			{Name: "test_inventory__cache_latency", Outcome: "passed", DurationMs: 180, Seed: 7, Expectation: "expect_success"},
			{Name: "test_inventory__disk_full", Outcome: "passed", DurationMs: 100, Seed: 7, Expectation: "expect_success"},
			{Name: "test_checkout__none", Outcome: "passed", DurationMs: 115, Seed: 7, Expectation: "expect_success"},
			{Name: "test_checkout__db_down", Outcome: "passed", DurationMs: 140, Seed: 7, Expectation: "expect_success"},
			{Name: "test_checkout__cache_latency", Outcome: "passed", DurationMs: 200, Seed: 7, Expectation: "expect_success"},
			{Name: "test_checkout__disk_full", Outcome: "passed", DurationMs: 122, Seed: 7, Expectation: "expect_success"},
		},
		Summary: bundle.Summary{Total: 12, Passed: 10, Failed: 2, Errored: 0, ExpectationViolated: 1},
	}
	must(w.AddJSON("manifest.json", manifest))

	env := bundle.Env{
		FaultboxVersion: "0.11.0",
		FaultboxCommit:  "5b3c5d4a1f9e2b6c7d8e",
		HostOS:          "linux",
		HostArch:        "arm64",
		Kernel:          "6.1.0-faultbox",
		GoToolchain:     "go1.26.1",
		DockerVersion:   "25.0.2",
		RuntimeHints:    []string{"lima"},
		Images: map[string]string{
			"postgres:16-alpine": "sha256:a1b2c3d4e5f67890abcdef1122334455667788aa",
			"redis:7-alpine":     "sha256:deadbeef9876543210abcd1111222233334444ff",
			"kafka:3.7.0":        "sha256:01234567890abcdef01234567890abcdef012345",
			"nats:2.10":          "sha256:facefeed9876543210abcd1111222233334444ff",
		},
	}
	must(w.AddJSON("env.json", env))

	trace := map[string]any{
		"version":     2,
		"star_file":   "examples/orders/faultbox.star",
		"duration_ms": 4328,
		"pass":        10,
		"fail":        2,
		"tests":       buildTests(),
		"matrix":      buildMatrix(),
	}
	must(w.AddJSON("trace.json", trace))
	w.AddFile("replay.sh", []byte("#!/bin/sh\nfaultbox replay run-2026-04-23T11-30-00-7.fb\n"))

	w.AddFile("spec/faultbox.star", []byte(specFaultbox))
	w.AddFile("spec/helpers/retries.star", []byte(specHelpers))

	dst := "demo.fb"
	must(w.WriteTo(dst))
	fmt.Printf("wrote %s\n", dst)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func buildMatrix() map[string]any {
	scenarios := []string{"order_flow", "inventory", "checkout"}
	faults := []string{"none", "db_down", "cache_latency", "disk_full"}
	cells := []map[string]any{
		{"scenario": "order_flow", "fault": "none", "passed": true, "outcome": "passed", "expectation": "expect_success", "duration_ms": 82},
		{"scenario": "order_flow", "fault": "db_down", "passed": true, "outcome": "passed", "expectation": "expect_success", "duration_ms": 110},
		{"scenario": "order_flow", "fault": "cache_latency", "passed": false, "outcome": "expectation_violated", "expectation": "expect_error_within", "duration_ms": 2840, "reason": "expect_error_within(ms=1000): took 2840ms — exceeded budget"},
		{"scenario": "order_flow", "fault": "disk_full", "passed": true, "outcome": "passed", "expectation": "expect_success", "duration_ms": 134},
		{"scenario": "inventory", "fault": "none", "passed": true, "outcome": "passed", "expectation": "expect_success", "duration_ms": 95},
		{"scenario": "inventory", "fault": "db_down", "passed": false, "outcome": "failed", "expectation": "expect_success", "duration_ms": 210, "reason": "assert_true: response.ok was false"},
		{"scenario": "inventory", "fault": "cache_latency", "passed": true, "outcome": "passed", "expectation": "expect_success", "duration_ms": 180},
		{"scenario": "inventory", "fault": "disk_full", "passed": true, "outcome": "passed", "expectation": "expect_success", "duration_ms": 100},
		{"scenario": "checkout", "fault": "none", "passed": true, "outcome": "passed", "expectation": "expect_success", "duration_ms": 115},
		{"scenario": "checkout", "fault": "db_down", "passed": true, "outcome": "passed", "expectation": "expect_success", "duration_ms": 140},
		{"scenario": "checkout", "fault": "cache_latency", "passed": true, "outcome": "passed", "expectation": "expect_success", "duration_ms": 200},
		{"scenario": "checkout", "fault": "disk_full", "passed": true, "outcome": "passed", "expectation": "expect_success", "duration_ms": 122},
	}
	return map[string]any{
		"scenarios": scenarios,
		"faults":    faults,
		"cells":     cells,
		"total":     12,
		"passed":    10,
		"failed":    2,
	}
}

// ───────── Event helpers ─────────

type clock map[string]int64

func (c clock) tick(svc string) clock {
	out := clock{}
	for k, v := range c {
		out[k] = v
	}
	out[svc]++
	return out
}

func (c clock) merge(other clock) clock {
	out := clock{}
	for k, v := range c {
		out[k] = v
	}
	for k, v := range other {
		if v > out[k] {
			out[k] = v
		}
	}
	return out
}

type evBuf struct {
	seq    int64
	clocks map[string]clock
	out    []map[string]any
}

func newEvBuf() *evBuf { return &evBuf{clocks: map[string]clock{}} }

func (b *evBuf) emit(svc, typ string, fields map[string]string) map[string]any {
	b.seq++
	if b.clocks[svc] == nil {
		b.clocks[svc] = clock{}
	}
	b.clocks[svc] = b.clocks[svc].tick(svc)
	vc := map[string]int64{}
	for k, v := range b.clocks[svc] {
		vc[k] = v
	}
	ev := map[string]any{
		"seq":          b.seq,
		"type":         typ,
		"service":      svc,
		"vector_clock": vc,
	}
	if fields != nil {
		ev["fields"] = fields
	}
	b.out = append(b.out, ev)
	return ev
}

// merge simulates a synchronous call by merging target's clock into caller before caller's next tick.
func (b *evBuf) mergeClock(local, remote string) {
	if b.clocks[local] == nil {
		b.clocks[local] = clock{}
	}
	if b.clocks[remote] == nil {
		return
	}
	b.clocks[local] = b.clocks[local].merge(b.clocks[remote])
}

// ───────── Scenarios ─────────

// orderFlowEvents models the order creation happy path plus the cache_latency failure.
func orderFlowEvents(fault string) []map[string]any {
	b := newEvBuf()

	// Lifecycle
	for _, svc := range []string{"orders", "postgres", "redis"} {
		b.emit(svc, "service_started", nil)
		b.emit(svc, "service_ready", nil)
	}

	// Test driver sends request
	b.emit("test", "step_send", map[string]string{
		"target":    "orders",
		"method":    "POST",
		"path":      "/orders",
		"interface": "http",
		"protocol":  "http",
		"body":      `{"user_id":42,"items":[{"sku":"A-100","qty":2}]}`,
	})
	b.mergeClock("orders", "test")

	// Orders opens a postgres connection
	b.emit("orders", "syscall", map[string]string{
		"syscall":  "connect",
		"decision": "allow",
		"path":     "postgres:5432",
		"op":       "net.connect",
	})
	b.mergeClock("postgres", "orders")

	// Postgres SELECT for user profile
	b.emit("postgres", "proxy", map[string]string{
		"protocol":   "postgres",
		"query":      "SELECT id, email, credit_limit FROM users WHERE id = $1",
		"rows":       "1",
		"latency_ms": "14",
	})
	b.mergeClock("orders", "postgres")

	// Orders checks redis cache for cart
	b.emit("orders", "syscall", map[string]string{
		"syscall":  "connect",
		"decision": "allow",
		"path":     "/var/run/redis.sock",
		"op":       "net.connect",
	})
	b.mergeClock("redis", "orders")

	if fault == "cache_latency" {
		// Three faulted reads — retries + timeout
		for i := 0; i < 3; i++ {
			b.emit("redis", "proxy", map[string]string{
				"protocol":   "redis",
				"command":    "GET",
				"key":        "cart:u42",
				"latency_ms": "500",
				"action":     "delay",
			})
			b.emit("redis", "syscall", map[string]string{
				"syscall":    "read",
				"decision":   "delay(500ms)",
				"latency_ms": "500",
				"label":      "cache_latency",
				"op":         "io.read",
			})
			b.mergeClock("orders", "redis")
		}
		b.emit("test", "violation", map[string]string{
			"reason": "request took 2.8s, expected < 1s",
			"test":   "test_order_flow__cache_latency",
		})
	} else {
		// Fast cache miss — lookup + set
		b.emit("redis", "proxy", map[string]string{
			"protocol":   "redis",
			"command":    "GET",
			"key":        "cart:u42",
			"result":     "miss",
			"latency_ms": "1",
		})
		b.mergeClock("orders", "redis")

		// Postgres INSERT the order
		b.emit("postgres", "proxy", map[string]string{
			"protocol":   "postgres",
			"query":      "INSERT INTO orders (user_id, total_cents, items) VALUES ($1, $2, $3) RETURNING id",
			"rows":       "1",
			"latency_ms": "8",
		})
		b.mergeClock("orders", "postgres")

		// Write audit log to disk
		auditDecision := "allow"
		if fault == "disk_full" {
			auditDecision = "deny(ENOSPC)"
			b.emit("orders", "fault_applied", map[string]string{
				"syscall": "write",
				"action":  "deny",
				"errno":   "ENOSPC",
				"label":   "disk_full",
			})
		}
		b.emit("orders", "syscall", map[string]string{
			"syscall":  "write",
			"decision": auditDecision,
			"path":     "/var/log/orders/audit.log",
			"op":       "io.write",
		})

		// Redis SETEX to cache the new order
		b.emit("redis", "proxy", map[string]string{
			"protocol":   "redis",
			"command":    "SETEX",
			"key":        "order:1247",
			"ttl":        "300",
			"latency_ms": "1",
		})
		b.mergeClock("orders", "redis")

		// Kafka publish for downstream
		b.emit("orders", "proxy", map[string]string{
			"protocol":   "kafka",
			"api":        "Produce",
			"topic":      "orders.created",
			"partition":  "3",
			"size":       "284",
			"latency_ms": "6",
		})

		if fault == "db_down" {
			// Second postgres call fails
			b.emit("orders", "fault_applied", map[string]string{
				"syscall": "connect",
				"action":  "deny",
				"errno":   "ECONNREFUSED",
				"label":   "db_down",
			})
			b.emit("orders", "syscall", map[string]string{
				"syscall":  "connect",
				"decision": "deny(ECONNREFUSED)",
				"path":     "postgres:5432",
				"op":       "net.connect",
			})
		}
	}

	// Response to test
	status := "201"
	if fault == "cache_latency" {
		status = "500"
	}
	b.mergeClock("test", "orders")
	b.emit("test", "step_recv", map[string]string{
		"target": "orders",
		"method": "POST",
		"path":   "/orders",
		"status": status,
	})
	return b.out
}

// inventoryEvents models the inventory check flow.
func inventoryEvents(fault string) []map[string]any {
	b := newEvBuf()
	for _, svc := range []string{"inventory", "postgres", "redis"} {
		b.emit(svc, "service_started", nil)
		b.emit(svc, "service_ready", nil)
	}
	b.emit("test", "step_send", map[string]string{
		"target":    "inventory",
		"method":    "GET",
		"path":      "/stock/A-100",
		"interface": "http",
		"protocol":  "http",
	})
	b.mergeClock("inventory", "test")

	if fault == "db_down" {
		// Three retry attempts against a denied connect
		for i := 0; i < 3; i++ {
			b.emit("postgres", "fault_applied", map[string]string{
				"syscall": "connect",
				"action":  "deny",
				"errno":   "ECONNREFUSED",
				"label":   "db_down",
			})
			b.emit("postgres", "syscall", map[string]string{
				"syscall":  "connect",
				"decision": "deny(ECONNREFUSED)",
				"path":     "postgres:5432",
				"op":       "net.connect",
			})
		}
		b.emit("test", "violation", map[string]string{
			"reason": "response.ok was false",
			"test":   "test_inventory__db_down",
		})
	} else {
		// Cache check
		cacheResult := "miss"
		latency := "1"
		if fault == "cache_latency" {
			latency = "120"
		}
		b.emit("redis", "proxy", map[string]string{
			"protocol":   "redis",
			"command":    "GET",
			"key":        "stock:A-100",
			"result":     cacheResult,
			"latency_ms": latency,
		})
		b.mergeClock("inventory", "redis")

		b.emit("inventory", "syscall", map[string]string{
			"syscall":  "connect",
			"decision": "allow",
			"path":     "postgres:5432",
			"op":       "net.connect",
		})
		b.mergeClock("postgres", "inventory")

		b.emit("postgres", "proxy", map[string]string{
			"protocol":   "postgres",
			"query":      "SELECT sku, on_hand, reserved FROM stock WHERE sku = $1",
			"rows":       "1",
			"latency_ms": "11",
		})
		b.mergeClock("inventory", "postgres")

		b.emit("redis", "proxy", map[string]string{
			"protocol":   "redis",
			"command":    "SETEX",
			"key":        "stock:A-100",
			"ttl":        "60",
			"latency_ms": "1",
		})

		if fault == "disk_full" {
			b.emit("inventory", "fault_applied", map[string]string{
				"syscall": "write",
				"action":  "deny",
				"errno":   "ENOSPC",
				"label":   "disk_full",
			})
			b.emit("inventory", "syscall", map[string]string{
				"syscall":  "write",
				"decision": "deny(ENOSPC)",
				"path":     "/var/log/inventory/metrics.log",
				"op":       "io.write",
			})
		}
	}

	status := "200"
	if fault == "db_down" {
		status = "503"
	}
	b.mergeClock("test", "inventory")
	b.emit("test", "step_recv", map[string]string{
		"target": "inventory",
		"method": "GET",
		"path":   "/stock/A-100",
		"status": status,
	})
	return b.out
}

// checkoutEvents models the checkout flow.
func checkoutEvents(fault string) []map[string]any {
	b := newEvBuf()
	for _, svc := range []string{"checkout", "postgres", "redis"} {
		b.emit(svc, "service_started", nil)
		b.emit(svc, "service_ready", nil)
	}
	b.emit("test", "step_send", map[string]string{
		"target":    "checkout",
		"method":    "POST",
		"path":      "/checkout",
		"interface": "http",
		"protocol":  "http",
		"body":      `{"order_id":1247,"payment_token":"tok_live_abc123"}`,
	})
	b.mergeClock("checkout", "test")

	b.emit("checkout", "syscall", map[string]string{
		"syscall":  "connect",
		"decision": "allow",
		"path":     "postgres:5432",
		"op":       "net.connect",
	})
	b.mergeClock("postgres", "checkout")

	b.emit("postgres", "proxy", map[string]string{
		"protocol":   "postgres",
		"query":      "BEGIN",
		"latency_ms": "1",
	})
	b.emit("postgres", "proxy", map[string]string{
		"protocol":   "postgres",
		"query":      "UPDATE orders SET status = 'paid' WHERE id = $1",
		"rows":       "1",
		"latency_ms": "9",
	})
	b.emit("postgres", "proxy", map[string]string{
		"protocol":   "postgres",
		"query":      "COMMIT",
		"latency_ms": "2",
	})
	b.mergeClock("checkout", "postgres")

	b.emit("redis", "proxy", map[string]string{
		"protocol":   "redis",
		"command":    "DEL",
		"key":        "cart:u42",
		"latency_ms": "1",
	})

	// Publish confirmation
	b.emit("checkout", "proxy", map[string]string{
		"protocol":   "kafka",
		"api":        "Produce",
		"topic":      "orders.confirmed",
		"partition":  "1",
		"size":       "312",
		"latency_ms": "5",
	})

	b.mergeClock("test", "checkout")
	b.emit("test", "step_recv", map[string]string{
		"target": "checkout",
		"method": "POST",
		"path":   "/checkout",
		"status": "200",
	})
	return b.out
}

// ───────── Tests ─────────

type testSpec struct {
	name     string
	result   string
	duration int
	scenario string
	fault    string
	faults   []map[string]any
	diagnostics []map[string]any
	reason       string
	failureType  string
	syscallSum   map[string]any
}

func buildTests() []map[string]any {
	specs := []testSpec{
		{
			name: "test_order_flow__none", result: "pass", duration: 82,
			scenario: "order_flow", fault: "none",
		},
		{
			name: "test_order_flow__db_down", result: "pass", duration: 110,
			scenario: "order_flow", fault: "db_down",
			faults: []map[string]any{
				{"service": "postgres", "syscall": "connect", "action": "deny", "errno": "ECONNREFUSED", "hits": 1, "label": "db_down"},
			},
		},
		{
			name: "test_order_flow__cache_latency", result: "fail", duration: 2840,
			scenario: "order_flow", fault: "cache_latency",
			reason: "request took 2.8s, expected < 1s", failureType: "assertion",
			faults: []map[string]any{
				{"service": "redis", "syscall": "read", "action": "delay", "errno": "500ms", "hits": 42, "label": "cache_latency"},
			},
			diagnostics: []map[string]any{
				{"level": "error", "code": "ASSERTION_MISMATCH",
					"message":    "request took 2.8s, expected < 1s",
					"suggestion": "service may be retrying on slow cache reads without a timeout"},
			},
			syscallSum: map[string]any{
				"redis":  map[string]any{"total": 52, "faulted": 42, "breakdown": map[string]int{"read": 42, "write": 10}},
				"orders": map[string]any{"total": 210, "faulted": 0, "breakdown": map[string]int{}},
			},
		},
		{
			name: "test_order_flow__disk_full", result: "pass", duration: 134,
			scenario: "order_flow", fault: "disk_full",
			faults: []map[string]any{
				{"service": "orders", "syscall": "write", "action": "deny", "errno": "ENOSPC", "hits": 1, "label": "disk_full"},
			},
		},
		{
			name: "test_inventory__none", result: "pass", duration: 95,
			scenario: "inventory", fault: "none",
		},
		{
			name: "test_inventory__db_down", result: "fail", duration: 210,
			scenario: "inventory", fault: "db_down",
			reason: "response.ok was false", failureType: "assertion",
			faults: []map[string]any{
				{"service": "postgres", "syscall": "connect", "action": "deny", "errno": "ECONNREFUSED", "hits": 3, "label": "db_down"},
			},
			diagnostics: []map[string]any{
				{"level": "error", "code": "ASSERTION_MISMATCH",
					"message":    "response.ok was false",
					"suggestion": "inventory service may be missing a cache fallback when DB is down"},
			},
			syscallSum: map[string]any{
				"postgres":  map[string]any{"total": 3, "faulted": 3, "breakdown": map[string]int{"connect": 3}},
				"inventory": map[string]any{"total": 85, "faulted": 0, "breakdown": map[string]int{}},
			},
		},
		{
			name: "test_inventory__cache_latency", result: "pass", duration: 180,
			scenario: "inventory", fault: "cache_latency",
		},
		{
			name: "test_inventory__disk_full", result: "pass", duration: 100,
			scenario: "inventory", fault: "disk_full",
			faults: []map[string]any{
				{"service": "inventory", "syscall": "write", "action": "deny", "errno": "ENOSPC", "hits": 1, "label": "disk_full"},
			},
		},
		{
			name: "test_checkout__none", result: "pass", duration: 115,
			scenario: "checkout", fault: "none",
		},
		{
			name: "test_checkout__db_down", result: "pass", duration: 140,
			scenario: "checkout", fault: "none", // checkout doesn't hit postgres first — simpler graph
		},
		{
			name: "test_checkout__cache_latency", result: "pass", duration: 200,
			scenario: "checkout", fault: "none",
		},
		{
			name: "test_checkout__disk_full", result: "pass", duration: 122,
			scenario: "checkout", fault: "none",
		},
	}

	out := make([]map[string]any, 0, len(specs))
	for _, s := range specs {
		m := map[string]any{
			"name":        s.name,
			"result":      s.result,
			"seed":        7,
			"duration_ms": s.duration,
			"events":      eventsForScenario(s.scenario, s.fault),
		}
		if s.reason != "" {
			m["reason"] = s.reason
		}
		if s.failureType != "" {
			m["failure_type"] = s.failureType
		}
		if len(s.faults) > 0 {
			m["faults"] = s.faults
		}
		if len(s.diagnostics) > 0 {
			m["diagnostics"] = s.diagnostics
		}
		if s.syscallSum != nil {
			m["syscall_summary"] = s.syscallSum
		} else {
			m["syscall_summary"] = defaultSyscallSummary(s.scenario)
		}
		out = append(out, m)
	}
	return out
}

func eventsForScenario(scenario, fault string) []map[string]any {
	switch scenario {
	case "order_flow":
		return orderFlowEvents(fault)
	case "inventory":
		return inventoryEvents(fault)
	case "checkout":
		return checkoutEvents(fault)
	}
	return nil
}

func defaultSyscallSummary(scenario string) map[string]any {
	switch scenario {
	case "order_flow":
		return map[string]any{
			"orders":   map[string]any{"total": 76, "faulted": 0, "breakdown": map[string]int{"connect": 4, "read": 32, "write": 40}},
			"postgres": map[string]any{"total": 18, "faulted": 0, "breakdown": map[string]int{"connect": 2, "read": 8, "write": 8}},
			"redis":    map[string]any{"total": 6, "faulted": 0, "breakdown": map[string]int{"read": 3, "write": 3}},
		}
	case "inventory":
		return map[string]any{
			"inventory": map[string]any{"total": 85, "faulted": 0, "breakdown": map[string]int{"connect": 4, "read": 40, "write": 41}},
			"postgres":  map[string]any{"total": 15, "faulted": 0, "breakdown": map[string]int{"connect": 2, "read": 7, "write": 6}},
			"redis":     map[string]any{"total": 5, "faulted": 0, "breakdown": map[string]int{"read": 3, "write": 2}},
		}
	case "checkout":
		return map[string]any{
			"checkout": map[string]any{"total": 64, "faulted": 0, "breakdown": map[string]int{"connect": 3, "read": 28, "write": 33}},
			"postgres": map[string]any{"total": 24, "faulted": 0, "breakdown": map[string]int{"connect": 2, "read": 10, "write": 12}},
			"redis":    map[string]any{"total": 4, "faulted": 0, "breakdown": map[string]int{"read": 2, "write": 2}},
		}
	}
	return map[string]any{}
}

// ───────── Spec content ─────────

const specFaultbox = `# faultbox.star — orders / inventory / checkout demo topology
load("@faultbox/recipes/postgres.star", "postgres")
load("@faultbox/recipes/redis.star", "redis")
load("./helpers/retries.star", "retry_on_5xx")

orders = service("orders", image = "orders:latest",
    interface = interface("http", port = 8080),
    healthcheck = healthcheck("/healthz"),
    depends_on = ["postgres", "redis"])

inventory = service("inventory", image = "inventory:latest",
    interface = interface("http", port = 8081),
    healthcheck = healthcheck("/healthz"),
    depends_on = ["postgres", "redis"])

checkout = service("checkout", image = "checkout:latest",
    interface = interface("http", port = 8082),
    healthcheck = healthcheck("/healthz"),
    depends_on = ["postgres", "redis"])

postgres = service("postgres", image = "postgres:16-alpine")
redis    = service("redis", image = "redis:7-alpine")

def test_order_flow__none():
    r = orders.post("/orders", {"user_id": 42, "items": [{"sku": "A-100", "qty": 2}]})
    assert_eq(r.status, 201)

def test_order_flow__cache_latency():
    fault(redis, read = delay("500ms"), label = "cache_latency")
    r = retry_on_5xx(lambda: orders.post("/orders", {"user_id": 42}))
    assert_eq(r.status, 201)

def test_inventory__db_down():
    fault(postgres, connect = deny("ECONNREFUSED"), label = "db_down")
    r = inventory.get("/stock/A-100")
    assert_true(r.ok)

def test_checkout__none():
    r = checkout.post("/checkout", {"order_id": 1247, "payment_token": "tok_live_abc"})
    assert_eq(r.status, 200)
`

const specHelpers = `# helpers/retries.star — shared retry logic for the suite.

def retry_on_5xx(fn, max_attempts = 3):
    """Invoke fn() until it returns a 2xx, up to max_attempts times."""
    last = None
    for _ in range(max_attempts):
        last = fn()
        if last.status < 500:
            return last
    return last
`
