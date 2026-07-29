package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// OpenAPI half of RFC-055 clients: turn a loaded OpenAPI document into an
// OperationTable, bind caller kwargs onto the wire, and check responses
// against the contract.
//
// This is the caller-side mirror of openapi.go's GenerateRoutes, which does
// the same walk to produce mock responses. Both read the same
// *OpenAPISpec; neither knows about the other.

// openapiOpRef is the format-specific payload hanging off an Operation
// built from an OpenAPI document.
type openapiOpRef struct {
	op *openapi3.Operation
	// contentType is the request body media type, chosen once at build
	// time (application/json preferred). Empty when no body is declared.
	contentType string
}

// HTTPRequest is a fully-bound HTTP call, ready for the transport.
type HTTPRequest struct {
	Method string
	// Path is the resolved path plus encoded query string.
	Path        string
	Headers     map[string]string
	Body        []byte
	ContentType string
}

// BuildOpenAPIOperations walks an OpenAPI document and produces the
// operation table a client exposes.
//
// rename maps a contract-native operation name (operationId, or the
// synthesized stand-in for operations that declare none) to the canonical
// attribute the spec author wants. Renames are applied before collision
// detection, so they are the documented fix for a collision. A rename key
// that matches no operation is an error — a silently-ignored rename would
// leave the author believing they'd fixed something.
func BuildOpenAPIOperations(spec *OpenAPISpec, rename map[string]string) (*OperationTable, error) {
	if spec == nil || spec.Doc == nil {
		return nil, fmt.Errorf("nil OpenAPI spec")
	}
	if spec.Doc.Paths == nil || spec.Doc.Paths.Len() == 0 {
		return nil, fmt.Errorf("OpenAPI document %s declares no paths", spec.Path)
	}

	info := ContractInfo{Kind: ContractOpenAPI, Path: spec.Path}
	if spec.Doc.Info != nil {
		info.Version = spec.Doc.Info.Version
	}
	table := newOperationTable(info)

	// Deterministic walk: sorted paths, canonical method order.
	pathKeys := make([]string, 0, spec.Doc.Paths.Len())
	for k := range spec.Doc.Paths.Map() {
		pathKeys = append(pathKeys, k)
	}
	sort.Strings(pathKeys)

	usedRenames := make(map[string]bool, len(rename))

	for _, p := range pathKeys {
		item := spec.Doc.Paths.Value(p)
		if item == nil {
			continue
		}
		for _, method := range httpMethodsOf(item) {
			op := operationFor(item, method)
			if op == nil {
				continue
			}
			built, err := buildOpenAPIOperation(method, p, item, op, rename, usedRenames)
			if err != nil {
				return nil, err
			}
			if err := table.add(built); err != nil {
				return nil, err
			}
		}
	}

	for key := range rename {
		if !usedRenames[key] {
			return nil, fmt.Errorf(
				"rename key %q matches no operation in %s\n"+
					"  rename keys are operationIds (or the synthesized name shown by `faultbox inspect --clients`)",
				key, spec.Path)
		}
	}

	if table.Len() == 0 {
		return nil, fmt.Errorf("OpenAPI document %s declares no operations", spec.Path)
	}
	table.finalize()
	return table, nil
}

func buildOpenAPIOperation(method, pathTemplate string, item *openapi3.PathItem, op *openapi3.Operation,
	rename map[string]string, usedRenames map[string]bool) (*Operation, error) {

	contractName := op.OperationID
	if contractName == "" {
		contractName = synthesizeOperationID(method, pathTemplate)
	}

	canonical := ""
	if target, ok := rename[contractName]; ok {
		usedRenames[contractName] = true
		c, err := CanonicalName(target)
		if err != nil {
			return nil, fmt.Errorf("rename target %q for operation %q: %w", target, contractName, err)
		}
		canonical = c
	} else {
		c, err := CanonicalName(contractName)
		if err != nil {
			return nil, fmt.Errorf("operation %s %s: %w", method, pathTemplate, err)
		}
		canonical = c
	}

	built := &Operation{
		Name:         canonical,
		ContractName: contractName,
		Method:       method,
		PathTemplate: pathTemplate,
		Summary:      op.Summary,
		openapi:      &openapiOpRef{op: op},
	}

	params, err := collectOpenAPIParams(contractName, item, op)
	if err != nil {
		return nil, err
	}
	built.Params = params

	if op.RequestBody != nil && op.RequestBody.Value != nil {
		built.AcceptsBody = true
		built.BodyRequired = op.RequestBody.Value.Required
		if _, ct, ok := pickMediaType(op.RequestBody.Value.Content); ok {
			built.openapi.contentType = ct
		} else {
			built.openapi.contentType = "application/json"
		}
	}

	return built, nil
}

