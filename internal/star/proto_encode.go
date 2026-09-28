package star

import (
	"fmt"

	"github.com/faultbox/Faultbox/internal/protocol"
	"go.starlark.net/starlark"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// proto_encode is reusable for Kafka and any other binary step API.
func (rt *Runtime) builtinProtoEncode(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var path, message string
	var body starlark.Value
	if err := starlark.UnpackArgs("proto_encode", args, kwargs, "descriptors", &path, "message", &message, "body", &body); err != nil {
		return nil, err
	}
	if err := rt.captureResource(rt.resolveSpecPath(path)); err != nil {
		return nil, err
	}
	files, err := protocol.LoadDescriptorSet(rt.resolveSpecPath(path))
	if err != nil {
		return nil, err
	}
	desc, err := files.FindDescriptorByName(protoreflect.FullName(message))
	if err != nil {
		return nil, fmt.Errorf("proto_encode message %q: %w", message, err)
	}
	md, ok := desc.(protoreflect.MessageDescriptor)
	if !ok {
		return nil, fmt.Errorf("proto_encode %q is not a message", message)
	}
	data, err := marshalJSONBody(body)
	if err != nil {
		return nil, err
	}
	wire, err := protocol.JSONToTypedMessage(files, md, data)
	if err != nil {
		return nil, err
	}
	return starlark.Bytes(wire), nil
}
