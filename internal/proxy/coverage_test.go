package proxy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestProxyPluginsHaveCoverage is the v0.12 #84 process gate: every
// production source file in internal/proxy/ must have a sibling
// _test.go containing at least one Test* function. The customer
// regression Bug #1 (gRPC proxy passthrough corruption, v0.11.1)
// shipped because internal/proxy/grpc.go had **zero tests at all** —
// the lightest possible coverage gate would have caught it. The
// canonical pattern for new plugins is a byte-identity passthrough
// test (see grpc_test.go::TestGRPCProxyPassthroughDoesNotCorruptMessages),
// but the gate enforces only "at least one test" so existing
// rule-matching unit tests (postgres, mysql, cassandra, ...) keep
// counting. Plugins still shipping zero tests live in
// coverageExemptions until they're backfilled.
//
// To unblock a new plugin, add its test file with one Test* — even
// a placeholder t.Skip("TODO: passthrough test pending") satisfies
// the gate but flags the work in test output.
func TestProxyPluginsHaveCoverage(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/proxy: %v", err)
	}

	testFuncRE := regexp.MustCompile(`(?m)^func Test\w+\(`)

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// Allow-list non-plugin files: the protocol-agnostic
		// proxy.go dispatcher is exercised by proxy_test.go.
		if name == "proxy.go" {
			continue
		}
		if coverageExemptions[name] {
			t.Logf("EXEMPT (track in #84 follow-ups): %s has no test file yet", name)
			continue
		}

		base := strings.TrimSuffix(name, ".go")
		testPath := base + "_test.go"
		raw, err := os.ReadFile(testPath)
		if err != nil {
			t.Errorf("%s has no sibling %s — proxy plugins must ship with at least one test (#84). "+
				"Canonical pattern: byte-identity passthrough test. See grpc_test.go::"+
				"TestGRPCProxyPassthroughDoesNotCorruptMessages. "+
				"To unblock intentionally, add %q to coverageExemptions in coverage_test.go with an issue link.",
				name, testPath, name)
			continue
		}
		if !testFuncRE.Match(raw) {
			t.Errorf("%s exists but contains no Test* functions — at least one is required (#84).", testPath)
		}
	}
}

// coverageExemptions lists production files that are knowingly
// missing a passthrough test today. Each entry should map to an
// open follow-up issue. Empty when we've cleared the backlog.
//
// Backfill plan: every entry here corresponds to a proxy plugin
// that predates the v0.12 #84 gate. As tests land, drop the entry.
// New plugins must NOT be added here — write the test first.
var coverageExemptions = map[string]bool{
	"amqp.go":      true, // no protocol round-trip test yet — backfill in v0.12.x
	"http.go":      true, // covered by integration via poc/, but no unit-level passthrough
	"http2.go":     true, // ditto
	"kafka.go":     true, // backfill candidate
	"memcached.go": true,
	"nats.go":      true,
	"redis.go":     true,
	"udp.go":       true, // covered by clickhouse/cassandra adjacency tests; no dedicated udp passthrough yet
}

// TestCoverageExemptionsAreFresh is the meta-gate: every exemption
// must point at an actual existing source file. Stops the
// exemption list from rotting as files are renamed/removed.
func TestCoverageExemptionsAreFresh(t *testing.T) {
	for name := range coverageExemptions {
		path := filepath.Join(".", name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("coverageExemptions has a stale entry %q (file does not exist): %v", name, err)
		}
	}
}
