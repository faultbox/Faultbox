package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const clientsTestContract = `
openapi: 3.0.3
info:
  title: Orders
  version: 1.4.0
paths:
  /orders:
    post:
      operationId: createOrder
      requestBody:
        required: true
        content:
          application/json:
            schema: {type: object}
      responses:
        "201": {description: created}
  /orders/{orderId}:
    get:
      operationId: getOrder
      parameters:
        - name: orderId
          in: path
          required: true
          schema: {type: integer}
      responses:
        "200": {description: ok}
`

func writeClientSpec(t *testing.T, starBody string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "orders.yaml"), []byte(clientsTestContract), 0o644); err != nil {
		t.Fatalf("write contract: %v", err)
	}
	specPath := filepath.Join(dir, "faultbox.star")
	if err := os.WriteFile(specPath, []byte(starBody), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	return specPath
}

func TestInspectClients_PrintsOperationTable(t *testing.T) {
	spec := writeClientSpec(t, `
orders = service("orders", interface("public", "http", 8080), image = "orders:1")
api = client("mobile-app", target = orders.public, openapi = "./orders.yaml",
             headers = {"X-Client": "ios"}, validate = "response")
`)

	var buf bytes.Buffer
	if code := printSpecClients(&buf, spec); code != 0 {
		t.Fatalf("printSpecClients exit code = %d, want 0\n%s", code, buf.String())
	}
	out := buf.String()

	for _, want := range []string{
		"mobile-app",
		"orders.public (http)",
		"validate:  response",
		"X-Client",
		"operations (2):",
		"create_order(body)",
		"POST /orders",
		"get_order(order_id)",
		"GET /orders/{orderId}",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestInspectClients_NoClientsIsNotAnError(t *testing.T) {
	spec := writeClientSpec(t, `
orders = service("orders", interface("public", "http", 8080), image = "orders:1")
`)

	var buf bytes.Buffer
	if code := printSpecClients(&buf, spec); code != 0 {
		t.Fatalf("exit code = %d, want 0 — a spec with no clients is a normal state", code)
	}
	if !strings.Contains(buf.String(), "declares no clients") {
		t.Errorf("expected a helpful no-clients message, got:\n%s", buf.String())
	}
	// The message should show how to declare one rather than just saying no.
	if !strings.Contains(buf.String(), "client(") {
		t.Errorf("no-clients message should show the declaration form, got:\n%s", buf.String())
	}
}

func TestInspectClients_BadSpecExitsNonZero(t *testing.T) {
	spec := writeClientSpec(t, `this is not starlark(((`)
	var buf bytes.Buffer
	if code := printSpecClients(&buf, spec); code == 0 {
		t.Error("expected a non-zero exit for a spec that fails to load")
	}
}
