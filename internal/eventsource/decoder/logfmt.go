package decoder

import (
	"encoding/json"
	"strings"

	"github.com/faultbox/Faultbox/internal/eventsource"
)

func init() {
	eventsource.RegisterDecoder("logfmt", func(params map[string]string) (eventsource.Decoder, error) {
		return &logfmtDecoder{}, nil
	})
}

type logfmtDecoder struct{}

func (d *logfmtDecoder) Name() string { return "logfmt" }

func (d *logfmtDecoder) Decode(raw []byte) (map[string]string, error) {
	fields := make(map[string]string)
	line := string(raw)

	// Simple logfmt parser: key=value key2="value with spaces"
	for len(line) > 0 {
		line = strings.TrimLeft(line, " \t")
		if len(line) == 0 {
			break
		}

		eqIdx := strings.IndexByte(line, '=')
		if eqIdx < 0 {
			break
		}
		key := line[:eqIdx]
		line = line[eqIdx+1:]

		var val string
		if len(line) > 0 && line[0] == '"' {
			// Quoted value — find closing quote.
			end := strings.IndexByte(line[1:], '"')
			if end < 0 {
				val = line[1:]
				line = ""
			} else {
				val = line[1 : end+1]
				line = line[end+2:]
			}
		} else {
			// Unquoted — read until space.
			spIdx := strings.IndexAny(line, " \t")
			if spIdx < 0 {
				val = line
				line = ""
			} else {
				val = line[:spIdx]
				line = line[spIdx:]
			}
		}
		fields[key] = val
	}

	// Store full fields as JSON in "data" for Starlark auto-decoding.
	if jsonBytes, err := json.Marshal(fields); err == nil {
		fields["data"] = string(jsonBytes)
	}

	return fields, nil
}
