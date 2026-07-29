package protocol

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

// gRPC half of RFC-055 clients: turn a protoc FileDescriptorSet into an
// OperationTable, encode caller kwargs as the operation's real request
// message, and invoke it.
//
// This is the caller-side mirror of RFC-023's typed mock. Both build
// dynamicpb messages from descriptors loaded by LoadDescriptorSet; the mock
// encodes responses, the client encodes requests and decodes responses.
//
// Note this path does NOT go through grpcProtocol.ExecuteStep. That step
// method invokes with raw []byte against grpc-go's proto codec, which only
// accepts proto.Message — it cannot carry a typed request. Clients dial and
// invoke with dynamicpb messages, which is what makes typed calls work.

// grpcOpRef is the format-specific payload hanging off an Operation built
// from a proto descriptor set.
type grpcOpRef struct {
	input  protoreflect.MessageDescriptor
	output protoreflect.MessageDescriptor
	files  *protoregistry.Files
}

// GRPCRequest is a fully-bound unary gRPC call.
type GRPCRequest struct {
	FullMethod string
	// Message is a dynamicpb message of the operation's real request type,
	// ready to hand to grpc-go's default proto codec.
	Message proto.Message
	// JSON is the canonical JSON rendering of Message, recorded on the
	// client_call trace event so the trace shows what was actually sent.
	JSON []byte
}

// GRPCResponse is the outcome of a unary call.
type GRPCResponse struct {
	// Code is the gRPC status code. codes.OK on success.
	Code     codes.Code
	CodeName string
	// Message is the status message when Code != OK.
	Message string
	// JSON is the protojson rendering of the response message. Empty when
	// the call failed.
	JSON []byte
	// UnknownFieldBytes counts wire bytes the response descriptor could not
	// account for — a contract-drift signal surfaced by validate="response".
	UnknownFieldBytes int
	DurationMs        int64
}

// BuildGRPCOperations walks a descriptor registry and produces the
// operation table a client exposes.
//
// serviceFQN selects which service to bind when the descriptor set declares
// more than one. Passing "" is valid only for single-service sets; anything
// else is an error listing the candidates, because guessing which of eight
// upstream services the author meant is exactly the kind of implicit
// behaviour that produces a confusing failure three steps later.
//
// Streaming methods are skipped with the rest of the table still built —
// v1 is unary-only (RFC-055 "out of scope"), and refusing to load a whole
// descriptor set because one method streams would block adoption for a
// feature nobody asked for yet.
func BuildGRPCOperations(files *protoregistry.Files, sourcePath, serviceFQN string,
	rename map[string]string) (*OperationTable, error) {

	if files == nil {
		return nil, fmt.Errorf("nil descriptor registry")
	}

	svc, err := selectGRPCService(files, sourcePath, serviceFQN)
	if err != nil {
		return nil, err
	}

	table := newOperationTable(ContractInfo{
		Kind:    ContractGRPC,
		Path:    sourcePath,
		Version: string(svc.FullName()),
	})

	usedRenames := make(map[string]bool, len(rename))
	methods := svc.Methods()
	for i := 0; i < methods.Len(); i++ {
		m := methods.Get(i)
		if m.IsStreamingClient() || m.IsStreamingServer() {
			continue
		}
		fullMethod := "/" + string(svc.FullName()) + "/" + string(m.Name())

		canonical := ""
		if target, ok := rename[string(m.Name())]; ok {
			usedRenames[string(m.Name())] = true
			c, err := CanonicalName(target)
			if err != nil {
				return nil, fmt.Errorf("rename target %q for method %q: %w", target, m.Name(), err)
			}
			canonical = c
		} else if target, ok := rename[fullMethod]; ok {
			usedRenames[fullMethod] = true
			c, err := CanonicalName(target)
			if err != nil {
				return nil, fmt.Errorf("rename target %q for method %q: %w", target, fullMethod, err)
			}
			canonical = c
		} else {
			c, err := CanonicalName(string(m.Name()))
			if err != nil {
				return nil, fmt.Errorf("method %s: %w", fullMethod, err)
			}
			canonical = c
		}

		params, err := grpcRequestParams(fullMethod, m.Input())
		if err != nil {
			return nil, err
		}

		op := &Operation{
			Name:         canonical,
			ContractName: fullMethod,
			FullMethod:   fullMethod,
			Params:       params,
			grpc: &grpcOpRef{
				input:  m.Input(),
				output: m.Output(),
				files:  files,
			},
		}
		if err := table.add(op); err != nil {
			return nil, err
		}
	}

	for key := range rename {
		if !usedRenames[key] {
			return nil, fmt.Errorf(
				"rename key %q matches no unary method on service %s\n"+
					"  rename keys are proto method names (\"GetOrder\") or full paths (\"/pkg.Svc/GetOrder\")",
				key, svc.FullName())
		}
	}

	if table.Len() == 0 {
		return nil, fmt.Errorf("service %s declares no unary methods (streaming RPCs are not supported in v1)",
			svc.FullName())
	}
	table.finalize()
	return table, nil
}

