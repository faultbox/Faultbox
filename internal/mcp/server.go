// Package mcp implements a Model Context Protocol server for Faultbox.
// It exposes fault injection tools over JSON-RPC 2.0 on stdio.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"

	"github.com/faultbox/Faultbox/internal/compose"
	"github.com/faultbox/Faultbox/internal/generate"
	"github.com/faultbox/Faultbox/internal/logging"
	"github.com/faultbox/Faultbox/internal/star"
)

// JSON-RPC 2.0 types.

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MCP protocol types.

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeResult struct {
	ProtocolVersion string     `json:"protocolVersion"`
	Capabilities    capability `json:"capabilities"`
	ServerInfo      serverInfo `json:"serverInfo"`
}

type capability struct {
	Tools *toolsCap `json:"tools,omitempty"`
}

type toolsCap struct {
	ListChanged bool `json:"listChanged"`
}

type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type toolsListResult struct {
	Tools []toolDef `json:"tools"`
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type toolCallResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Server is the MCP server instance.
type Server struct {
	version string
	logger  *slog.Logger
}

// New creates an MCP server.
func New(version string) *Server {
	// Log to stderr so stdout stays clean for JSON-RPC.
	logger := logging.New(logging.Config{
		Format: logging.FormatJSON,
		Level:  slog.LevelWarn,
		Output: os.Stderr,
	})
	return &Server{version: version, logger: logger}
}

// Run starts the MCP server on stdio.
func (s *Server) Run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	reader := bufio.NewReader(os.Stdin)

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("read stdin: %w", err)
		}

		line = []byte(strings.TrimSpace(string(line)))
		if len(line) == 0 {
			continue
		}

		var req jsonRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.sendError(req.ID, -32700, "parse error: "+err.Error())
			continue
		}

		resp := s.handle(ctx, &req)
		if resp != nil {
			s.send(resp)
		}
	}
}

func (s *Server) handle(ctx context.Context, req *jsonRPCRequest) *jsonRPCResponse {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req)
	case "initialized":
		return nil // notification, no response
	case "tools/list":
		return s.handleToolsList(req)
	case "tools/call":
		return s.handleToolsCall(ctx, req)
	case "ping":
		return s.result(req.ID, json.RawMessage(`{}`))
	default:
		return s.errorResp(req.ID, -32601, "method not found: "+req.Method)
	}
}

func (s *Server) handleInitialize(req *jsonRPCRequest) *jsonRPCResponse {
	result := initializeResult{
		ProtocolVersion: "2024-11-05",
		Capabilities: capability{
			Tools: &toolsCap{ListChanged: false},
		},
		ServerInfo: serverInfo{
			Name:    "faultbox",
			Version: s.version,
		},
	}
	return s.resultObj(req.ID, result)
}

func (s *Server) handleToolsList(req *jsonRPCRequest) *jsonRPCResponse {
	tools := toolsListResult{
		Tools: []toolDef{
			{
				Name:        "run_test",
				Description: "Run all tests in a .star spec file and return structured results",
				InputSchema: json.RawMessage(`{
					"type": "object",
					"properties": {
						"file": {"type": "string", "description": "Path to the .star spec file"},
						"seed": {"type": "integer", "description": "Deterministic seed for replay (optional)"}
					},
					"required": ["file"]
				}`),
			},
			{
				Name:        "run_single_test",
				Description: "Run a specific test by name from a .star spec file",
				InputSchema: json.RawMessage(`{
					"type": "object",
					"properties": {
						"file": {"type": "string", "description": "Path to the .star spec file"},
						"test": {"type": "string", "description": "Test function name (without test_ prefix)"},
						"seed": {"type": "integer", "description": "Deterministic seed for replay (optional)"}
					},
					"required": ["file", "test"]
				}`),
			},
			{
				Name:        "list_tests",
				Description: "Discover test functions in a .star spec file",
				InputSchema: json.RawMessage(`{
					"type": "object",
					"properties": {
						"file": {"type": "string", "description": "Path to the .star spec file"}
					},
					"required": ["file"]
				}`),
			},
			{
				Name:        "generate_faults",
				Description: "Run the failure scenario generator on a .star spec file",
				InputSchema: json.RawMessage(`{
					"type": "object",
					"properties": {
						"file": {"type": "string", "description": "Path to the .star spec file"},
						"scenario": {"type": "string", "description": "Filter by scenario name (optional)"}
					},
					"required": ["file"]
				}`),
			},
			{
				Name:        "init_from_compose",
				Description: "Generate a .star spec from a docker-compose.yml file",
				InputSchema: json.RawMessage(`{
					"type": "object",
					"properties": {
						"compose_file": {"type": "string", "description": "Path to docker-compose.yml"}
					},
					"required": ["compose_file"]
				}`),
			},
			{
				Name:        "init_spec",
				Description: "Generate a starter .star spec for a binary service",
				InputSchema: json.RawMessage(`{
					"type": "object",
					"properties": {
						"binary": {"type": "string", "description": "Path to the service binary"},
						"name": {"type": "string", "description": "Service name (default: myapp)"},
						"port": {"type": "integer", "description": "Port number (default: 8080)"},
						"protocol": {"type": "string", "description": "Protocol: http or tcp (default: http)"}
					},
					"required": ["binary"]
				}`),
			},
		},
	}
	return s.resultObj(req.ID, tools)
}

