package protocol

import (
	"fmt"
	"os"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	// Side-effect imports: register well-known types with the global
	// registry so customer descriptor files that import them resolve
	// without requiring the customer to pass `--include_imports` to
	// protoc. Blank imports — the Go init() in each package is what
	// populates protoregistry.GlobalFiles.
	_ "google.golang.org/protobuf/types/known/anypb"
	_ "google.golang.org/protobuf/types/known/durationpb"
	_ "google.golang.org/protobuf/types/known/emptypb"
	_ "google.golang.org/protobuf/types/known/fieldmaskpb"
	_ "google.golang.org/protobuf/types/known/structpb"
	_ "google.golang.org/protobuf/types/known/timestamppb"
	_ "google.golang.org/protobuf/types/known/wrapperspb"
)

// LoadDescriptorSet reads a protoc-generated FileDescriptorSet (.pb file,
// typically produced by `protoc --include_imports --descriptor_set_out=out.pb
// <proto files...>`) and returns an isolated protoregistry.Files containing
// every file in the set plus the standard google.protobuf.* well-known types
// pre-registered.
//
// The returned registry is per-mock — two mocks loading different .pb files
// can coexist in the same process without polluting each other's type space.
//
// Well-known types are pulled from Go's global registry
// (protoregistry.GlobalFiles), which is populated by the blank imports at
// the top of this file. Customer .pb files referencing
// google.protobuf.Timestamp, google.protobuf.Empty, etc. therefore resolve
// even if the customer omitted --include_imports when building their .pb.
func LoadDescriptorSet(path string) (*protoregistry.Files, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read descriptor set %s: %w", path, err)
	}

	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("parse descriptor set %s: %w", path, err)
	}

	files := new(protoregistry.Files)

	// Pre-register the standard google.protobuf.* well-known types so that
	// customer files importing them resolve even when the customer did NOT
	// pass --include_imports to protoc. Non-fatal if any individual WKT
	// fails to register (defensive; these are all in the stdlib).
	protoregistry.GlobalFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if strings.HasPrefix(string(fd.Path()), "google/protobuf/") {
			_ = files.RegisterFile(fd)
		}
		return true
	})

	// Register each file from the customer's descriptor set. protodesc.NewFile
	// resolves imports against the registry we're building up, so customer
	// files that import other customer files work if they appear in the set
	// in topological order. `protoc --include_imports` emits them that way.
	for _, fdp := range set.File {
		fd, err := protodesc.NewFile(fdp, files)
		if err != nil {
			return nil, fmt.Errorf("register file %s: %w", fdp.GetName(), err)
		}
		if err := files.RegisterFile(fd); err != nil {
			return nil, fmt.Errorf("add file %s to registry: %w", fdp.GetName(), err)
		}
	}

	return files, nil
}

// ResolveMethod looks up a gRPC method by its full wire path
// (e.g. "/pkg.Service/Method") in the given registry and returns the
// descriptors for its input and output message types. Used by the typed
// gRPC mock handler to know how to decode the incoming request and encode
// the response.
//
// Returns a descriptive error for each failure class so request-time error
// messages can point spec authors at the right fix:
//   - unknown path format ("/foo" — missing service/method segment)
//   - no service by that fully-qualified name in the descriptor set
//   - service exists but no such method
func ResolveMethod(files *protoregistry.Files, fullMethod string) (input, output protoreflect.MessageDescriptor, err error) {
	// Expected shape: "/pkg.Service/Method". The leading slash is mandatory
	// per gRPC wire spec; split on the second slash to separate service
	// from method.
	trimmed := strings.TrimPrefix(fullMethod, "/")
	slash := strings.LastIndex(trimmed, "/")
	if slash <= 0 || slash == len(trimmed)-1 {
		return nil, nil, fmt.Errorf("invalid gRPC method path %q (expected /pkg.Service/Method)", fullMethod)
	}
	serviceName := protoreflect.FullName(trimmed[:slash])
	methodName := protoreflect.Name(trimmed[slash+1:])

	desc, err := files.FindDescriptorByName(serviceName)
	if err != nil {
		return nil, nil, fmt.Errorf("service %q not found in descriptor set: %w", serviceName, err)
	}
	svc, ok := desc.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, nil, fmt.Errorf("descriptor %q is %T, not a service", serviceName, desc)
	}

	method := svc.Methods().ByName(methodName)
	if method == nil {
		// Collect available method names for a helpful error message. Users
		// hit this when they mistyped a method or forgot to update their
		// route table after a proto change.
		var available []string
		for i := 0; i < svc.Methods().Len(); i++ {
			available = append(available, string(svc.Methods().Get(i).Name()))
		}
		return nil, nil, fmt.Errorf("method %q not found on service %q (available: %s)",
			methodName, serviceName, strings.Join(available, ", "))
	}

	return method.Input(), method.Output(), nil
}
