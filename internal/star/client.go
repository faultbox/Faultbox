package star

import (
	"context"
	"crypto/tls"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.starlark.net/starlark"

	"github.com/faultbox/Faultbox/internal/protocol"
)

// RFC-055 clients. A ClientVal is a named caller bound to a service
// interface, with its callable surface generated from an API contract.
//
// Two things make it more than sugar over step methods:
//
//  1. Calls are contract-checked. Parameters bind by name against the
//     declared operation; responses can be validated against the schema
//     the service publishes.
//  2. The client is its own trace actor. Events carry `service = <client
//     name>`, which is what the event log keys lanes and vector clocks on,
//     so N logical callers show up as N participants rather than one
//     anonymous `test` driver.

// isCallEventType reports whether an event type is one half of a
// request/response pair. step_send/step_recv (test-driver steps) and
// client_call/client_return (RFC-055 clients) carry the same field
// schema, so consumers that reason about "a call and its reply" —
// assertion context, the report's lane rendering, severity scoring —
// treat both pairs alike rather than growing a parallel branch.
func isCallEventType(t string) bool {
	switch t {
	case "step_send", "step_recv", "client_call", "client_return":
		return true
	}
	return false
}

// clientReservedAttrs are the ClientVal attributes that are not
// operations. An operation normalizing to one of these would be
// unreachable, so client() rejects it at load time.
var clientReservedAttrs = map[string]string{
	"name":       "the client's trace identity",
	"target":     "the bound interface",
	"contract":   "the contract identity string",
	"operations": "the list of generated operation names",
	"call":       "the by-contract-name escape hatch",
}

// ValidateMode governs contract checking on a client's calls.
type ValidateMode string

const (
	// ValidateOff performs no schema checks — byte-identical behaviour to
	// a hand-written step method.
	ValidateOff ValidateMode = "off"
	// ValidateRequest checks outgoing requests. A failure is a spec error:
	// the test asked for something the contract doesn't describe.
	ValidateRequest ValidateMode = "request"
	// ValidateResponse checks responses and records the verdict on the
	// Response without raising — a contract violation under fault is
	// usually the finding, not a harness error.
	ValidateResponse ValidateMode = "response"
	// ValidateStrict checks both and raises on a response violation.
	ValidateStrict ValidateMode = "strict"
)

func (m ValidateMode) checksRequest() bool {
	return m == ValidateRequest || m == ValidateStrict
}

func (m ValidateMode) checksResponse() bool {
	return m == ValidateResponse || m == ValidateStrict
}

// ClientVal is the Starlark value produced by client().
type ClientVal struct {
	Name   string
	Target *InterfaceRef
	Table  *protocol.OperationTable

	BasePath string
	Headers  map[string]string
	Before   starlark.Callable
	Validate ValidateMode
	Timeout  time.Duration

	runtime *Runtime
	frozen  bool
}

var _ starlark.Value = (*ClientVal)(nil)
var _ starlark.HasAttrs = (*ClientVal)(nil)

func (c *ClientVal) String() string {
	return fmt.Sprintf("<client %s → %s.%s (%d operations)>",
		c.Name, c.Target.Service.Name, c.Target.Interface.Name, c.Table.Len())
}
func (c *ClientVal) Type() string          { return "client" }
func (c *ClientVal) Freeze()               { c.frozen = true }
func (c *ClientVal) Truth() starlark.Bool  { return true }
func (c *ClientVal) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: client") }

func (c *ClientVal) Attr(name string) (starlark.Value, error) {
	switch name {
	case "name":
		return starlark.String(c.Name), nil
	case "target":
		return c.Target, nil
	case "contract":
		return starlark.String(c.Table.Contract.String()), nil
	case "operations":
		names := c.Table.Names()
		items := make([]starlark.Value, len(names))
		for i, n := range names {
			items[i] = starlark.String(n)
		}
		return starlark.NewList(items), nil
	case "call":
		return starlark.NewBuiltin("call", c.builtinCall), nil
	}

	op, err := c.Table.Resolve(c.Name, name)
	if err != nil {
		// NoSuchAttrError so `hasattr()` and Starlark's own diagnostics
		// behave, but keep our suggestion text — the resolver's message is
		// the whole point of typing the client.
		return nil, starlark.NoSuchAttrError(err.Error())
	}
	return &OperationVal{client: c, op: op}, nil
}

