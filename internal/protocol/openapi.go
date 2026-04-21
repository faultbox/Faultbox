package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// OpenAPISpec is the loaded, resolved, and structurally-validated OpenAPI 3.0
// document used to drive route generation for HTTP mock_service(). It is an
// opaque handle carried through MockConfig into the HTTP mock handler.
// Populated by LoadOpenAPI; produced at spec-load time so malformed documents
// fail fast (before the test starts).
type OpenAPISpec struct {
	// Path the document was loaded from. Used for relative $ref resolution
	// and diagnostics only.
	Path string
	// Doc is the parsed document. Kept whole (rather than decomposed into
	// our own routes structure at load time) so future phases — request
	// validation, schema synthesis — can consult it without re-parsing.
	Doc *openapi3.T
}

// LoadOpenAPI reads an OpenAPI 3.0 document from path and returns a validated
// OpenAPISpec. Relative $refs to other files on disk are resolved; $refs that
// start with http:// or https:// are rejected to avoid surprising network
// I/O at spec-load time (RFC-021 OQ2 resolution).
func LoadOpenAPI(path string) (*OpenAPISpec, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", path, err)
	}
	loader := &openapi3.Loader{
		IsExternalRefsAllowed: true,
		ReadFromURIFunc:       readLocalURIOnly,
	}
	doc, err := loader.LoadFromFile(abs)
	if err != nil {
		return nil, fmt.Errorf("load OpenAPI %q: %w", path, err)
	}
	if err := doc.Validate(context.Background(), openapi3.DisableExamplesValidation()); err != nil {
		return nil, fmt.Errorf("validate OpenAPI %q: %w", path, err)
	}
	return &OpenAPISpec{Path: abs, Doc: doc}, nil
}

// readLocalURIOnly is a ReadFromURIFunc that permits only filesystem refs.
// Network schemes (http/https) produce an explicit error so users see a
// clear message instead of an opaque loader timeout.
func readLocalURIOnly(loader *openapi3.Loader, location *url.URL) ([]byte, error) {
	switch location.Scheme {
	case "", "file":
		return openapi3.ReadFromFile(loader, location)
	default:
		return nil, fmt.Errorf("external $ref scheme %q not allowed (RFC-021 Phase 1 permits filesystem refs only)", location.Scheme)
	}
}