// selectGRPCService resolves the service a client binds to.
func selectGRPCService(files *protoregistry.Files, sourcePath, serviceFQN string) (protoreflect.ServiceDescriptor, error) {
	if serviceFQN != "" {
		desc, err := files.FindDescriptorByName(protoreflect.FullName(serviceFQN))
		if err != nil {
			available := listGRPCServices(files)
			return nil, fmt.Errorf("service %q not found in %s (available: %s)",
				serviceFQN, sourcePath, strings.Join(available, ", "))
		}
		svc, ok := desc.(protoreflect.ServiceDescriptor)
		if !ok {
			return nil, fmt.Errorf("descriptor %q in %s is %T, not a service", serviceFQN, sourcePath, desc)
		}
		return svc, nil
	}

	var found []protoreflect.ServiceDescriptor
	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if strings.HasPrefix(string(fd.Path()), "google/protobuf/") {
			return true
		}
		svcs := fd.Services()
		for i := 0; i < svcs.Len(); i++ {
			found = append(found, svcs.Get(i))
		}
		return true
	})
	sort.Slice(found, func(i, j int) bool { return found[i].FullName() < found[j].FullName() })

	switch len(found) {
	case 0:
		return nil, fmt.Errorf("descriptor set %s declares no gRPC services", sourcePath)
	case 1:
		return found[0], nil
	default:
		names := make([]string, len(found))
		for i, s := range found {
			names[i] = string(s.FullName())
		}
		return nil, fmt.Errorf(
			"descriptor set %s declares %d services; pass grpc_service= to pick one\n  candidates: %s",
			sourcePath, len(found), strings.Join(names, ", "))
	}
}

func listGRPCServices(files *protoregistry.Files) []string {
	var out []string
	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if strings.HasPrefix(string(fd.Path()), "google/protobuf/") {
			return true
		}
		svcs := fd.Services()
		for i := 0; i < svcs.Len(); i++ {
			out = append(out, string(svcs.Get(i).FullName()))
		}
		return true
	})
	sort.Strings(out)
	return out
}

// grpcRequestParams maps the top-level fields of a request message onto
// call-site kwargs. Nested messages are addressed as a whole (pass a dict);
// we deliberately don't flatten them, because a flattened name space would
// collide the moment two sub-messages share a field name.
func grpcRequestParams(fullMethod string, input protoreflect.MessageDescriptor) ([]Param, error) {
	fields := input.Fields()
	out := make([]Param, 0, fields.Len())
	seen := make(map[string]Param, fields.Len())

	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		canonical, err := CanonicalName(string(f.Name()))
		if err != nil {
			return nil, fmt.Errorf("method %s field %q: %w", fullMethod, f.Name(), err)
		}
		if err := checkReservedParamName(fullMethod, string(f.Name()), canonical); err != nil {
			return nil, err
		}
		if prev, dup := seen[canonical]; dup {
			return nil, fmt.Errorf("method %s: fields %q and %q both normalize to kwarg %q",
				fullMethod, prev.WireName, f.Name(), canonical)
		}
		p := Param{
			Name:     canonical,
			WireName: string(f.Name()),
			In:       ParamField,
			// proto3 has no required fields; every field is optional at
			// the wire level and defaults to its zero value.
			Required: false,
		}
		seen[canonical] = p
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// BuildGRPCRequest encodes caller-supplied kwargs as the operation's real
// request message.
func (t *OperationTable) BuildGRPCRequest(op *Operation, args map[string]any) (*GRPCRequest, error) {
	if op.grpc == nil {
		return nil, fmt.Errorf("operation %q is not a gRPC operation", op.Name)
	}

	byName := make(map[string]Param, len(op.Params))
	for _, p := range op.Params {
		byName[p.Name] = p
	}

	// Translate canonical kwargs back to proto field names. protojson
	// accepts the proto name verbatim, so this round-trips exactly.
	payload := make(map[string]any, len(args))
	for k, v := range args {
		if _, reserved := reservedKwargs[k]; reserved {
			if k == "body" {
				return nil, fmt.Errorf("%s: gRPC operations take request fields as kwargs, not body=", op.SignatureHint())
			}
			continue
		}
		p, ok := byName[k]
		if !ok {
			return nil, unknownArgError(op, k, byName)
		}
		payload[p.WireName] = v
	}

	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("%s: encode request: %w", op.SignatureHint(), err)
	}

	msg := dynamicpb.NewMessage(op.grpc.input)
	opts := protojson.UnmarshalOptions{
		DiscardUnknown: false,
		Resolver:       typesResolver{files: op.grpc.files},
	}
	if err := opts.Unmarshal(jsonBytes, msg); err != nil {
		return nil, fmt.Errorf("%s: encode as %s: %w",
			op.SignatureHint(), op.grpc.input.FullName(), enrichProtoFieldError(err, op.grpc.input))
	}

	return &GRPCRequest{FullMethod: op.FullMethod, Message: msg, JSON: jsonBytes}, nil
}

