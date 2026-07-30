package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Contract tests for the MCP surface.
//
// The feature manifest recorded `faultbox mcp` as having no test coverage at
// all — a red row on the one surface whose declared primary user is an LLM
// agent. Adding a tool without fixing that would have made the row worse, so
// RFC-052 M4 includes these.
//
// They are contract tests on purpose: what an agent depends on is the tool
// list, the argument schemas, and the shape of what comes back. Those are the
// things that break silently and that no compiler checks.

func call(t *testing.T, s *Server, tool string, args any) *jsonRPCResponse {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	params, err := json.Marshal(map[string]any{"name": tool, "arguments": json.RawMessage(raw)})
	if err != nil {
		t.Fatal(err)
	}
	return s.handleToolsCall(context.Background(), &jsonRPCRequest{
		JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: params,
	})
}

// textOf extracts the single text block a tool returns.
func textOf(t *testing.T, resp *jsonRPCResponse) (string, bool) {
	t.Helper()
	if resp.Result == nil {
		t.Fatalf("response carries no result: %+v", resp.Error)
	}
	var r toolCallResult
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		t.Fatalf("result is not a toolCallResult: %v", err)
	}
	if len(r.Content) == 0 {
		t.Fatal("result has no content blocks")
	}
	return r.Content[0].Text, r.IsError
}

func writeSpec(t *testing.T, src string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "spec.star")
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const goodSpec = `
svc = service("api",
    interface("main", "http", 8080),
    image = "nginx:1.27-alpine",
    healthcheck = ready(timeout = "30s"),
)

def test_one():
    r = svc.main.get(path = "/")
    assert_true(r.ok, "get failed")
`

// Every advertised tool must be dispatchable. A tool listed but not wired
// returns "unknown tool" only when an agent tries it — at which point the agent
// has already committed to a plan around it.
func TestEveryAdvertisedToolDispatches(t *testing.T) {
	s := New("test")

	resp := s.handleToolsList(&jsonRPCRequest{JSONRPC: "2.0", ID: 1})
	if resp.Result == nil {
		t.Fatal("tools/list returned no result")
	}
	var listed struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &listed); err != nil {
		t.Fatalf("tools/list result: %v", err)
	}
	if len(listed.Tools) == 0 {
		t.Fatal("no tools advertised")
	}

	for _, tool := range listed.Tools {
		t.Run(tool.Name, func(t *testing.T) {
			if tool.Description == "" {
				t.Error("no description — an agent picks tools by description")
			}
			if len(tool.InputSchema) == 0 {
				t.Fatal("no input schema")
			}
			// The schema must be valid JSON Schema-ish: an object with
			// properties. An agent generates arguments from this.
			var schema struct {
				Type       string         `json:"type"`
				Properties map[string]any `json:"properties"`
				Required   []string       `json:"required"`
			}
			if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
				t.Fatalf("input schema is not valid JSON: %v", err)
			}
			if schema.Type != "object" {
				t.Errorf("schema type = %q, want object", schema.Type)
			}
			if len(schema.Properties) == 0 {
				t.Error("schema declares no properties")
			}
			for _, req := range schema.Required {
				if _, ok := schema.Properties[req]; !ok {
					t.Errorf("required field %q is not among the declared properties", req)
				}
			}

			// Dispatch with empty args: it may error, but never as
			// "unknown tool", which would mean the tool is advertised
			// and not wired.
			resp := call(t, s, tool.Name, map[string]any{})
			text, _ := textOf(t, resp)
			if strings.Contains(text, "unknown tool") {
				t.Errorf("advertised but not dispatchable: %s", text)
			}
		})
	}
}

func TestUnknownToolIsRejected(t *testing.T) {
	s := New("test")
	text, isErr := textOf(t, call(t, s, "no_such_tool", map[string]any{}))
	if !isErr {
		t.Error("unknown tool should be flagged as an error result")
	}
	if !strings.Contains(text, "unknown tool") {
		t.Errorf("message = %q", text)
	}
}