// ValidateRequest checks a request body against the OpenAPI operation for
// the given method and path. Returns nil if the body is absent (no
// requestBody declared or body empty) or validates successfully; otherwise
// returns an error describing the first validation failure.
//
// Phase 2 semantics: JSON-body content types only. Non-JSON bodies are
// accepted without validation (treated as opaque). Path matching walks the
// Paths map and compares segments — OpenAPI-style `{param}` matches any
// non-slash segment.
func (s *OpenAPISpec) ValidateRequest(method, path string, body []byte, contentType string) error {
	if s == nil || s.Doc == nil {
		return nil
	}
	op, _ := s.findOperation(method, path)
	if op == nil {
		return nil
	}
	if op.RequestBody == nil || op.RequestBody.Value == nil {
		return nil
	}
	// Accept declarations that don't require a body.
	body = trimTrailingWS(body)
	if len(body) == 0 {
		if op.RequestBody.Value.Required {
			return fmt.Errorf("request body is required")
		}
		return nil
	}
	ct := contentType
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if ct == "" {
		ct = "application/json"
	}
	media, ok := op.RequestBody.Value.Content[ct]
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

func trimTrailingWS(b []byte) []byte {
	for len(b) > 0 {
		switch b[len(b)-1] {
		case ' ', '\t', '\n', '\r':
			b = b[:len(b)-1]
		default:
			return b
		}
	}
	return b
}

// findOperation resolves a (method, concrete-path) pair to the OpenAPI
// operation that matches it. Path templates (`/pets/{id}`) are compared
// segment-by-segment: literal segments must match exactly; `{param}` matches
// any single non-slash segment.
func (s *OpenAPISpec) findOperation(method, concretePath string) (*openapi3.Operation, string) {
	if s.Doc == nil || s.Doc.Paths == nil {
		return nil, ""
	}
	method = strings.ToUpper(method)
	// Fast path: literal match.
	if item := s.Doc.Paths.Value(concretePath); item != nil {
		if op := operationFor(item, method); op != nil {
			return op, concretePath
		}
	}
	concreteSegs := splitPathSegments(concretePath)
	for tmpl, item := range s.Doc.Paths.Map() {
		if !strings.Contains(tmpl, "{") {
			continue
		}
		if matchTemplateSegments(splitPathSegments(tmpl), concreteSegs) {
			if op := operationFor(item, method); op != nil {
				return op, tmpl
			}
		}
	}
	return nil, ""
}

func splitPathSegments(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func matchTemplateSegments(tmpl, concrete []string) bool {
	if len(tmpl) != len(concrete) {
		return false
	}
	for i, seg := range tmpl {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			continue
		}
		if seg != concrete[i] {
			return false
		}
	}
	return true
}

// GenerateRoutes walks the OpenAPI document and produces one MockRoute per
// (path, method) pair. The selector decides which example to serve for each
// operation.
//
// Phase 1 semantics: if an operation has no example and no example can be
// selected, GenerateRoutes returns an error — we refuse to guess at spec-load
// time (RFC-021 OQ3 resolution). Phase 3 will add schema synthesis.
func (s *OpenAPISpec) GenerateRoutes(sel ExampleSelector) ([]MockRoute, error) {
	if s == nil || s.Doc == nil {
		return nil, fmt.Errorf("nil OpenAPI spec")
	}

	// Deterministic iteration order: sort path keys so generated route
	// tables are stable across runs (easier diffing, easier reproduction
	// of test failures).
	paths := s.Doc.Paths
	if paths == nil {
		return nil, fmt.Errorf("OpenAPI document has no paths")
	}
	keys := make([]string, 0, paths.Len())
	for k := range paths.Map() {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var routes []MockRoute
	for _, p := range keys {
		item := paths.Value(p)
		if item == nil {
			continue
		}
		for _, method := range httpMethodsOf(item) {
			op := operationFor(item, method)
			if op == nil {
				continue
			}
			resp, err := sel.Select(op, method, p)
			if err != nil {
				return nil, err
			}
			routes = append(routes, MockRoute{
				Pattern:  method + " " + OpenAPIPathToGlob(p),
				Response: resp,
			})
		}
	}
	return routes, nil
}

// ExampleSelector picks a response payload for an operation given the
// operation's declared responses. Three implementations ship in v0.9.3:
// FirstExampleSelector (deterministic default), NamedExampleSelector (pick
// by key across all ops), RandomExampleSelector (seeded random pick per op).
type ExampleSelector interface {
	Select(op *openapi3.Operation, method, path string) (*MockResponse, error)
}

// FirstExampleSelector picks the first declared example on the first 2xx (or
// else the first declared) response. This is the deterministic default —
// useful for CI where the same example must be returned on every run.
type FirstExampleSelector struct {
	// SynthesizeMissing, when true, falls back to schema-based synthesis
	// when an operation has no declared example. Opt-in because synthesis
	// produces placeholder data ("", 0) that may not be useful to every
	// SUT — Phase 1 semantics (hard error) remain the default.
	SynthesizeMissing bool
}

func (s FirstExampleSelector) Select(op *openapi3.Operation, method, path string) (*MockResponse, error) {
	return pickWithStrategy(op, method, path, &strategyOpts{pickFirst: true, synthesize: s.SynthesizeMissing})
}

// NamedExampleSelector picks the example whose key matches Name. Falls back
// to the first declared example when the named key is absent on a particular
// operation — we'd rather serve something reasonable than refuse to start
// when users have a mix of ops with/without the named variant.
type NamedExampleSelector struct {
	Name              string
	SynthesizeMissing bool
}

func (s NamedExampleSelector) Select(op *openapi3.Operation, method, path string) (*MockResponse, error) {
	return pickWithStrategy(op, method, path, &strategyOpts{name: s.Name, pickFirst: true, synthesize: s.SynthesizeMissing})
}

// RandomExampleSelector picks a random example per operation. Seeded once
// at construction so a given spec+seed combo produces a stable route table
// (reproducible) while different seeds exercise different examples — useful
// for fuzzing SUT error-handling paths across test runs.
type RandomExampleSelector struct {
	rng               *rand.Rand
	SynthesizeMissing bool
}

// NewRandomExampleSelector constructs a RandomExampleSelector seeded with
// the given value. Pass 0 to seed from the current time (non-reproducible);
// pass any non-zero value to get deterministic selection.
func NewRandomExampleSelector(seed int64) *RandomExampleSelector {
	return &RandomExampleSelector{rng: rand.New(rand.NewSource(seed))}
}

func (s *RandomExampleSelector) Select(op *openapi3.Operation, method, path string) (*MockResponse, error) {
	return pickWithStrategy(op, method, path, &strategyOpts{rng: s.rng, pickFirst: true, synthesize: s.SynthesizeMissing})
}

// strategyOpts carries the knobs the shared picker honours. Having one
// function body means the status/content/encoding logic can't drift between
// selectors.
type strategyOpts struct {
	// name, when non-empty, selects an example by key from the `examples:`
	// map. Falls back to `pickFirst` behaviour if the key is missing.
	name string
	// rng, when non-nil, randomises the choice among declared examples.
	rng *rand.Rand
	// pickFirst requests deterministic first-example selection when neither
	// `name` nor `rng` resolves a choice.
	pickFirst bool
	// synthesize allows the picker to synthesize a response body from the
	// operation's schema when no example exists. Schema synthesis is
	// opt-in because it produces placeholder values.
	synthesize bool
}

func pickWithStrategy(op *openapi3.Operation, method, path string, opts *strategyOpts) (*MockResponse, error) {
	if op.Responses == nil {
		return nil, fmt.Errorf("%s %s: operation has no responses", method, path)
	}
	status, respRef := pickPrimaryResponse(op.Responses)
	if respRef == nil || respRef.Value == nil {
		return nil, fmt.Errorf("%s %s: no usable response", method, path)
	}

	mediaType, contentType, ok := pickMediaType(respRef.Value.Content)
	if !ok {
		return &MockResponse{Status: status}, nil
	}

	example, err := pickExample(mediaType, opts)
	if err != nil {
		if opts != nil && opts.synthesize && mediaType.Schema != nil {
			example = synthesizeFromSchema(mediaType.Schema)
		} else {
			return nil, fmt.Errorf("%s %s: %w", method, path, err)
		}
	}
	body, err := encodeExample(example, contentType)
	if err != nil {
		return nil, fmt.Errorf("%s %s: encode example: %w", method, path, err)
	}
	return &MockResponse{
		Status:      status,
		Body:        body,
		ContentType: contentType,
	}, nil
}

// pickPrimaryResponse returns (status, responseRef) for the first 2xx
// response declared on the operation, falling back to "default", falling
// back to the first declared response. Status 200 is used when the key is
// "default" or non-numeric.
func pickPrimaryResponse(responses *openapi3.Responses) (int, *openapi3.ResponseRef) {
	keys := sortedResponseKeys(responses.Map())
	// Preference order: 200, 201, any 2xx, "default", first.
	pref := []string{"200", "201", "202", "203", "204", "default"}
	for _, k := range pref {
		if r := responses.Value(k); r != nil {
			return responseStatusForKey(k), r
		}
	}
	for _, k := range keys {
		if strings.HasPrefix(k, "2") {
			return responseStatusForKey(k), responses.Value(k)
		}
	}
	if len(keys) > 0 {
		k := keys[0]
		return responseStatusForKey(k), responses.Value(k)
	}
	return 0, nil
}

func responseStatusForKey(k string) int {
	if k == "default" {
		return 200
	}
	var n int
	_, err := fmt.Sscanf(k, "%d", &n)
	if err != nil || n == 0 {
		return 200
	}
	return n
}

func sortedResponseKeys(m map[string]*openapi3.ResponseRef) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// pickMediaType chooses a response body media-type. application/json is
// preferred when present; otherwise the first declared content type (sorted
// for determinism). Returns ok=false when no content is declared (status-only
// response).
func pickMediaType(content openapi3.Content) (*openapi3.MediaType, string, bool) {
	if len(content) == 0 {
		return nil, "", false
	}
	if mt, ok := content["application/json"]; ok && mt != nil {
		return mt, "application/json", true
	}
	keys := make([]string, 0, len(content))
	for k := range content {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if mt := content[k]; mt != nil {
			return mt, k, true
		}
	}
	return nil, "", false
}

// pickExample returns an example value from the media type, honouring the
// strategy options. Order of precedence:
//
//  1. If opts.name is set and mt.Examples[name] exists, use it.
//  2. If opts.rng is non-nil and examples exist, pick a random one.
//  3. If opts.pickFirst is true, fall back to the inline `example:` (if any)
//     then the first entry in `examples:` (sorted for determinism).
//
// Returns an error if none of the above yields a value. Callers with
// opts.synthesize may recover from that error via schema synthesis.
func pickExample(mt *openapi3.MediaType, opts *strategyOpts) (any, error) {
	if mt == nil {
		return nil, fmt.Errorf("no media type")
	}
	if opts != nil && opts.name != "" {
		if ex, ok := mt.Examples[opts.name]; ok && ex != nil && ex.Value != nil {
			return ex.Value.Value, nil
		}
		// Named example missing on this op; fall through to pickFirst
		// behaviour rather than erroring — most real specs have some ops
		// without every named variant.
	}
	if opts != nil && opts.rng != nil && len(mt.Examples) > 0 {
		keys := make([]string, 0, len(mt.Examples))
		for k := range mt.Examples {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		pick := keys[opts.rng.Intn(len(keys))]
		if ex, ok := mt.Examples[pick]; ok && ex != nil && ex.Value != nil {
			return ex.Value.Value, nil
		}
	}
	if mt.Example != nil {
		return mt.Example, nil
	}
	if len(mt.Examples) > 0 {
		keys := make([]string, 0, len(mt.Examples))
		for k := range mt.Examples {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		ex := mt.Examples[keys[0]]
		if ex == nil || ex.Value == nil {
			return nil, fmt.Errorf("examples[%q] is empty", keys[0])
		}
		return ex.Value.Value, nil
	}
	return nil, fmt.Errorf("no example declared (set examples=\"synthesize\" to fall back to schema-based values)")
}

// synthesizeFromSchema produces a minimal, type-correct placeholder value
// for a JSON Schema. This is the Phase 3 fallback when users enable
// SynthesizeMissing: rather than refusing to serve an operation that lacks
// an example, we produce something structurally valid — the SUT's decoder
// won't blow up, even if the business semantics are trivial.
//
// Values chosen are deliberately simple placeholders (empty string, 0,
// empty object/array) rather than realistic fakes. A future RFC can add a
// "realistic" mode if customers actually need it.
func synthesizeFromSchema(sref *openapi3.SchemaRef) any {
	if sref == nil || sref.Value == nil {
		return nil
	}
	s := sref.Value

	// If the schema itself declares a default/example, honour it.
	if s.Default != nil {
		return s.Default
	}
	if s.Example != nil {
		return s.Example
	}

	types := s.Type
	switch {
	case types.Is("string"):
		return ""
	case types.Is("number"), types.Is("integer"):
		return 0
	case types.Is("boolean"):
		return false
	case types.Is("array"):
		if s.Items == nil || s.Items.Value == nil {
			return []any{}
		}
		return []any{synthesizeFromSchema(s.Items)}
	case types.Is("object"):
		out := make(map[string]any, len(s.Properties))
		for name, prop := range s.Properties {
			out[name] = synthesizeFromSchema(prop)
		}
		return out
	}

	// Untyped / allOf / oneOf → fall back to empty object. Good enough for
	// Phase 3; the escape hatch is `overrides=` or explicit examples in
	// the spec.
	return map[string]any{}
}

// encodeExample serializes an example value into the wire bytes matching its
// declared content type. For application/json we round-trip through
// encoding/json; non-JSON types accept only string examples (pass-through).
func encodeExample(example any, contentType string) ([]byte, error) {
	if strings.Contains(contentType, "json") {
		return json.Marshal(example)
	}
	switch v := example.(type) {
	case string:
		return []byte(v), nil
	case []byte:
		return v, nil
	default:
		// Best-effort fallback: JSON-encode so the client at least gets
		// something deterministic.
		return json.Marshal(example)
	}
}

// httpMethodsOf returns the set of HTTP methods declared on a path item in
// OpenAPI's canonical order.
func httpMethodsOf(item *openapi3.PathItem) []string {
	methods := make([]string, 0, 8)
	if item.Get != nil {
		methods = append(methods, "GET")
	}
	if item.Post != nil {
		methods = append(methods, "POST")
	}
	if item.Put != nil {
		methods = append(methods, "PUT")
	}
	if item.Patch != nil {
		methods = append(methods, "PATCH")
	}
	if item.Delete != nil {
		methods = append(methods, "DELETE")
	}
	if item.Head != nil {
		methods = append(methods, "HEAD")
	}
	if item.Options != nil {
		methods = append(methods, "OPTIONS")
	}
	if item.Trace != nil {
		methods = append(methods, "TRACE")
	}
	return methods
}

func operationFor(item *openapi3.PathItem, method string) *openapi3.Operation {
	switch method {
	case "GET":
		return item.Get
	case "POST":
		return item.Post
	case "PUT":
		return item.Put
	case "PATCH":
		return item.Patch
	case "DELETE":
		return item.Delete
	case "HEAD":
		return item.Head
	case "OPTIONS":
		return item.Options
	case "TRACE":
		return item.Trace
	}
	return nil
}

// OpenAPIPathToGlob converts an OpenAPI path (with `{param}` placeholders)
// into the glob form that matchHTTPRoute understands (`*` matches a single
// path segment). `{id}` → `*`, `/pets/{id}/items` → `/pets/*/items`.
// Exported so callers outside this package (the Starlark runtime) can
// normalise user-supplied overrides that reference OpenAPI-style paths.
func OpenAPIPathToGlob(openapiPath string) string {
	if !strings.Contains(openapiPath, "{") {
		return openapiPath
	}
	var b strings.Builder
	b.Grow(len(openapiPath))
	depth := 0
	for _, r := range openapiPath {
		switch r {
		case '{':
			depth++
			if depth == 1 {
				b.WriteByte('*')
			}
		case '}':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}
