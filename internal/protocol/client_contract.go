package protocol

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// This file holds the contract-agnostic half of RFC-055 clients: the
// operation table a client exposes, the canonical-naming rules that turn
// contract-native identifiers into Starlark attribute names, and the
// lookup/suggestion machinery behind "did you mean …?" errors.
//
// The OpenAPI and gRPC builders (openapi_client.go, grpc_client.go) both
// produce an *OperationTable; everything above this layer — the Starlark
// ClientVal, the trace emitter, `faultbox inspect --clients` — works
// against the table and never against a contract format directly.

// ContractKind identifies which contract format an operation table came
// from. Recorded on trace events so a run says which kind of document it
// was checked against, not just which file.
type ContractKind string

const (
	ContractOpenAPI ContractKind = "openapi"
	ContractGRPC    ContractKind = "grpc"
)

// ContractInfo identifies the exact contract a client was built from. It
// lands verbatim in the `contract` field of client_call events so a trace
// records the contract *version* the run exercised — the difference
// between "this passed" and "this passed against the API we shipped".
type ContractInfo struct {
	Kind ContractKind
	// Path is the document the contract was loaded from.
	Path string
	// Version is the contract's own version marker: `info.version` for
	// OpenAPI, the fully-qualified service name for gRPC. May be empty
	// when the document declares none.
	Version string
}

// String renders the contract identity for trace fields, e.g.
// "openapi:orders.yaml@1.4.0" or "grpc:orders.pb#orders.v1.OrderService".
func (c ContractInfo) String() string {
	switch c.Kind {
	case ContractGRPC:
		if c.Version == "" {
			return "grpc:" + c.Path
		}
		return "grpc:" + c.Path + "#" + c.Version
	default:
		if c.Version == "" {
			return string(c.Kind) + ":" + c.Path
		}
		return string(c.Kind) + ":" + c.Path + "@" + c.Version
	}
}

// ParamLocation is where a bound parameter travels on the wire.
type ParamLocation string

const (
	ParamPath   ParamLocation = "path"
	ParamQuery  ParamLocation = "query"
	ParamHeader ParamLocation = "header"
	ParamCookie ParamLocation = "cookie"
	// ParamField is a protobuf request-message field. gRPC operations
	// carry only these.
	ParamField ParamLocation = "field"
)

// Param is one caller-supplied input to an operation.
type Param struct {
	// Name is the canonical snake_case name the spec author passes as a
	// Starlark kwarg.
	Name string
	// WireName is the contract-native name: the `{orderId}` path
	// placeholder, the `?include=` query key, the `X-Tenant` header, or
	// the proto field name.
	WireName string
	In       ParamLocation
	Required bool
	// Description is carried through for `faultbox inspect --clients`.
	Description string
}

// Operation is a single callable entry on a client.
type Operation struct {
	// Name is the canonical snake_case Starlark attribute.
	Name string
	// ContractName is the contract-native identifier — the OpenAPI
	// `operationId` (or its synthesized stand-in) or the full gRPC method
	// path. This is the key accepted by client.call().
	ContractName string
	// Method is the HTTP verb (uppercase). Empty for gRPC operations.
	Method string
	// PathTemplate is the OpenAPI path with `{param}` placeholders intact.
	// Empty for gRPC operations.
	PathTemplate string
	// FullMethod is the gRPC wire path ("/pkg.Service/Method"). Empty for
	// HTTP operations.
	FullMethod string
	// Summary is a one-line human description from the contract.
	Summary string
	// Params are ordered: path, then query, then header, then cookie (or
	// proto field order for gRPC). Stable across runs.
	Params []Param
	// AcceptsBody reports whether the operation declares a request body.
	// gRPC operations bind fields as params instead and leave this false.
	AcceptsBody bool
	// BodyRequired reports whether a declared request body is mandatory.
	BodyRequired bool

	// openapiOp / grpc descriptors are attached by the respective builders
	// and consumed by request building and response validation. They stay
	// unexported so consumers above this package can't reach into a
	// format-specific representation.
	openapi *openapiOpRef
	grpc    *grpcOpRef
}

