package star

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
	"go.starlark.net/starlark"
)

func builtinDecompress(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var format string
	var data, b64 starlark.Value = starlark.None, starlark.None
	limit := 16 * 1024 * 1024
	if err := starlark.UnpackArgs("decompress", args, kwargs, "format", &format, "data?", &data, "data_base64?", &b64, "max_output_bytes?", &limit); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 1024*1024*1024 {
		return nil, fmt.Errorf("decompress: max_output_bytes must be between 1 and 1073741824")
	}
	if (data == starlark.None) == (b64 == starlark.None) {
		return nil, fmt.Errorf("decompress: provide exactly one of data or data_base64")
	}
	var wire []byte
	if b64 != starlark.None {
		text, ok := starlark.AsString(b64)
		if !ok {
			return nil, fmt.Errorf("decompress: data_base64 must be a string")
		}
		var err error
		wire, err = base64.StdEncoding.Strict().DecodeString(text)
		if err != nil {
			return nil, err
		}
	} else {
		switch v := data.(type) {
		case starlark.String:
			wire = []byte(v)
		case starlark.Bytes:
			wire = []byte(v)
		default:
			return nil, fmt.Errorf("decompress: data must be string or bytes")
		}
	}
	var reader io.Reader
	switch format {
	case "gzip":
		r, err := gzip.NewReader(bytes.NewReader(wire))
		if err != nil {
			return nil, err
		}
		defer r.Close()
		reader = r
	case "zstd":
		r, err := zstd.NewReader(bytes.NewReader(wire), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(uint64(limit)))
		if err != nil {
			return nil, err
		}
		defer r.Close()
		reader = r
	default:
		return nil, fmt.Errorf("decompress: format must be gzip or zstd")
	}
	output, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(output) > limit {
		return nil, fmt.Errorf("decompress: output exceeds max_output_bytes=%d", limit)
	}
	return starlark.Bytes(output), nil
}