// unknownProtoField extracts the offending field name from a protojson
// unknown-field error so we can suggest the right one.
var unknownProtoField = regexp.MustCompile(`unknown field "([^"]+)"`)

// enrichProtoFieldError appends a nearest-field suggestion to protojson's
// unknown-field error. Field-name typos (cityid vs city_id, camelCase vs
// snake_case) are the dominant failure when hand-writing a typed request;
// the raw error names the bad field but not the right one.
func enrichProtoFieldError(err error, desc protoreflect.MessageDescriptor) error {
	m := unknownProtoField.FindStringSubmatch(err.Error())
	if m == nil {
		return err
	}
	bad := m[1]
	fields := desc.Fields()
	best, bestDist := "", 4
	for i := 0; i < fields.Len(); i++ {
		name := string(fields.Get(i).Name())
		if d := editDistance(bad, name); d < bestDist {
			best, bestDist = name, d
		}
	}
	if best == "" {
		return err
	}
	return fmt.Errorf("%w (did you mean %q?)", err, best)
}

// InvokeGRPCUnary dials addr and performs one unary call.
//
// Stateless per call (RFC-055 OQ-5): the connection is created and torn
// down around the invoke, matching how the existing step methods behave and
// keeping the proxy's connection accounting honest — one client call is one
// proxy connection in the trace.
func InvokeGRPCUnary(ctx context.Context, addr string, op *Operation, req *GRPCRequest,
	headers map[string]string, tlsCfg *tls.Config, timeout time.Duration) (*GRPCResponse, error) {

	if op.grpc == nil {
		return nil, fmt.Errorf("operation %q is not a gRPC operation", op.Name)
	}
	start := time.Now()

	creds := insecure.NewCredentials()
	if tlsCfg != nil {
		creds = credentials.NewTLS(tlsCfg)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", addr, err)
	}
	defer conn.Close()

	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	if len(headers) > 0 {
		md := metadata.New(nil)
		for k, v := range headers {
			md.Set(strings.ToLower(k), v)
		}
		ctx = metadata.NewOutgoingContext(ctx, md)
	}

	resp := dynamicpb.NewMessage(op.grpc.output)
	invokeErr := conn.Invoke(ctx, op.FullMethod, req.Message, resp)
	out := &GRPCResponse{DurationMs: time.Since(start).Milliseconds()}

	if invokeErr != nil {
		st, _ := status.FromError(invokeErr)
		out.Code = st.Code()
		out.CodeName = st.Code().String()
		out.Message = st.Message()
		return out, nil
	}

	out.Code = codes.OK
	out.CodeName = codes.OK.String()
	out.UnknownFieldBytes = len(resp.ProtoReflect().GetUnknown())

	jsonBytes, err := protojson.MarshalOptions{
		// Emit zero values so a decoded response has the full field set —
		// spec authors index into resp.data and shouldn't have to know
		// which fields protojson elides.
		EmitUnpopulated: true,
		Resolver:        typesResolver{files: op.grpc.files},
	}.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("decode %s response as %s: %w", op.FullMethod, op.grpc.output.FullName(), err)
	}
	out.JSON = jsonBytes
	return out, nil
}

// ValidateGRPCResponse reports contract conformance for a completed call.
//
// A typed decode already enforces most of the contract — wire bytes that
// don't match the response descriptor fail at Invoke. What survives that
// and still indicates drift is unknown fields: bytes the server sent that
// this descriptor set has no field for, i.e. the server is running a newer
// proto than the contract the test was checked against.
func (t *OperationTable) ValidateGRPCResponse(op *Operation, resp *GRPCResponse) error {
	if op.grpc == nil || resp == nil {
		return nil
	}
	if resp.Code != codes.OK {
		// A non-OK status is an outcome, not a conformance failure. The
		// test's own assertions decide whether UNAVAILABLE was expected.
		return nil
	}
	if resp.UnknownFieldBytes > 0 {
		return fmt.Errorf("response carried %d bytes of fields absent from %s — the server's proto is newer than %s",
			resp.UnknownFieldBytes, op.grpc.output.FullName(), t.Contract.Path)
	}
	return nil
}
