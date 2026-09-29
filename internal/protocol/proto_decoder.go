package protocol

import (
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// LoadProtoDecoder resolves once per operation; record loops never reload a
// descriptor file. Its JSON follows the same rules as typed gRPC mocks.
func LoadProtoDecoder(path, message string) (func([]byte) (json.RawMessage, error), error) {
	files, err := LoadDescriptorSet(path)
	if err != nil {
		return nil, err
	}
	desc, err := files.FindDescriptorByName(protoreflect.FullName(message))
	if err != nil {
		return nil, fmt.Errorf("protobuf message %q: %w", message, err)
	}
	md, ok := desc.(protoreflect.MessageDescriptor)
	if !ok {
		return nil, fmt.Errorf("%q is not a protobuf message", message)
	}
	return func(wire []byte) (json.RawMessage, error) {
		data, err := TypedMessageToJSON(files, md, wire)
		return json.RawMessage(data), err
	}, nil
}