// collectOpenAPIParams merges path-item-level parameters (shared by every
// operation on that path) with operation-level ones. Operation-level wins
// on a (name, in) clash, per the OpenAPI spec.
func collectOpenAPIParams(contractName string, item *openapi3.PathItem, op *openapi3.Operation) ([]Param, error) {
	type key struct{ name, in string }
	merged := make(map[key]*openapi3.Parameter)

	for _, ref := range item.Parameters {
		if ref == nil || ref.Value == nil {
			continue
		}
		merged[key{ref.Value.Name, ref.Value.In}] = ref.Value
	}
	for _, ref := range op.Parameters {
		if ref == nil || ref.Value == nil {
			continue
		}
		merged[key{ref.Value.Name, ref.Value.In}] = ref.Value
	}

	out := make([]Param, 0, len(merged))
	for k, p := range merged {
		canonical, err := CanonicalName(p.Name)
		if err != nil {
			return nil, fmt.Errorf("operation %q parameter %q: %w", contractName, p.Name, err)
		}
		if err := checkReservedParamName(contractName, p.Name, canonical); err != nil {
			return nil, err
		}
		loc := ParamLocation(k.in)
		out = append(out, Param{
			Name:        canonical,
			WireName:    p.Name,
			In:          loc,
			Required:    p.Required || loc == ParamPath, // path params are always required
			Description: p.Description,
		})
	}

	// Two distinct wire names can normalize to the same kwarg (`order-id`
	// in the query and `orderId` in the path). Binding would be ambiguous,
	// so refuse at build time.
	seen := make(map[string]Param, len(out))
	for _, p := range out {
		if prev, dup := seen[p.Name]; dup {
			return nil, fmt.Errorf(
				"operation %q: parameters %q (%s) and %q (%s) both normalize to kwarg %q",
				contractName, prev.WireName, prev.In, p.WireName, p.In, p.Name)
		}
		seen[p.Name] = p
	}

	sortParams(out)
	return out, nil
}

// sortParams orders parameters path → query → header → cookie, then by
// canonical name. Stable ordering keeps inspect output and signature hints
// reproducible.
func sortParams(ps []Param) {
	rank := map[ParamLocation]int{ParamPath: 0, ParamQuery: 1, ParamHeader: 2, ParamCookie: 3, ParamField: 4}
	sort.SliceStable(ps, func(i, j int) bool {
		ri, rj := rank[ps[i].In], rank[ps[j].In]
		if ri != rj {
			return ri < rj
		}
		return ps[i].Name < ps[j].Name
	})
}

// synthesizeOperationID builds a stable contract-native name for an
// operation that declares no operationId — common in real-world documents.
//
// The rule (RFC-055 §5.2): method, then each path segment, with `{param}`
// placeholders contributing the parameter's *name* rather than a
// positional marker. `GET /orders/{orderId}/items` → "get orders orderId
// items" → canonicalized downstream to `get_orders_order_id_items`.
// Renaming a literal segment changes the name; that's intended — the
// generated name tracks the path it describes.
func synthesizeOperationID(method, pathTemplate string) string {
	parts := []string{strings.ToLower(method)}
	for _, seg := range splitPathSegments(pathTemplate) {
		seg = strings.TrimSuffix(strings.TrimPrefix(seg, "{"), "}")
		if seg == "" {
			continue
		}
		parts = append(parts, seg)
	}
	return strings.Join(parts, "_")
}

