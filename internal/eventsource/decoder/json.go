// Package decoder provides built-in decoders for event sources.
package decoder

import (
	"encoding/json"
	"fmt"

	"github.com/faultbox/Faultbox/internal/eventsource"
)

func init() {
	eventsource.RegisterDecoder("json", func(params map[string]string) (eventsource.Decoder, error) {
		return &jsonDecoder{}, nil
	})
}

type jsonDecoder struct{}

func (d *jsonDecoder) Name() string { return "json" }

func (d *jsonDecoder) Decode(raw []byte) (map[string]string, error) {
	// Parse as JSON object. Store the full JSON in "data" field
	// for auto-decoding in Starlark, plus extract top-level string fields.
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		// Not a JSON object — store raw as "data".
		return map[string]string{"data": string(raw)}, nil
	}

	fields := make(map[string]string)
	// Store full JSON in "data" for Starlark auto-decoding.
	fields["data"] = string(raw)
	// Also extract top-level string values as flat fields for dict matching.
	for k, v := range obj {
		if s, ok := v.(string); ok {
			fields[k] = s
		} else {
			fields[k] = fmt.Sprint(v)
		}
	}
	return fields, nil
}