// SignatureHint renders the operation as a call the user could paste,
// e.g. `get_order(order_id, include=…)`. Used in inspect output and in
// "did you mean" errors.
func (o *Operation) SignatureHint() string {
	var required, optional []string
	for _, p := range o.Params {
		if p.Required {
			required = append(required, p.Name)
		} else {
			optional = append(optional, p.Name+"=…")
		}
	}
	if o.AcceptsBody {
		if o.BodyRequired {
			required = append(required, "body")
		} else {
			optional = append(optional, "body=…")
		}
	}
	return o.Name + "(" + strings.Join(append(required, optional...), ", ") + ")"
}

// Wire renders the operation's wire target for diagnostics and trace
// summaries: "GET /orders/{orderId}" or "/orders.v1.OrderService/GetOrder".
func (o *Operation) Wire() string {
	if o.FullMethod != "" {
		return o.FullMethod
	}
	return o.Method + " " + o.PathTemplate
}

// OperationTable is the full set of operations a client exposes.
type OperationTable struct {
	Contract ContractInfo
	// ops is keyed by canonical name.
	ops map[string]*Operation
	// byContractName lets client.call() accept contract-native names.
	byContractName map[string]*Operation
	// order is the sorted canonical-name list — deterministic iteration
	// for inspect output and error messages.
	order []string
}

func newOperationTable(info ContractInfo) *OperationTable {
	return &OperationTable{
		Contract:       info,
		ops:            make(map[string]*Operation),
		byContractName: make(map[string]*Operation),
	}
}

// add registers an operation, rejecting canonical-name collisions. Two
// operations that normalize to the same attribute are a spec error, not
// something to disambiguate silently: the caller can't tell which one they
// would have got, and a rename= entry makes the intent explicit
// (RFC-055 §5.2).
func (t *OperationTable) add(op *Operation) error {
	if prev, exists := t.ops[op.Name]; exists {
		return fmt.Errorf(
			"operation name collision: %q and %q both normalize to %q\n"+
				"  fix: pass rename={%q: \"<other_name>\"} to client()",
			prev.ContractName, op.ContractName, op.Name, op.ContractName)
	}
	t.ops[op.Name] = op
	t.byContractName[op.ContractName] = op
	t.order = append(t.order, op.Name)
	return nil
}

// finalize sorts the canonical-name order. Called once by each builder
// after every operation is added.
func (t *OperationTable) finalize() {
	sort.Strings(t.order)
}

// Names returns the canonical operation names in sorted order.
func (t *OperationTable) Names() []string {
	out := make([]string, len(t.order))
	copy(out, t.order)
	return out
}

// Len reports how many operations the contract declared.
func (t *OperationTable) Len() int { return len(t.ops) }

// Operations returns every operation in canonical-name order.
func (t *OperationTable) Operations() []*Operation {
	out := make([]*Operation, 0, len(t.order))
	for _, n := range t.order {
		out = append(out, t.ops[n])
	}
	return out
}

// Lookup resolves a canonical operation name. Exact match only — the
// suggestion path lives in Resolve.
func (t *OperationTable) Lookup(name string) (*Operation, bool) {
	op, ok := t.ops[name]
	return op, ok
}

// LookupContractName resolves a contract-native identifier (operationId or
// "/pkg.Service/Method"). Backs client.call().
func (t *OperationTable) LookupContractName(name string) (*Operation, bool) {
	op, ok := t.byContractName[name]
	return op, ok
}

// Resolve looks up an operation by canonical name and, on a miss, returns
// an error carrying the nearest match. Attribute typos are the single most
// common client error, and "no operation get_orders" without a suggestion
// makes the user re-read the contract.
func (t *OperationTable) Resolve(clientName, name string) (*Operation, error) {
	if op, ok := t.ops[name]; ok {
		return op, nil
	}
	msg := fmt.Sprintf("client %q has no operation %q", clientName, name)
	if best, ok := t.nearest(name); ok {
		msg += fmt.Sprintf(" (did you mean %q?)", best)
	}
	msg += fmt.Sprintf("\n  %d operations available — run `faultbox inspect --clients` for the full table",
		len(t.order))
	return nil, fmt.Errorf("%s", msg)
}

