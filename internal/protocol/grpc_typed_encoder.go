package protocol

import (
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

// JSONToTypedMessage encodes the given JSON bytes as a wire-format protobuf
// message of the exact type described by `desc`. Used by the typed gRPC
// mock path (RFC-023) so specs can declare responses as ordinary Starlark
// dicts and have them emitted on the wire as the customer's real message
// type — not as google.protobuf.Struct, which Go clients with compiled
// stubs reject at decode time.
//
// Implementation: build a dynamicpb.Message bound to `desc`, let protojson
// parse the JSON into it (resolver threaded through so google.protobuf.*
// well-known types work), then proto.Marshal to wire bytes. dynamicpb
// satisfies proto.Message so both ends of the pipeline are stdlib.
//
// `files` may be nil when the message has no well-known-type dependencies;
// pass the per-mock registry from LoadDescriptorSet to resolve Timestamp,
// Any, etc.
//
// Error shape is deliberately user-facing — spec authors will see this
// directly when a route's response dict doesn't match its message type:
//
//	grpc mock response for "/inDriver.geo_config.GeoConfigService/GetCity":
//	  encode as inDriver.geo_config.City: proto: (line 1:12): unknown field "cityid"
//
// which tells the author exactly which route + message + field is wrong.
func JSONToTypedMessage(files *protoregistry.Files, desc protoreflect.MessageDescriptor, jsonBytes []byte) ([]byte, error) {
	msg := dynamicpb.NewMessage(desc)

	opts := protojson.UnmarshalOptions{
		// DiscardUnknown: false so typos surface immediately rather than
		// silently producing a partially-populated message on the wire.
		DiscardUnknown: false,
		// Resolver lets protojson find nested message types (Any's inner
		// type URL resolution in particular). Falls back to GlobalFiles
		// if the per-mock registry is nil, which covers simple cases
		// without WKT dependencies.
		Resolver: typesResolver{files: files},
	}
	if err := opts.Unmarshal(jsonBytes, msg); err != nil {
		return nil, fmt.Errorf("encode as %s: %w", desc.FullName(), err)
	}
	return proto.Marshal(msg)
}

// typesResolver adapts a *protoregistry.Files to the protojson.Resolver
// interface. protojson needs to look up message types by full name (for
// Any unpacking) and extension types; we forward to the per-mock
// registry and fall back to GlobalFiles for types the customer's .pb
// doesn't declare (the well-known types are registered globally via
// side-effect imports in grpc_descriptors.go).
type typesResolver struct {
	files *protoregistry.Files
}

func (r typesResolver) FindMessageByName(name protoreflect.FullName) (protoreflect.MessageType, error) {
	if r.files != nil {
		if desc, err := r.files.FindDescriptorByName(name); err == nil {
			if md, ok := desc.(protoreflect.MessageDescriptor); ok {
				return dynamicpb.NewMessageType(md), nil
			}
		}
	}
	// Global fallback — covers google.protobuf.* WKTs and anything else
	// already linked into the binary.
	return protoregistry.GlobalTypes.FindMessageByName(name)
}

func (r typesResolver) FindMessageByURL(url string) (protoreflect.MessageType, error) {
	// Any URLs are "type.googleapis.com/<full.name>" — strip the host and
	// delegate. Empty host is also permitted per the spec.
	name := url
	for i := len(url) - 1; i >= 0; i-- {
		if url[i] == '/' {
			name = url[i+1:]
			break
		}
	}
	return r.FindMessageByName(protoreflect.FullName(name))
}

func (r typesResolver) FindExtensionByName(name protoreflect.FullName) (protoreflect.ExtensionType, error) {
	return protoregistry.GlobalTypes.FindExtensionByName(name)
}

func (r typesResolver) FindExtensionByNumber(message protoreflect.FullName, field protoreflect.FieldNumber) (protoreflect.ExtensionType, error) {
	return protoregistry.GlobalTypes.FindExtensionByNumber(message, field)
}