// BuildHTTPRequest binds caller-supplied arguments onto the operation's
// wire form.
//
// args carries the call-site kwargs: one entry per declared parameter
// (keyed by canonical name), plus the reserved `body` and `headers` keys.
// Unknown keys are an error — the same strict-kwarg stance the proxy fault
// builtins adopted in v0.13.2, and the whole point of a typed client.
//
// basePath is prepended to the operation's path; defaultHeaders are the
// client-level headers, which per-call `headers=` overrides.
func (t *OperationTable) BuildHTTPRequest(op *Operation, args map[string]any,
	basePath string, defaultHeaders map[string]string) (*HTTPRequest, error) {

	if op.openapi == nil {
		return nil, fmt.Errorf("operation %q is not an HTTP operation", op.Name)
	}

	byName := make(map[string]Param, len(op.Params))
	for _, p := range op.Params {
		byName[p.Name] = p
	}

	// Reject unknown kwargs before doing any work, so the error names the
	// offending key rather than surfacing as a missing-required-param
	// complaint later.
	for k := range args {
		if _, ok := byName[k]; ok {
			continue
		}
		if _, reserved := reservedKwargs[k]; reserved {
			continue
		}
		return nil, unknownArgError(op, k, byName)
	}

	if _, ok := args["body"]; !ok && op.AcceptsBody && op.BodyRequired {
		return nil, fmt.Errorf("%s: body= is required", op.SignatureHint())
	}
	if _, ok := args["body"]; ok && !op.AcceptsBody {
		return nil, fmt.Errorf("%s: operation %s declares no request body", op.SignatureHint(), op.Wire())
	}

	resolvedPath := op.PathTemplate
	query := url.Values{}
	headers := make(map[string]string, len(defaultHeaders)+len(op.Params))
	for k, v := range defaultHeaders {
		headers[k] = v
	}
	var cookies []string

	for _, p := range op.Params {
		raw, provided := args[p.Name]
		if !provided {
			if p.Required {
				return nil, fmt.Errorf("%s: missing required %s parameter %q", op.SignatureHint(), p.In, p.Name)
			}
			continue
		}
		// Query parameters accept lists (repeated key); everything else
		// takes a single scalar.
		if list, isList := raw.([]any); isList {
			if p.In != ParamQuery {
				return nil, fmt.Errorf("%s: parameter %q is a %s parameter and cannot take a list",
					op.SignatureHint(), p.Name, p.In)
			}
			for _, item := range list {
				s, err := paramToString(item)
				if err != nil {
					return nil, fmt.Errorf("%s: parameter %q: %w", op.SignatureHint(), p.Name, err)
				}
				query.Add(p.WireName, s)
			}
			continue
		}
		s, err := paramToString(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: parameter %q: %w", op.SignatureHint(), p.Name, err)
		}
		switch p.In {
		case ParamPath:
			placeholder := "{" + p.WireName + "}"
			if !strings.Contains(resolvedPath, placeholder) {
				return nil, fmt.Errorf("operation %s declares path parameter %q but its path %q has no %s placeholder",
					op.ContractName, p.WireName, op.PathTemplate, placeholder)
			}
			resolvedPath = strings.ReplaceAll(resolvedPath, placeholder, url.PathEscape(s))
		case ParamQuery:
			query.Set(p.WireName, s)
		case ParamHeader:
			headers[p.WireName] = s
		case ParamCookie:
			cookies = append(cookies, p.WireName+"="+s)
		}
	}

	if rest := remainingPlaceholders(resolvedPath); rest != "" {
		return nil, fmt.Errorf("%s: path %q still has unbound placeholder %s", op.SignatureHint(), op.PathTemplate, rest)
	}

	// Per-call headers override client-level defaults and header params.
	if hv, ok := args["headers"]; ok {
		extra, err := stringMapArg(hv)
		if err != nil {
			return nil, fmt.Errorf("%s: headers=: %w", op.SignatureHint(), err)
		}
		for k, v := range extra {
			headers[k] = v
		}
	}
	if len(cookies) > 0 {
		sort.Strings(cookies)
		if existing := headers["Cookie"]; existing != "" {
			headers["Cookie"] = existing + "; " + strings.Join(cookies, "; ")
		} else {
			headers["Cookie"] = strings.Join(cookies, "; ")
		}
	}

	req := &HTTPRequest{
		Method:  op.Method,
		Path:    joinBasePath(basePath, resolvedPath),
		Headers: headers,
	}
	if q := query.Encode(); q != "" {
		req.Path += "?" + q
	}

	if bv, ok := args["body"]; ok {
		body, ct, err := encodeRequestBody(bv, op.openapi.contentType)
		if err != nil {
			return nil, fmt.Errorf("%s: body=: %w", op.SignatureHint(), err)
		}
		req.Body = body
		req.ContentType = ct
		if _, set := headers["Content-Type"]; !set {
			headers["Content-Type"] = ct
		}
	}

	return req, nil
}

