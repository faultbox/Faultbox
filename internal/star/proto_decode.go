package star

import (
	"encoding/base64"
	"fmt"

	"github.com/faultbox/Faultbox/internal/protocol"
	"go.starlark.net/starlark"
)

func (rt *Runtime) builtinProtoDecode(thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var path, message string
	var data, b64 starlark.Value = starlark.None, starlark.None
	if err := starlark.UnpackArgs("proto_decode", args, kwargs, "descriptors", &path, "message", &message, "data?", &data, "data_base64?", &b64); err != nil {
		return nil, err
	}
	if (data == starlark.None) == (b64 == starlark.None) {
		return nil, fmt.Errorf("proto_decode requires exactly one of data or data_base64")
	}
	var wire []byte
	if b64 != starlark.None {
		text, ok := starlark.AsString(b64)
		if !ok {
			return nil, fmt.Errorf("proto_decode data_base64 must be a string")
		}
		var err error
		wire, err = base64.StdEncoding.Strict().DecodeString(text)
		if err != nil {
			return nil, fmt.Errorf("proto_decode base64: %w", err)
		}
	} else {
		switch value := data.(type) {
		case starlark.Bytes:
			wire = []byte(value)
		case starlark.String:
			wire = []byte(value)
		default:
			return nil, fmt.Errorf("proto_decode data must be bytes or a byte-carrying string")
		}
	}
	resolved := rt.resolveCallerPath(thread, path)
	if err := rt.captureResource(resolved); err != nil {
		return nil, err
	}
	decode, err := protocol.LoadProtoDecoder(resolved, message)
	if err != nil {
		return nil, err
	}
	decoded, err := decode(wire)
	if err != nil {
		return nil, err
	}
	return jsonToStarlark(string(decoded)), nil
}