func (c *ClientVal) AttrNames() []string {
	names := c.Table.Names()
	for k := range clientReservedAttrs {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// builtinCall backs client.call("<contract-native name>", params={...}, ...).
//
// The escape hatch for operations whose canonical name can't be spelled as
// an attribute — a parameter that collides with a reserved kwarg, or a
// caller who'd rather paste the operationId straight from the document.
// Parameters go in an explicit `params=` dict so they can't collide with
// the reserved call-site kwargs.
func (c *ClientVal) builtinCall(thread *starlark.Thread, _ *starlark.Builtin,
	args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {

	var name string
	var params *starlark.Dict
	var body starlark.Value
	var headers *starlark.Dict
	var timeout string
	if err := starlark.UnpackArgs("call", args, kwargs,
		"name", &name,
		"params?", &params,
		"body?", &body,
		"headers?", &headers,
		"timeout?", &timeout,
	); err != nil {
		return nil, err
	}

	op, ok := c.Table.LookupContractName(name)
	if !ok {
		// Fall back to the canonical name so call() accepts either
		// spelling — users reach for it when an attribute won't work, and
		// making them remember which name form applies is a papercut.
		resolved, err := c.Table.Resolve(c.Name, name)
		if err != nil {
			return nil, fmt.Errorf("client %q call(%q): no such operation\n"+
				"  call() accepts an operationId / method path, or a generated name", c.Name, name)
		}
		op = resolved
	}

	callArgs := make(map[string]any)
	if params != nil {
		for _, pair := range params.Items() {
			key, ok := pair[0].(starlark.String)
			if !ok {
				return nil, fmt.Errorf("client %q call(%q): params keys must be strings (got %s)",
					c.Name, name, pair[0].Type())
			}
			native, err := starlarkToGo(pair[1])
			if err != nil {
				return nil, fmt.Errorf("client %q call(%q): params[%q]: %w", c.Name, name, string(key), err)
			}
			callArgs[string(key)] = native
		}
	}
	if body != nil {
		native, err := starlarkToGo(body)
		if err != nil {
			return nil, fmt.Errorf("client %q call(%q): body=: %w", c.Name, name, err)
		}
		callArgs["body"] = native
	}
	if headers != nil {
		native, err := starlarkToGo(headers)
		if err != nil {
			return nil, fmt.Errorf("client %q call(%q): headers=: %w", c.Name, name, err)
		}
		callArgs["headers"] = native
	}
	if timeout != "" {
		callArgs["timeout"] = timeout
	}

	return c.runtime.executeClientCall(thread, c, op, callArgs)
}

// OperationVal is one generated, callable operation on a client.
type OperationVal struct {
	client *ClientVal
	op     *protocol.Operation
}

var _ starlark.Value = (*OperationVal)(nil)
var _ starlark.Callable = (*OperationVal)(nil)
var _ starlark.HasAttrs = (*OperationVal)(nil)

func (o *OperationVal) String() string {
	return fmt.Sprintf("<operation %s.%s → %s>", o.client.Name, o.op.Name, o.op.Wire())
}
func (o *OperationVal) Type() string          { return "operation" }
func (o *OperationVal) Freeze()               {}
func (o *OperationVal) Truth() starlark.Bool  { return true }
func (o *OperationVal) Hash() (uint32, error) { return 0, fmt.Errorf("unhashable: operation") }
func (o *OperationVal) Name() string          { return o.op.Name }

// Attr exposes the operation's contract metadata. Useful for building
// assertions and for `print()`-driven exploration of an unfamiliar API.
func (o *OperationVal) Attr(name string) (starlark.Value, error) {
	switch name {
	case "name":
		return starlark.String(o.op.Name), nil
	case "contract_name":
		return starlark.String(o.op.ContractName), nil
	case "wire":
		return starlark.String(o.op.Wire()), nil
	case "signature":
		return starlark.String(o.op.SignatureHint()), nil
	case "params":
		items := make([]starlark.Value, 0, len(o.op.Params))
		for _, p := range o.op.Params {
			items = append(items, starlark.String(p.Name))
		}
		return starlark.NewList(items), nil
	}
	return nil, starlark.NoSuchAttrError(fmt.Sprintf("operation has no .%s attribute", name))
}

func (o *OperationVal) AttrNames() []string {
	return []string{"contract_name", "name", "params", "signature", "wire"}
}

// CallInternal binds kwargs and performs the call.
//
// Positional arguments are refused: contract parameters are a named set
// with no meaningful order, and accepting positions would silently break
// the moment the contract declares a new one.
func (o *OperationVal) CallInternal(thread *starlark.Thread,
	args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {

	if len(args) > 0 {
		return nil, fmt.Errorf("%s takes keyword arguments only (got %d positional)\n  %s",
			o.op.Name, len(args), o.op.SignatureHint())
	}

	callArgs := make(map[string]any, len(kwargs))
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		native, err := starlarkToGo(kv[1])
		if err != nil {
			return nil, fmt.Errorf("%s: argument %q: %w", o.op.SignatureHint(), key, err)
		}
		callArgs[key] = native
	}
	return o.client.runtime.executeClientCall(thread, o.client, o.op, callArgs)
}

// executeClientCall runs one contract-bound call and records it in the
// trace as the client's own activity.
func (rt *Runtime) executeClientCall(thread *starlark.Thread, c *ClientVal,
	op *protocol.Operation, args map[string]any) (starlark.Value, error) {

	timeout := c.Timeout
	if raw, ok := args["timeout"]; ok {
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("%s: timeout= must be a duration string like \"5s\"", op.SignatureHint())
		}
		d, err := parseStarDuration(s)
		if err != nil {
			return nil, fmt.Errorf("%s: timeout=%q: %w", op.SignatureHint(), s, err)
		}
		timeout = d
		delete(args, "timeout")
	}

	addr := rt.clientAddr(c)
	specCaller := rt.callerInTest(thread)

	// The test body caused this call: record that edge before the client
	// advances its own clock, or the client's lane reads as an independent
	// process spontaneously emitting requests (RFC-055 §5.5).
	rt.events.MergeClock(c.Name, "test")

	switch {
	case op.FullMethod != "":
		return rt.executeGRPCClientCall(c, op, args, addr, specCaller, timeout)
	default:
		return rt.executeHTTPClientCall(c, op, args, addr, specCaller, timeout)
	}
}

// clientAddr resolves where a client's calls go: the proxy listener when
// one is up for the target interface, otherwise the interface's own host
// address. Identical resolution to executeStep, so a client is the same
// caller as a step method as far as fault injection is concerned.
func (rt *Runtime) clientAddr(c *ClientVal) string {
	iface := c.Target.Interface
	port := iface.Port
	if iface.HostPort > 0 {
		port = iface.HostPort
	}
	addr := fmt.Sprintf("localhost:%d", port)
	if proxyAddr := rt.proxyMgr.GetProxyAddr(c.Target.Service.Name, iface.Name); proxyAddr != "" {
		addr = proxyAddr
	}
	return addr
}

func (rt *Runtime) executeHTTPClientCall(c *ClientVal, op *protocol.Operation, args map[string]any,
	addr, specCaller string, timeout time.Duration) (starlark.Value, error) {

	req, err := c.Table.BuildHTTPRequest(op, args, c.BasePath, c.Headers)
	if err != nil {
		return nil, fmt.Errorf("client %q: %w", c.Name, err)
	}

	if c.Before != nil {
		if err := rt.applyBeforeHook(c, op, req); err != nil {
			return nil, err
		}
	}

	if c.Validate.checksRequest() {
		if err := c.Table.ValidateHTTPRequest(op, req); err != nil {
			return nil, fmt.Errorf("client %q %s: request does not conform to %s: %w",
				c.Name, op.Name, c.Table.Contract.Path, err)
		}
	}

	target := c.Target.Service.Name
	fields := map[string]string{
		"client":    c.Name,
		"target":    target,
		"interface": c.Target.Interface.Name,
		"protocol":  c.Target.Interface.Protocol,
		"operation": op.Name,
		"contract":  c.Table.Contract.String(),
		"method":    req.Method,
		"path":      truncate(req.Path, 200),
		"params":    truncate(formatCallParams(op, args), 200),
		"summary":   fmt.Sprintf("→ %s.%s %s %s", c.Name, op.Name, req.Method, truncate(req.Path, 120)),
	}
	if len(req.Body) > 0 {
		fields["body"] = truncate(string(req.Body), 200)
	}
	if specCaller != "" {
		fields["spec"] = specCaller
	}
	rt.events.MergeClock(target, c.Name)
	rt.events.Emit("client_call", c.Name, fields)

	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	res, err := protocol.ExecuteHTTPRequest(ctx, addr, req)
	if err != nil {
		return nil, fmt.Errorf("client %q %s: %w", c.Name, op.Name, err)
	}

	rt.events.MergeClock(c.Name, target)

	resp := &Response{
		Status:     res.StatusCode,
		Body:       res.Body,
		DurationMs: res.DurationMs,
		Ok:         res.Success,
		Error:      res.Error,
		Client:     c.Name,
		Operation:  op.Name,
		ContractOk: true,
	}

	var violation error
	if c.Validate.checksResponse() && res.Success {
		violation = c.Table.ValidateHTTPResponse(op, res.StatusCode,
			protocol.ResponseContentType(res), []byte(res.Body))
		if violation != nil {
			resp.ContractOk = false
			resp.ContractError = violation.Error()
		}
	}

	rt.emitClientReturn(c, op, target, specCaller, resp, map[string]string{
		"status_code": fmt.Sprintf("%d", res.StatusCode),
		"summary": fmt.Sprintf("← %s.%s %s (%dms)", c.Name, op.Name,
			httpOutcome(res.StatusCode, res.Success, res.Error), res.DurationMs),
	}, errorBodyField(res.StatusCode, res.Body))

	if violation != nil {
		rt.emitContractViolation(c, op, target, specCaller, violation)
		if c.Validate == ValidateStrict {
			return nil, fmt.Errorf("client %q %s: response does not conform to %s: %w",
				c.Name, op.Name, c.Table.Contract.Path, violation)
		}
	}

	// The test driver observes the client's result: merge back so
	// assertions after the call are causally downstream of it.
	rt.events.MergeClock("test", c.Name)
	return resp, nil
}

func (rt *Runtime) executeGRPCClientCall(c *ClientVal, op *protocol.Operation, args map[string]any,
	addr, specCaller string, timeout time.Duration) (starlark.Value, error) {

	req, err := c.Table.BuildGRPCRequest(op, args)
	if err != nil {
		return nil, fmt.Errorf("client %q: %w", c.Name, err)
	}

	headers := make(map[string]string, len(c.Headers))
	for k, v := range c.Headers {
		headers[k] = v
	}
	if raw, ok := args["headers"]; ok {
		extra, err := stringMapFromNative(raw)
		if err != nil {
			return nil, fmt.Errorf("client %q %s: headers=: %w", c.Name, op.Name, err)
		}
		for k, v := range extra {
			headers[k] = v
		}
	}

	target := c.Target.Service.Name
	fields := map[string]string{
		"client":      c.Name,
		"target":      target,
		"interface":   c.Target.Interface.Name,
		"protocol":    c.Target.Interface.Protocol,
		"operation":   op.Name,
		"contract":    c.Table.Contract.String(),
		"method_path": op.FullMethod,
		"params":      truncate(formatCallParams(op, args), 200),
		"summary":     fmt.Sprintf("→ %s.%s %s", c.Name, op.Name, op.FullMethod),
	}
	if len(req.JSON) > 0 && string(req.JSON) != "{}" {
		fields["body"] = truncate(string(req.JSON), 200)
	}
	if specCaller != "" {
		fields["spec"] = specCaller
	}
	rt.events.MergeClock(target, c.Name)
	rt.events.Emit("client_call", c.Name, fields)

	var tlsCfg *tls.Config
	res, err := protocol.InvokeGRPCUnary(context.Background(), addr, op, req, headers, tlsCfg, timeout)
	if err != nil {
		return nil, fmt.Errorf("client %q %s: %w", c.Name, op.Name, err)
	}

	rt.events.MergeClock(c.Name, target)

	ok := res.Code == 0 // codes.OK
	resp := &Response{
		Body:       string(res.JSON),
		DurationMs: res.DurationMs,
		Ok:         ok,
		Client:     c.Name,
		Operation:  op.Name,
		ContractOk: true,
	}
	if !ok {
		resp.Error = fmt.Sprintf("%s: %s", res.CodeName, res.Message)
	}

	var violation error
	if c.Validate.checksResponse() {
		violation = c.Table.ValidateGRPCResponse(op, res)
		if violation != nil {
			resp.ContractOk = false
			resp.ContractError = violation.Error()
		}
	}

	extra := map[string]string{
		"grpc_code": res.CodeName,
		"summary":   fmt.Sprintf("← %s.%s %s (%dms)", c.Name, op.Name, res.CodeName, res.DurationMs),
	}
	if res.Message != "" {
		extra["grpc_message"] = truncate(res.Message, 200)
	}
	rt.emitClientReturn(c, op, target, specCaller, resp, extra, "")

	if violation != nil {
		rt.emitContractViolation(c, op, target, specCaller, violation)
		if c.Validate == ValidateStrict {
			return nil, fmt.Errorf("client %q %s: response does not conform to %s: %w",
				c.Name, op.Name, c.Table.Contract.Path, violation)
		}
	}

	rt.events.MergeClock("test", c.Name)
	return resp, nil
}

// emitClientReturn emits the client_return event. Field names are shared
// with step_recv wherever the concept already exists (status_code,
// duration_ms, success, error, body, summary, spec) so downstream
// consumers need one allowlist entry rather than a parallel schema.
func (rt *Runtime) emitClientReturn(c *ClientVal, op *protocol.Operation, target, specCaller string,
	resp *Response, extra map[string]string, bodyField string) {

	fields := map[string]string{
		"client":      c.Name,
		"target":      target,
		"operation":   op.Name,
		"duration_ms": fmt.Sprintf("%d", resp.DurationMs),
		"success":     fmt.Sprintf("%t", resp.Ok),
		"contract_ok": fmt.Sprintf("%t", resp.ContractOk),
	}
	for k, v := range extra {
		fields[k] = v
	}
	if resp.Error != "" {
		fields["error"] = truncate(resp.Error, 200)
	}
	if resp.ContractError != "" {
		fields["contract_error"] = truncate(resp.ContractError, 200)
	}
	if bodyField != "" {
		fields["body"] = bodyField
	}
	if specCaller != "" {
		fields["spec"] = specCaller
	}
	rt.events.Emit("client_return", c.Name, fields)
}

// emitContractViolation records a conformance failure as its own event so
// it is greppable, matchable as an anchor, and visible in the report
// without the reader having to notice a field on a return event.
func (rt *Runtime) emitContractViolation(c *ClientVal, op *protocol.Operation,
	target, specCaller string, violation error) {

	fields := map[string]string{
		"client":    c.Name,
		"target":    target,
		"operation": op.Name,
		"contract":  c.Table.Contract.String(),
		"detail":    truncate(violation.Error(), 400),
		"summary":   fmt.Sprintf("%s: %s", op.Wire(), truncate(violation.Error(), 160)),
	}
	if specCaller != "" {
		fields["spec"] = specCaller
	}
	rt.events.Emit("contract_violation", c.Name, fields)
}

// applyBeforeHook runs the client's before= callable and folds the result
// back into the request. v1 reads `headers` and `body` off the returned
// dict; everything else is ignored so the hook can pass the request
// through unchanged.
func (rt *Runtime) applyBeforeHook(c *ClientVal, op *protocol.Operation, req *protocol.HTTPRequest) error {
	reqDict := starlark.NewDict(5)
	_ = reqDict.SetKey(starlark.String("operation"), starlark.String(op.Name))
	_ = reqDict.SetKey(starlark.String("method"), starlark.String(req.Method))
	_ = reqDict.SetKey(starlark.String("path"), starlark.String(req.Path))
	_ = reqDict.SetKey(starlark.String("body"), starlark.String(string(req.Body)))
	headerDict := starlark.NewDict(len(req.Headers))
	for k, v := range req.Headers {
		_ = headerDict.SetKey(starlark.String(k), starlark.String(v))
	}
	_ = reqDict.SetKey(starlark.String("headers"), headerDict)

	thread := &starlark.Thread{Name: "client-before-" + c.Name}
	out, err := starlark.Call(thread, c.Before, starlark.Tuple{reqDict}, nil)
	if err != nil {
		return fmt.Errorf("client %q before= hook: %w", c.Name, err)
	}
	outDict, ok := out.(*starlark.Dict)
	if !ok {
		return fmt.Errorf("client %q before= hook must return a dict (got %s)", c.Name, out.Type())
	}

	if hv, found, _ := outDict.Get(starlark.String("headers")); found {
		hd, ok := hv.(*starlark.Dict)
		if !ok {
			return fmt.Errorf("client %q before= hook: headers must be a dict (got %s)", c.Name, hv.Type())
		}
		for _, pair := range hd.Items() {
			k, kok := starlark.AsString(pair[0])
			v, vok := starlark.AsString(pair[1])
			if !kok || !vok {
				return fmt.Errorf("client %q before= hook: header keys and values must be strings", c.Name)
			}
			req.Headers[k] = v
		}
	}
	if bv, found, _ := outDict.Get(starlark.String("body")); found {
		s, ok := starlark.AsString(bv)
		if !ok {
			return fmt.Errorf("client %q before= hook: body must be a string (got %s)", c.Name, bv.Type())
		}
		req.Body = []byte(s)
	}
	return nil
}

// formatCallParams renders the bound arguments for the trace, in the
// operation's declared parameter order so the same call always produces
// the same string. Reserved kwargs are excluded — the body has its own
// field and headers are noise in a one-line summary.
func formatCallParams(op *protocol.Operation, args map[string]any) string {
	var parts []string
	for _, p := range op.Params {
		if v, ok := args[p.Name]; ok {
			parts = append(parts, fmt.Sprintf("%s=%v", p.Name, v))
		}
	}
	return strings.Join(parts, ", ")
}

// httpOutcome renders a one-word result for the return event's summary.
func httpOutcome(status int, success bool, errMsg string) string {
	if !success {
		if errMsg != "" {
			return "failed"
		}
		return "failed"
	}
	return fmt.Sprintf("%d", status)
}

// errorBodyField reuses the step_recv rule: record the response body on
// non-2xx only, capped, so debugging a 500 reads off the trace without
// bloating bundles with successful payloads.
func errorBodyField(status int, body string) string {
	if b, ok := errorBodyForTrace(status, body); ok {
		return b
	}
	return ""
}

// stringMapFromNative coerces a converted Starlark dict into a string map.
func stringMapFromNative(v any) (map[string]string, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected a dict, got %T", v)
	}
	out := make(map[string]string, len(m))
	for k, raw := range m {
		s, ok := raw.(string)
		if !ok {
			s = fmt.Sprintf("%v", raw)
		}
		out[k] = s
	}
	return out, nil
}
