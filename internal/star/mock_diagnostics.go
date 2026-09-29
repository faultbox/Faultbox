package star

import (
	"fmt"
	"strings"
)

// Intentional grpc_error()/HTTP error responses are not infrastructure errors.
// Only failures to execute/encode the mock itself invalidate test evidence.
func mockErrorDiagnostic(ev Event) (Diagnostic, bool) {
	switch ev.Type {
	case "mock.encode_error", "mock.decode_error", "mock.resolve_error", "mock.dynamic_error", "mock.observation_error":
	default:
		return Diagnostic{}, false
	}
	method := ev.Fields["method"]
	if method == "" {
		method = ev.Fields["path"]
	}
	return Diagnostic{
		Level:      "error",
		Code:       strings.ToUpper(strings.ReplaceAll(ev.Type, ".", "_")),
		Message:    fmt.Sprintf("mock %s.%s %s: %s", ev.Service, ev.Fields["interface"], method, ev.Fields["error"]),
		Suggestion: "Fix the mock handler or its protobuf schema/response. Use grpc.error() for intentional upstream errors.",
		Service:    ev.Service,
	}, true
}

func enforceMockErrors(tr *TestResult) {
	for _, ev := range tr.Events {
		if d, ok := mockErrorDiagnostic(ev); ok {
			// Preserve an existing failure's reason; diagnostics still expose the
			// mock problem. A pass, halt or inconclusive result is invalid evidence.
			if tr.Result != "fail" && tr.Result != "error" {
				tr.Result = "fail"
				tr.Reason = d.Code + ": " + d.Message
				tr.FaultBypassed = false
			}
			return
		}
	}
}