// nearest finds the closest canonical name within an edit distance that
// scales with the length of the query, so short names don't match
// everything and long names tolerate a typo or two.
func (t *OperationTable) nearest(name string) (string, bool) {
	budget := len(name)/3 + 1
	if budget > 4 {
		budget = 4
	}
	best, bestDist := "", budget+1
	for _, cand := range t.order {
		d := editDistance(name, cand)
		if d < bestDist {
			best, bestDist = cand, d
		}
	}
	if best == "" || bestDist > budget {
		return "", false
	}
	return best, true
}

// editDistance is Levenshtein distance over runes, two-row variant. Input
// sizes here are operation names, so allocation behaviour doesn't matter.
func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}
	prev := make([]int, len(br)+1)
	cur := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = min3(cur[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(br)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// reservedKwargs are the call-site keyword arguments the client layer owns.
// A contract parameter normalizing to one of these can't be bound by name,
// so it's rejected at table-build time with an actionable message rather
// than silently shadowed at call time.
var reservedKwargs = map[string]string{
	"body":    "the request body",
	"headers": "per-call header overrides",
	"timeout": "the per-call deadline",
}

// checkReservedParamName rejects a parameter whose canonical name would
// collide with a reserved call-site kwarg.
func checkReservedParamName(opContractName, paramWireName, canonical string) error {
	purpose, clash := reservedKwargs[canonical]
	if !clash {
		return nil
	}
	return fmt.Errorf(
		"operation %q declares parameter %q, which normalizes to the reserved kwarg %q (%s)\n"+
			"  fix: call this operation via client.call(%q, params={%q: …}) instead of the generated attribute",
		opContractName, paramWireName, canonical, purpose, opContractName, canonical)
}

// CanonicalName converts a contract-native identifier into a Starlark
// attribute name: snake_case, lowercase, digits preserved.
//
//	getOrder      → get_order
//	GetOrderByID  → get_order_by_id
//	ListOrdersV2  → list_orders_v2
//	HTTPServer    → http_server
//	get-order     → get_order
//
// A name starting with a digit gets an `op_` prefix so the result is
// always a valid identifier. Returns an error only when the input has no
// alphanumeric content at all.
func CanonicalName(raw string) (string, error) {
	words := splitIdentifierWords(raw)
	if len(words) == 0 {
		return "", fmt.Errorf("cannot derive an operation name from %q (no alphanumeric characters)", raw)
	}
	out := strings.Join(words, "_")
	if unicode.IsDigit(rune(out[0])) {
		out = "op_" + out
	}
	return out, nil
}

// splitIdentifierWords breaks an identifier into lowercase words on
// separator characters and camel/Pascal-case boundaries.
//
// The boundary rule fires when an uppercase rune follows a lowercase rune
// or a digit ("orderId" → order|Id), or when it starts a new word inside a
// run of uppercase ("HTTPServer" → HTTP|Server, because S is followed by a
// lowercase). Digits continue the current word so "V2" stays one token.
func splitIdentifierWords(raw string) []string {
	runes := []rune(raw)
	var words []string
	var cur strings.Builder

	flush := func() {
		if cur.Len() > 0 {
			words = append(words, cur.String())
			cur.Reset()
		}
	}

	for i, r := range runes {
		switch {
		case unicode.IsUpper(r):
			if cur.Len() > 0 {
				prev := runes[i-1]
				nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
				if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
					flush()
				}
			}
			cur.WriteRune(unicode.ToLower(r))
		case unicode.IsLower(r) || unicode.IsDigit(r):
			cur.WriteRune(r)
		default:
			// Any other rune (-, _, ., /, space, …) is a separator.
			flush()
		}
	}
	flush()
	return words
}

// paramToString renders a caller-supplied parameter value for a path
// segment, query value, or header. Starlark's numeric types arrive as
// int64/float64; everything else is rejected explicitly so a spec author
// sees "list not supported here" rather than a mangled "[1 2 3]".
func paramToString(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case bool:
		if t {
			return "true", nil
		}
		return "false", nil
	case int:
		return fmt.Sprintf("%d", t), nil
	case int64:
		return fmt.Sprintf("%d", t), nil
	case float64:
		// Render integral floats without a trailing ".0" — Starlark ints
		// that overflowed into float, and JSON-decoded numbers, both land
		// here and users expect "42" not "42.0".
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t)), nil
		}
		return fmt.Sprintf("%g", t), nil
	default:
		return "", fmt.Errorf("unsupported parameter value of type %T (expected string, int, float, or bool)", v)
	}
}
