package protocol

import (
	"fmt"
	"io"
)

// Large responses remain bounded, but an incomplete body is never a success.
func readHTTPResponseBody(body io.Reader, kwargs map[string]any) ([]byte, error) {
	limit := int64(16 * 1024 * 1024)
	if raw, ok := kwargs["max_response_bytes"]; ok {
		switch n := raw.(type) {
		case int:
			limit = int64(n)
		case int64:
			limit = n
		default:
			return nil, fmt.Errorf("max_response_bytes must be an integer")
		}
		if limit < 1 || limit > 1024*1024*1024 {
			return nil, fmt.Errorf("max_response_bytes must be between 1 and 1073741824")
		}
	}
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read HTTP response: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("HTTP response exceeds max_response_bytes=%d; increase the limit to read the full body", limit)
	}
	return data, nil
}
func errorString(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}