// unknownArgError reports an unrecognized kwarg, suggesting the nearest
// declared parameter when there is one.
func unknownArgError(op *Operation, key string, byName map[string]Param) error {
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)

	best, bestDist := "", 4
	for _, n := range names {
		if d := editDistance(key, n); d < bestDist {
			best, bestDist = n, d
		}
	}
	msg := fmt.Sprintf("%s: unknown argument %q", op.SignatureHint(), key)
	if best != "" {
		msg += fmt.Sprintf(" (did you mean %q?)", best)
	}
	if len(names) > 0 {
		msg += "\n  declared parameters: " + strings.Join(names, ", ")
	} else {
		msg += "\n  operation declares no parameters"
	}
	return fmt.Errorf("%s", msg)
}

// remainingPlaceholders returns the first unbound `{param}` in a path, or
// "" when the path is fully resolved. Guards against a document whose path
// template references a parameter it never declares.
func remainingPlaceholders(p string) string {
	open := strings.IndexByte(p, '{')
	if open < 0 {
		return ""
	}
	close := strings.IndexByte(p[open:], '}')
	if close < 0 {
		return p[open:]
	}
	return p[open : open+close+1]
}

// joinBasePath prefixes a client-level base path onto an operation path,
// collapsing duplicate slashes. An empty base returns the path unchanged.
func joinBasePath(base, p string) string {
	base = strings.TrimSpace(base)
	if base == "" || base == "/" {
		return p
	}
	joined := path.Join("/"+strings.Trim(base, "/"), p)
	// path.Join drops a meaningful trailing slash; restore it.
	if strings.HasSuffix(p, "/") && !strings.HasSuffix(joined, "/") {
		joined += "/"
	}
	return joined
}

// encodeRequestBody renders a body argument into wire bytes. Strings pass
// through verbatim (the escape hatch for hand-crafted payloads); dicts and
// lists are JSON-encoded.
func encodeRequestBody(v any, contentType string) ([]byte, string, error) {
	if contentType == "" {
		contentType = "application/json"
	}
	switch t := v.(type) {
	case string:
		return []byte(t), contentType, nil
	case []byte:
		return t, contentType, nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, "", fmt.Errorf("encode as JSON: %w", err)
		}
		return b, contentType, nil
	}
}

// stringMapArg coerces a headers= argument into a string map.
func stringMapArg(v any) (map[string]string, error) {
	m, ok := v.(map[string]any)
	if !ok {
		if sm, ok := v.(map[string]string); ok {
			return sm, nil
		}
		return nil, fmt.Errorf("expected a dict of string→string, got %T", v)
	}
	out := make(map[string]string, len(m))
	for k, raw := range m {
		s, err := paramToString(raw)
		if err != nil {
			return nil, fmt.Errorf("header %q: %w", k, err)
		}
		out[k] = s
	}
	return out, nil
}

// ExecuteHTTPRequest sends a bound request to addr.
//
// It deliberately routes through the registered http protocol plugin's
// ExecuteStep rather than opening its own transport: the client must be
// byte-for-byte the same caller as a hand-written step method, so proxy
// interception, timeouts, and body limits behave identically whether the
// request came from a contract or from `api.public.get(path=…)`.
//
// The per-call deadline rides on ctx.
func ExecuteHTTPRequest(ctx context.Context, addr string, req *HTTPRequest) (*StepResult, error) {
	p, ok := Get("http")
	if !ok {
		return nil, fmt.Errorf("http protocol plugin is not registered")
	}
	kwargs := map[string]any{"path": req.Path}
	if len(req.Body) > 0 {
		kwargs["body"] = string(req.Body)
	}
	if len(req.Headers) > 0 {
		headers := make(map[string]any, len(req.Headers))
		for k, v := range req.Headers {
			headers[k] = v
		}
		kwargs["headers"] = headers
	}
	return p.ExecuteStep(ctx, addr, req.Method, kwargs)
}