// check_spec is the RFC-052 Gap 1 tool. Its result must be the same structured
// document the CLI produces, since the whole point is that agent and human see
// one answer.
func TestCheckSpecReturnsTheCheckResult(t *testing.T) {
	s := New("test")
	text, isErr := textOf(t, call(t, s, "check_spec", map[string]any{
		"file": writeSpec(t, goodSpec),
	}))
	if isErr {
		t.Fatalf("valid spec reported as tool error: %s", text)
	}

	var res struct {
		SchemaVersion int      `json:"schema_version"`
		OK            bool     `json:"ok"`
		Tests         []string `json:"tests"`
		Findings      []struct {
			Code string `json:"code"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		t.Fatalf("check_spec did not return JSON: %v\n%s", err, text)
	}
	if res.SchemaVersion != 1 {
		t.Errorf("schema_version = %d", res.SchemaVersion)
	}
	if !res.OK {
		t.Errorf("valid spec reported not ok: %+v", res.Findings)
	}
	if len(res.Tests) != 1 || res.Tests[0] != "test_one" {
		t.Errorf("tests = %v", res.Tests)
	}
}

// A broken spec is a *successful* tool call reporting a finding — the agent
// asked whether the spec is valid and got a complete answer. Returning a tool
// error instead would throw away the code and the suggestion, which are the
// parts the agent needs to act.
func TestCheckSpecReportsBrokenSpecAsAFinding(t *testing.T) {
	s := New("test")
	text, isErr := textOf(t, call(t, s, "check_spec", map[string]any{
		"file": writeSpec(t, `service("x", nonsense = 1)`+"\n"),
	}))
	if isErr {
		t.Fatal("a spec with a load error should still be a successful tool call")
	}

	var res struct {
		OK       bool `json:"ok"`
		Findings []struct {
			Code       string `json:"code"`
			Level      string `json:"level"`
			Suggestion string `json:"suggestion"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, text)
	}
	if res.OK {
		t.Error("broken spec reported ok")
	}
	if len(res.Findings) == 0 {
		t.Fatal("no findings for a broken spec")
	}
	f := res.Findings[0]
	if f.Code == "" || f.Level != "error" || f.Suggestion == "" {
		t.Errorf("finding is missing what an agent acts on: %+v", f)
	}
}

func TestCheckSpecRequiresAFile(t *testing.T) {
	s := New("test")
	text, isErr := textOf(t, call(t, s, "check_spec", map[string]any{}))
	if !isErr {
		t.Error("missing file should be a tool error — the agent's call is malformed")
	}
	if !strings.Contains(text, "file") {
		t.Errorf("message should name the missing argument: %q", text)
	}
}

// max_instances=0 is what an agent sends when it omits the field; it must not
// be read as a limit of zero that every plan exceeds.
func TestCheckSpecTreatsZeroLimitAsUnset(t *testing.T) {
	s := New("test")
	text, _ := textOf(t, call(t, s, "check_spec", map[string]any{
		"file": writeSpec(t, goodSpec), "max_instances": 0,
	}))
	if strings.Contains(text, "PLAN_COST_EXCEEDED") {
		t.Error("max_instances=0 was treated as a real cap")
	}
}

func TestInitializeAdvertisesTools(t *testing.T) {
	s := New("test")
	resp := s.handleInitialize(&jsonRPCRequest{JSONRPC: "2.0", ID: 1})
	if resp.Result == nil {
		t.Fatal("initialize returned no result")
	}
	var init struct {
		Capabilities struct {
			Tools map[string]any `json:"tools"`
		} `json:"capabilities"`
		ServerInfo struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(resp.Result, &init); err != nil {
		t.Fatalf("initialize result: %v", err)
	}
	if init.Capabilities.Tools == nil {
		t.Error("initialize does not advertise tool capability")
	}
	if init.ServerInfo.Name == "" {
		t.Error("initialize does not name the server")
	}
}

// Malformed JSON in arguments must be reported, not panic the server. An agent
// generating arguments will get this wrong sometimes.
func TestMalformedArgumentsAreRejected(t *testing.T) {
	s := New("test")
	params, _ := json.Marshal(map[string]any{
		"name":      "check_spec",
		"arguments": json.RawMessage(`{"file": 42}`), // wrong type
	})
	resp := s.handleToolsCall(context.Background(), &jsonRPCRequest{
		JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: params,
	})
	text, isErr := textOf(t, resp)
	if !isErr {
		t.Error("a type error in arguments should be reported as a tool error")
	}
	if !strings.Contains(text, "invalid arguments") {
		t.Errorf("message = %q", text)
	}
}