func (s *Server) handleToolsCall(ctx context.Context, req *jsonRPCRequest) *jsonRPCResponse {
	var params toolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return s.errorResp(req.ID, -32602, "invalid params: "+err.Error())
	}

	switch params.Name {
	case "run_test":
		return s.toolRunTest(ctx, req.ID, params.Arguments)
	case "run_single_test":
		return s.toolRunSingleTest(ctx, req.ID, params.Arguments)
	case "list_tests":
		return s.toolListTests(req.ID, params.Arguments)
	case "generate_faults":
		return s.toolGenerateFaults(req.ID, params.Arguments)
	case "init_from_compose":
		return s.toolInitFromCompose(req.ID, params.Arguments)
	case "init_spec":
		return s.toolInitSpec(req.ID, params.Arguments)
	default:
		return s.toolError(req.ID, "unknown tool: "+params.Name)
	}
}

// --- Tool implementations ---

func (s *Server) toolRunTest(ctx context.Context, id interface{}, args json.RawMessage) *jsonRPCResponse {
	var p struct {
		File string `json:"file"`
		Seed *int64 `json:"seed"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return s.toolError(id, "invalid arguments: "+err.Error())
	}

	result, err := s.executeTests(ctx, p.File, "", p.Seed)
	if err != nil {
		return s.toolError(id, err.Error())
	}

	data, _ := json.MarshalIndent(result, "", "  ")
	return s.toolResult(id, string(data))
}

func (s *Server) toolRunSingleTest(ctx context.Context, id interface{}, args json.RawMessage) *jsonRPCResponse {
	var p struct {
		File string `json:"file"`
		Test string `json:"test"`
		Seed *int64 `json:"seed"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return s.toolError(id, "invalid arguments: "+err.Error())
	}

	result, err := s.executeTests(ctx, p.File, p.Test, p.Seed)
	if err != nil {
		return s.toolError(id, err.Error())
	}

	data, _ := json.MarshalIndent(result, "", "  ")
	return s.toolResult(id, string(data))
}

func (s *Server) toolListTests(id interface{}, args json.RawMessage) *jsonRPCResponse {
	var p struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return s.toolError(id, "invalid arguments: "+err.Error())
	}

	rt := star.New(s.logger)
	if err := rt.LoadFile(p.File); err != nil {
		return s.toolError(id, "load: "+err.Error())
	}

	tests := rt.DiscoverTests()
	type testInfo struct {
		Name string `json:"name"`
	}
	var list []testInfo
	for _, t := range tests {
		list = append(list, testInfo{Name: t})
	}

	data, _ := json.MarshalIndent(map[string]interface{}{
		"file":  p.File,
		"tests": list,
		"count": len(list),
	}, "", "  ")
	return s.toolResult(id, string(data))
}

