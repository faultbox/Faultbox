package decoder

import (
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/faultbox/Faultbox/internal/eventsource"
)

func init() {
	eventsource.RegisterDecoder("regex", func(params map[string]string) (eventsource.Decoder, error) {
		pattern, ok := params["pattern"]
		if !ok || pattern == "" {
			return nil, fmt.Errorf("regex decoder requires 'pattern' parameter")
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("regex decoder: invalid pattern: %w", err)
		}
		return &regexDecoder{re: re}, nil
	})
}

type regexDecoder struct {
	re *regexp.Regexp
}

func (d *regexDecoder) Name() string { return "regex" }

func (d *regexDecoder) Decode(raw []byte) (map[string]string, error) {
	match := d.re.FindSubmatch(raw)
	if match == nil {
		return nil, fmt.Errorf("regex: no match for %q", string(raw))
	}

	fields := make(map[string]string)
	for i, name := range d.re.SubexpNames() {
		if i > 0 && name != "" && i < len(match) {
			fields[name] = string(match[i])
		}
	}

	// Store as JSON in "data" for Starlark auto-decoding.
	if jsonBytes, err := json.Marshal(fields); err == nil {
		fields["data"] = string(jsonBytes)
	}

	return fields, nil
}
