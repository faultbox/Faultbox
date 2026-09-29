package star

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.starlark.net/starlark"
)

func TestSpecOutputSurvivesFailedRunAndSnapshotsData(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("evidence.star", `
def setup():
    print("setup output")
    emit("setup", None)
def branch():
    print("branch output")
    emit("branch", {"ok": True})
def body():
    data = {"id": 9007199254740993, "nested": [None, "Привет\\\"\n"]}
    emit(type="asis.snapshot", data=data)
    data["id"] = 0
    print("ASIS-SNAPSHOT", json.encode(data))
    parallel(branch, branch)
    assert_eq(1, 2)
test("evidence", setup=setup, body=body)
def test_next():
    pass
`)
	if err != nil {
		t.Fatal(err)
	}
	rt.DiscoverTests()
	result := rt.RunTest(context.Background(), "test_evidence")
	if result.Result != "fail" {
		t.Fatalf("expected fail: %+v", result)
	}
	var prints, branches, snapshots int
	for _, ev := range result.Events {
		if ev.Type != "spec.stdout" && !strings.HasPrefix(ev.Type, "custom.") {
			continue
		}
		if ev.Fields["test"] != "test_evidence" || ev.Fields["source"] != "evidence.star" || ev.Fields["line"] == "0" {
			t.Fatalf("missing provenance: %+v", ev)
		}
		switch ev.Type {
		case "spec.stdout":
			prints++
		case "custom.branch":
			branches++
		case "custom.asis.snapshot":
			snapshots++
			if !strings.Contains(ev.Fields["data"], "9007199254740993") {
				t.Fatalf("mutated/rounded evidence: %+v", ev)
			}
		}
	}
	if prints != 4 || branches != 2 || snapshots != 1 {
		t.Fatalf("prints=%d branches=%d snapshots=%d reason=%s", prints, branches, snapshots, result.Reason)
	}
	encoded, err := json.Marshal(BuildTraceOutput("evidence.star", &SuiteResult{Tests: []TestResult{result}}))
	if err != nil || !strings.Contains(string(encoded), "custom.asis.snapshot") || !strings.Contains(string(encoded), "ASIS-SNAPSHOT") {
		t.Fatalf("trace dropped evidence: %s %v", encoded, err)
	}
	next := rt.RunTest(context.Background(), "test_next")
	for _, ev := range next.Events {
		if strings.HasPrefix(ev.Type, "custom.") || ev.Type == "spec.stdout" {
			t.Fatalf("evidence leaked: %+v", ev)
		}
	}
}

func TestSpecOutputScopeAndPredicateSafety(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("scope.star", `
alias_emit = emit
def predicate(trace):
    print("predicate output")
    return True
def forbidden(trace):
    alias_emit("forged", {})
    return True
def test_ok():
    expect = always(predicate)
    emit("mock.encode_error", {"ok": True})
    print("body output")
`)
	if err != nil {
		t.Fatal(err)
	}
	result := rt.RunTest(context.Background(), "test_ok")
	if result.Result != "pass" {
		t.Fatalf("%s: %s", result.Result, result.Reason)
	}
	for _, ev := range result.Events {
		if ev.Type == "mock.encode_error" || (ev.Type == "spec.stdout" && ev.Fields["message"] == "predicate output") {
			t.Fatalf("unsafe event: %+v", ev)
		}
	}
	end := rt.beginSpecOutput("manual")
	thread := rt.newSpecThread("manual", "body", "")
	restore := suppressSpecOutput(thread)
	_, err = starlark.Call(thread, rt.globals["forbidden"], starlark.Tuple{starlark.None}, nil)
	if err == nil || !strings.Contains(err.Error(), "not in predicates") {
		t.Fatalf("predicate emit accepted: %v", err)
	}
	child := childSpecThread("child", thread)
	if err := recordSpecOutput(child, "custom.bad", map[string]string{}); err == nil {
		t.Fatal("predicate child acquired capability")
	}
	restore()
	end()
	rt.events.Reset()
	nextEnd := rt.beginSpecOutput("next")
	defer nextEnd()
	if err := recordSpecOutput(thread, "custom.stale", map[string]string{}); err == nil {
		t.Fatal("late thread emitted")
	}
	printSpecOutput(thread, "late print")
	if len(rt.events.Events()) != 0 {
		t.Fatal("late output leaked")
	}
}

func TestNativeJSONDeepRoundtripAndErrors(t *testing.T) {
	rt := New(testLogger())
	err := rt.LoadString("json.star", `
def test_roundtrip():
    value = {"id": 9007199254740993, "text": "Привет\n\"\\", "null": None, "boolean": True}
    for i in range(40):
        value = {"child": [value]}
    encoded = json.encode(value)
    assert_eq(json.encode(json.decode(encoded)), encoded)
    assert_eq(json.encode(json.decode(json.indent(encoded))), encoded)
    assert_eq(json.encode(json.decode(json.encode_indent(value))), encoded)
    emit("deep", value)
def test_bad_json():
    json.decode("{")
def test_bad_payload():
    emit("invalid", print)
def test_cycle():
    x = []
    x.append(x)
    emit("cycle", x)
def test_bad_type():
    emit("bad type", {})
`)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"test_roundtrip", "test_bad_json", "test_bad_payload", "test_cycle", "test_bad_type"} {
		r := rt.RunTest(context.Background(), name)
		want := "fail"
		if name == "test_roundtrip" {
			want = "pass"
		}
		if r.Result != want {
			t.Fatalf("%s: %s %s", name, r.Result, r.Reason)
		}
	}
	if err := New(testLogger()).LoadString("load.star", `emit("load", {})`); err == nil {
		t.Fatal("emit allowed at load time")
	}
}