// ResponseContentType extracts the response media type a StepResult
// carried, defaulting to application/json when the server declared none —
// the same assumption ValidateHTTPResponse makes about an empty type.
func ResponseContentType(res *StepResult) string {
	if res == nil || res.Fields == nil {
		return ""
	}
	return res.Fields["content_type"]
}

// ValidateHTTPRequest checks a bound request against the operation's
// declared requestBody schema. Used by validate="request" and "strict".
func (t *OperationTable) ValidateHTTPRequest(op *Operation, req *HTTPRequest) error {
	if op.openapi == nil || op.openapi.op == nil {
		return nil
	}
	oo := op.openapi.op
	if oo.RequestBody == nil || oo.RequestBody.Value == nil {
		return nil
	}
	body := trimTrailingWS(req.Body)
	if len(body) == 0 {
		if oo.RequestBody.Value.Required {
			return fmt.Errorf("request body is required")
		}
		return nil
	}
	ct := req.ContentType
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if ct == "" {
		ct = "application/json"
	}
	media, ok := oo.RequestBody.Value.Content[ct]
	if !ok || media == nil || media.Schema == nil {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return fmt.Errorf("malformed JSON body: %w", err)
	}
	if err := media.Schema.Value.VisitJSON(decoded); err != nil {
		return err
	}
	return nil
}

// ValidateHTTPResponse checks a response against the schema the contract
// declares for its status code.
//
// Two failure classes, both meaningful findings rather than harness errors
// (RFC-055 §5.4):
//
//   - **Undeclared status.** The service returned a code the document never
//     mentions — the classic undocumented degraded path.
//   - **Schema mismatch.** The code is declared but the payload doesn't
//     match, e.g. a null in a non-nullable field under load.
//
// Non-JSON content types are accepted without inspection, matching the
// mock-side ValidateRequest behaviour.
func (t *OperationTable) ValidateHTTPResponse(op *Operation, status int, contentType string, body []byte) error {
	if op.openapi == nil || op.openapi.op == nil {
		return nil
	}
	responses := op.openapi.op.Responses
	if responses == nil || responses.Len() == 0 {
		return nil
	}

	respRef := responses.Status(status)
	if respRef == nil {
		respRef = responses.Default()
		if respRef == nil {
			return fmt.Errorf("status %d is not declared for %s (declared: %s)",
				status, op.Wire(), strings.Join(sortedResponseKeys(responses.Map()), ", "))
		}
	}
	if respRef.Value == nil {
		return nil
	}

	ct := contentType
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if len(respRef.Value.Content) == 0 {
		// Status-only response (204 and friends). A body where none is
		// declared isn't worth failing over — servers add them routinely.
		return nil
	}
	if ct == "" {
		ct = "application/json"
	}
	media, ok := respRef.Value.Content[ct]
	if !ok {
		declared := make([]string, 0, len(respRef.Value.Content))
		for k := range respRef.Value.Content {
			declared = append(declared, k)
		}
		sort.Strings(declared)
		return fmt.Errorf("content type %q is not declared for %s %d (declared: %s)",
			ct, op.Wire(), status, strings.Join(declared, ", "))
	}
	if media == nil || media.Schema == nil || media.Schema.Value == nil {
		return nil
	}
	if !strings.Contains(ct, "json") {
		return nil
	}
	body = trimTrailingWS(body)
	if len(body) == 0 {
		return fmt.Errorf("empty body, but %s %d declares a %s schema", op.Wire(), status, ct)
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return fmt.Errorf("malformed JSON body: %w", err)
	}
	if err := media.Schema.Value.VisitJSON(decoded); err != nil {
		return err
	}
	return nil
}