func (s *Server) toolGenerateFaults(id interface{}, args json.RawMessage) *jsonRPCResponse {
	var p struct {
		File     string `json:"file"`
		Scenario string `json:"scenario"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return s.toolError(id, "invalid arguments: "+err.Error())
	}

	rt := star.New(s.logger)
	if err := rt.LoadFile(p.File); err != nil {
		return s.toolError(id, "load: "+err.Error())
	}

	analysis, err := generate.Analyze(rt)
	if err != nil {
		return s.toolError(id, "analyze: "+err.Error())
	}

	type genResult struct {
		File      string      `json:"file"`
		Scenarios int         `json:"scenarios"`
		Services  int         `json:"services"`
		Analysis  interface{} `json:"analysis"`
	}

	data, _ := json.MarshalIndent(genResult{
		File:      p.File,
		Scenarios: len(analysis.Scenarios),
		Services:  len(analysis.Services),
		Analysis:  analysis,
	}, "", "  ")
	return s.toolResult(id, string(data))
}

func (s *Server) toolInitFromCompose(id interface{}, args json.RawMessage) *jsonRPCResponse {
	var p struct {
		ComposeFile string `json:"compose_file"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return s.toolError(id, "invalid arguments: "+err.Error())
	}

	services, err := compose.Parse(p.ComposeFile)
	if err != nil {
		return s.toolError(id, err.Error())
	}

	spec := compose.GenerateSpec(services)
	return s.toolResult(id, spec)
}

func (s *Server) toolInitSpec(id interface{}, args json.RawMessage) *jsonRPCResponse {
	var p struct {
		Binary   string `json:"binary"`
		Name     string `json:"name"`
		Port     int    `json:"port"`
		Protocol string `json:"protocol"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return s.toolError(id, "invalid arguments: "+err.Error())
	}

	if p.Name == "" {
		p.Name = "myapp"
	}
	if p.Port == 0 {
		p.Port = 8080
	}
	if p.Protocol == "" {
		p.Protocol = "http"
	}

	healthcheck := fmt.Sprintf(`%s("localhost:%d")`, p.Protocol, p.Port)
	if p.Protocol == "http" {
		healthcheck = fmt.Sprintf(`http("localhost:%d/health")`, p.Port)
	}

	spec := fmt.Sprintf(`# faultbox.star — generated by faultbox mcp
%s = service("%s",
    "%s",
    interface("main", "%s", %d),
    env={"PORT": "%d"},
    healthcheck=%s,
)

def test_health():
    resp = %s.main.get(path="/health") if "%s" == "http" else %s.main.send(data="PING")
    assert_true(True, "%s responds")
`, p.Name, p.Name, p.Binary, p.Protocol, p.Port, p.Port, healthcheck,
		p.Name, p.Protocol, p.Name, p.Name)

	return s.toolResult(id, spec)
}

// --- Test execution ---

func (s *Server) executeTests(ctx context.Context, file, filter string, seed *int64) (*star.TraceOutput, error) {
	rt := star.New(s.logger)
	rt.ServiceStdout = os.Stderr // keep stdout clean for JSON-RPC

	if err := rt.LoadFile(file); err != nil {
		return nil, fmt.Errorf("load %s: %w", file, err)
	}

	rcfg := star.RunConfig{
		Filter: filter,
	}
	if seed != nil {
		u := uint64(*seed)
		rcfg.Seed = &u
	}

	result, err := rt.RunAll(ctx, rcfg)
	if err != nil {
		return nil, fmt.Errorf("run tests: %w", err)
	}

	out := star.BuildTraceOutput(file, result)
	return &out, nil
}

// --- Response helpers ---

func (s *Server) send(resp *jsonRPCResponse) {
	data, _ := json.Marshal(resp)
	fmt.Fprintf(os.Stdout, "%s\n", data)
}

func (s *Server) sendError(id interface{}, code int, msg string) {
	s.send(&jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonRPCError{Code: code, Message: msg},
	})
}

func (s *Server) result(id interface{}, raw json.RawMessage) *jsonRPCResponse {
	return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: raw}
}

func (s *Server) resultObj(id interface{}, v interface{}) *jsonRPCResponse {
	data, _ := json.Marshal(v)
	return s.result(id, data)
}

func (s *Server) errorResp(id interface{}, code int, msg string) *jsonRPCResponse {
	return &jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonRPCError{Code: code, Message: msg},
	}
}

func (s *Server) toolResult(id interface{}, text string) *jsonRPCResponse {
	result := toolCallResult{
		Content: []contentBlock{{Type: "text", Text: text}},
	}
	return s.resultObj(id, result)
}

func (s *Server) toolError(id interface{}, msg string) *jsonRPCResponse {
	result := toolCallResult{
		Content: []contentBlock{{Type: "text", Text: "error: " + msg}},
		IsError: true,
	}
	return s.resultObj(id, result)
}

