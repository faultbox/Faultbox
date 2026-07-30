package check

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// RFC-052 Gap 1 — `faultbox check`.

func write(t *testing.T, name, src string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const validSpec = `
svc = service("api",
    interface("main", "http", 8080),
    image = "nginx:1.27-alpine",
    healthcheck = ready(timeout = "30s"),
)

def test_something():
    r = svc.main.get(path = "/")
    assert_true(r.ok, "get failed")
`

func TestCheckAcceptsAValidSpec(t *testing.T) {
	res := Run(write(t, "ok.star", validSpec), -1)
	if !res.OK {
		t.Fatalf("valid spec reported not ok: %+v", res.Findings)
	}
	if len(res.Findings) != 0 {
		t.Errorf("unexpected findings: %+v", res.Findings)
	}
	if len(res.Tests) != 1 || res.Tests[0] != "test_something" {
		t.Errorf("tests = %v, want [test_something]", res.Tests)
	}
	if res.PlanInstances < 1 {
		t.Errorf("plan instances = %d, want at least 1", res.PlanInstances)
	}
}

// The whole point: a spec referencing a real image and a real port must be
// checkable with no Docker, no network and no processes. If this test ever
// needs a daemon, the command has lost its reason to exist.
//
// Enforced by pointing Docker at an address nothing is listening on. A check
// that tried to reach the daemon would fail or hang; one that never touches it
// is unaffected.
func TestCheckLaunchesNothing(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")

	res := Run(write(t, "ok.star", validSpec), -1)
	if !res.OK {
		t.Fatalf("check needed Docker — it must not: %+v", res.Findings)
	}
	if len(res.Tests) != 1 {
		t.Errorf("tests = %v", res.Tests)
	}
}

// A syntax error and an execution failure are different fixes, so they must
// carry different codes all the way out to the caller.
func TestCheckReportsCodedLoadErrors(t *testing.T) {
	cases := []struct {
		name string
		src  string
		code string
	}{
		{"syntax", `service(name = "x", "positional")`, "SPEC_SYNTAX"},
		{"unknown kwarg", `service("x", nonsense = 1)`, "SPEC_LOAD_FAILED"},
		{"missing recipe", `load("@faultbox/recipes/nope.star", "x")`, "SPEC_RECIPE_NOT_FOUND"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Run(write(t, "bad.star", tc.src+"\n"), -1)
			if res.OK {
				t.Fatal("broken spec reported ok")
			}
			if len(res.Findings) == 0 {
				t.Fatal("no findings")
			}
			f := res.Findings[0]
			if f.Code != tc.code {
				t.Errorf("code = %q, want %q (%s)", f.Code, tc.code, f.Message)
			}
			if f.Level != "error" {
				t.Errorf("level = %q, want error", f.Level)
			}
			if f.Suggestion == "" {
				t.Error("no suggestion — the finding does not say what to do next")
			}
		})
	}
}

// A spec that loads but declares nothing is a warning, not an error: it is
// valid, it just does nothing, and an agent mid-authoring hits this constantly.
func TestCheckWarnsOnNoTests(t *testing.T) {
	res := Run(write(t, "empty.star", "# nothing here\n"), -1)
	if !res.OK {
		t.Error("a spec with no tests is a warning, not an error")
	}
	if len(res.Findings) != 1 || res.Findings[0].Code != "NO_TESTS_DISCOVERED" {
		t.Fatalf("findings = %+v", res.Findings)
	}
	if res.Findings[0].Level != "warning" {
		t.Errorf("level = %q, want warning", res.Findings[0].Level)
	}
}

// Fan-out blowups are the mistake an agent makes and cannot see, and
// enumeration finds them without launching anything.
func TestCheckReportsPlanCost(t *testing.T) {
	src := `
svc = service("api", interface("main", "http", 8080), image = "nginx:1.27-alpine")

def test_fanout():
    a = choose("a", [1, 2, 3])
    b = choose("b", [1, 2, 3])
    assert_true(True, "ok")
`
	spec := write(t, "fanout.star", src)

	if res := Run(spec, -1); !res.OK {
		t.Fatalf("unexpected findings without a limit: %+v", res.Findings)
	}

	res := Run(spec, 2)
	if res.OK {
		t.Fatal("a plan over the limit must report an error")
	}
	var found bool
	for _, f := range res.Findings {
		if f.Code == "PLAN_COST_EXCEEDED" {
			found = true
			if f.Suggestion == "" {
				t.Error("cost finding carries no suggestion")
			}
		}
	}
	if !found {
		t.Errorf("no PLAN_COST_EXCEEDED among %+v", res.Findings)
	}
}

// A limit of zero means "unset" at the CLI boundary; here it must not be
// treated as a real limit that everything exceeds.
func TestCheckIgnoresNonPositiveLimits(t *testing.T) {
	for _, limit := range []int{0, -1, -100} {
		res := Run(write(t, "ok.star", validSpec), limit)
		for _, f := range res.Findings {
			if f.Code == "PLAN_COST_EXCEEDED" {
				t.Errorf("limit %d was treated as a real cap", limit)
			}
		}
	}
}

func TestCheckHandlesMissingFile(t *testing.T) {
	res := Run(filepath.Join(t.TempDir(), "nope.star"), -1)
	if res.OK {
		t.Error("a missing file must not report ok")
	}
	if len(res.Findings) == 0 {
		t.Fatal("no finding for a missing file")
	}
	if !strings.Contains(strings.ToLower(res.Findings[0].Message), "no such file") &&
		!strings.Contains(strings.ToLower(res.Findings[0].Message), "cannot find") {
		t.Errorf("message should name the real problem: %q", res.Findings[0].Message)
	}
}

// The JSON shape is an API an agent parses. Schema version and the ok flag must
// always be present.
func TestCheckResultShapeIsStable(t *testing.T) {
	res := Run(write(t, "ok.star", validSpec), -1)
	if res.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", res.SchemaVersion)
	}
	if res.Spec == "" {
		t.Error("spec path missing from the result")
	}
}
