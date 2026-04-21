package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Minimal OpenAPI 3.0 doc used across tests. Keeps each test self-contained
// so changes to one fixture don't accidentally break another.
const petstoreSpec = `
openapi: 3.0.3
info:
  title: Petstore
  version: 1.0.0
paths:
  /pets:
    get:
      responses:
        "200":
          description: a list of pets
          content:
            application/json:
              example:
                - id: 1
                  name: fluffy
                - id: 2
                  name: scruffy
    post:
      responses:
        "201":
          description: pet created
          content:
            application/json:
              example:
                id: 42
                name: new-pet
  /pets/{id}:
    get:
      parameters:
        - name: id
          in: path
          required: true
          schema:
            type: integer
      responses:
        "200":
          description: single pet
          content:
            application/json:
              example:
                id: 1
                name: fluffy
  /healthz:
    get:
      responses:
        "200":
          description: ok (status-only, no body)
`

func writeSpec(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "spec.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestLoadOpenAPI_Valid(t *testing.T) {
	spec, err := LoadOpenAPI(writeSpec(t, petstoreSpec))
	if err != nil {
		t.Fatalf("LoadOpenAPI failed: %v", err)
	}
	if spec.Doc == nil {
		t.Fatal("spec.Doc is nil")
	}
	if spec.Doc.Info.Title != "Petstore" {
		t.Errorf("title = %q, want Petstore", spec.Doc.Info.Title)
	}
}

func TestLoadOpenAPI_Malformed(t *testing.T) {
	_, err := LoadOpenAPI(writeSpec(t, "this: is: not: openapi"))
	if err == nil {
		t.Fatal("expected malformed spec to error, got nil")
	}
}

func TestLoadOpenAPI_MissingFile(t *testing.T) {
	_, err := LoadOpenAPI("/nonexistent/spec.yaml")
	if err == nil {
		t.Fatal("expected missing file to error, got nil")
	}
}

func TestGenerateRoutes_FirstExample(t *testing.T) {
	spec, err := LoadOpenAPI(writeSpec(t, petstoreSpec))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	routes, err := spec.GenerateRoutes(FirstExampleSelector{})
	if err != nil {
		t.Fatalf("GenerateRoutes: %v", err)
	}

	// Expect 4 routes — sorted by (path, method in OpenAPI order).
	wantPatterns := []string{
		"GET /healthz",
		"GET /pets",
		"POST /pets",
		"GET /pets/*",
	}
	gotPatterns := make([]string, 0, len(routes))
	for _, r := range routes {
		gotPatterns = append(gotPatterns, r.Pattern)
	}
	sort.Strings(wantPatterns)
	sort.Strings(gotPatterns)
	if strings.Join(gotPatterns, ",") != strings.Join(wantPatterns, ",") {
		t.Errorf("patterns = %v, want %v", gotPatterns, wantPatterns)
	}

	for _, r := range routes {
		switch r.Pattern {
		case "GET /pets":
			if r.Response.Status != 200 {
				t.Errorf("GET /pets status = %d, want 200", r.Response.Status)
			}
			if r.Response.ContentType != "application/json" {
				t.Errorf("GET /pets content-type = %q", r.Response.ContentType)
			}
			var arr []map[string]any
			if err := json.Unmarshal(r.Response.Body, &arr); err != nil {
				t.Errorf("GET /pets body parse: %v", err)
			}
			if len(arr) != 2 || arr[0]["name"] != "fluffy" {
				t.Errorf("GET /pets body = %s, want array with fluffy", r.Response.Body)
			}
		case "POST /pets":
			if r.Response.Status != 201 {
				t.Errorf("POST /pets status = %d, want 201", r.Response.Status)
			}
		case "GET /pets/*":
			if r.Response.Status != 200 {
				t.Errorf("GET /pets/* status = %d, want 200", r.Response.Status)
			}
		case "GET /healthz":
			// status-only response — empty body, 200.
			if r.Response.Status != 200 {
				t.Errorf("GET /healthz status = %d, want 200", r.Response.Status)
			}
			if len(r.Response.Body) != 0 {
				t.Errorf("GET /healthz body = %q, want empty", r.Response.Body)
			}
		}
	}
}

func TestGenerateRoutes_MissingExampleErrors(t *testing.T) {
	// Operation declares a response with a content schema but no example —
	// Phase 1 must refuse rather than guess.
	const noExample = `
openapi: 3.0.3
info:
  title: t
  version: "1"
paths:
  /x:
    get:
      responses:
        "200":
          description: no example
          content:
            application/json:
              schema:
                type: object
                properties:
                  id: {type: integer}
`
	spec, err := LoadOpenAPI(writeSpec(t, noExample))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	_, err = spec.GenerateRoutes(FirstExampleSelector{})
	if err == nil {
		t.Fatal("expected GenerateRoutes to error on missing example, got nil")
	}
	if !strings.Contains(err.Error(), "example") {
		t.Errorf("error message should mention example, got: %v", err)
	}
}

func TestGenerateRoutes_NamedExamplesMap(t *testing.T) {
	const namedExamples = `
openapi: 3.0.3
info:
  title: t
  version: "1"
paths:
  /x:
    get:
      responses:
        "200":
          description: ok
          content:
            application/json:
              examples:
                happy:
                  value: {"ok": true}
                sad:
                  value: {"ok": false}
`
	spec, err := LoadOpenAPI(writeSpec(t, namedExamples))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	routes, err := spec.GenerateRoutes(FirstExampleSelector{})
	if err != nil {
		t.Fatalf("GenerateRoutes: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("got %d routes, want 1", len(routes))
	}
	// FirstExampleSelector picks examples in alphabetical order — "happy"
	// comes before "sad". This is deterministic by design.
	var got map[string]any
	if err := json.Unmarshal(routes[0].Response.Body, &got); err != nil {
		t.Fatalf("body parse: %v", err)
	}
	if got["ok"] != true {
		t.Errorf("expected examples.happy (ok=true), got %v", got)
	}
}

func TestToGlobPattern(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/pets", "/pets"},
		{"/pets/{id}", "/pets/*"},
		{"/users/{uid}/items/{iid}", "/users/*/items/*"},
		{"/", "/"},
	}
	for _, c := range cases {
		if got := OpenAPIPathToGlob(c.in); got != c.want {
			t.Errorf("OpenAPIPathToGlob(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNamedExampleSelector(t *testing.T) {
	const spec = `
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /x:
    get:
      responses:
        "200":
          description: ok
          content:
            application/json:
              examples:
                happy:  {value: {"status": "ok"}}
                sad:    {value: {"status": "down"}}
`
	s, err := LoadOpenAPI(writeSpec(t, spec))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	routes, err := s.GenerateRoutes(NamedExampleSelector{Name: "sad"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(routes[0].Response.Body, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["status"] != "down" {
		t.Errorf("expected named sad example, got %v", got)
	}
}

func TestNamedExampleSelector_FallsBackOnMissing(t *testing.T) {
	// One op has "sad", the other only has inline `example:`. Missing-name
	// should fall back to first rather than hard-error.
	const spec = `
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /a:
    get:
      responses:
        "200":
          description: ok
          content:
            application/json:
              examples:
                sad: {value: {"who": "a-sad"}}
  /b:
    get:
      responses:
        "200":
          description: ok
          content:
            application/json:
              example: {"who": "b-only"}
`
	s, err := LoadOpenAPI(writeSpec(t, spec))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	routes, err := s.GenerateRoutes(NamedExampleSelector{Name: "sad"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	byPattern := map[string]string{}
	for _, r := range routes {
		byPattern[r.Pattern] = string(r.Response.Body)
	}
	if !strings.Contains(byPattern["GET /a"], "a-sad") {
		t.Errorf("/a expected sad example, got %q", byPattern["GET /a"])
	}
	if !strings.Contains(byPattern["GET /b"], "b-only") {
		t.Errorf("/b expected fallback to first example, got %q", byPattern["GET /b"])
	}
}

func TestRandomExampleSelector_Deterministic(t *testing.T) {
	const spec = `
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /x:
    get:
      responses:
        "200":
          description: ok
          content:
            application/json:
              examples:
                a: {value: {"v": "a"}}
                b: {value: {"v": "b"}}
                c: {value: {"v": "c"}}
`
	s, err := LoadOpenAPI(writeSpec(t, spec))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Same seed → same pick. Different seed → may differ. We only check
	// reproducibility: two selectors with the same seed return the same body.
	r1, err := s.GenerateRoutes(NewRandomExampleSelector(42))
	if err != nil {
		t.Fatalf("r1: %v", err)
	}
	r2, err := s.GenerateRoutes(NewRandomExampleSelector(42))
	if err != nil {
		t.Fatalf("r2: %v", err)
	}
	if string(r1[0].Response.Body) != string(r2[0].Response.Body) {
		t.Errorf("same seed produced different bodies: %q vs %q",
			r1[0].Response.Body, r2[0].Response.Body)
	}
}

func TestFirstExampleSelector_SynthesizesMissing(t *testing.T) {
	const spec = `
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /x:
    get:
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                type: object
                properties:
                  id:     {type: integer}
                  name:   {type: string}
                  tags:   {type: array, items: {type: string}}
                  active: {type: boolean}
`
	s, err := LoadOpenAPI(writeSpec(t, spec))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Without synthesize: expect error.
	if _, err := s.GenerateRoutes(FirstExampleSelector{}); err == nil {
		t.Fatal("expected error without SynthesizeMissing, got nil")
	}

	// With synthesize: expect minimal type-correct JSON.
	routes, err := s.GenerateRoutes(FirstExampleSelector{SynthesizeMissing: true})
	if err != nil {
		t.Fatalf("generate with synthesize: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(routes[0].Response.Body, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["id"] != float64(0) {
		t.Errorf("id = %v, want 0", got["id"])
	}
	if got["name"] != "" {
		t.Errorf("name = %v, want empty string", got["name"])
	}
	if got["active"] != false {
		t.Errorf("active = %v, want false", got["active"])
	}
	if _, ok := got["tags"].([]any); !ok {
		t.Errorf("tags = %v, want array", got["tags"])
	}
}

func TestValidateRequest_Strict(t *testing.T) {
	const spec = `
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /pets:
    post:
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [name]
              properties:
                name: {type: string, minLength: 1}
      responses:
        "201":
          description: created
          content:
            application/json:
              example: {"id": 1, "name": "x"}
`
	s, err := LoadOpenAPI(writeSpec(t, spec))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Valid body → nil error.
	if err := s.ValidateRequest("POST", "/pets", []byte(`{"name":"fluffy"}`), "application/json"); err != nil {
		t.Errorf("valid body should pass: %v", err)
	}

	// Missing required field → error.
	if err := s.ValidateRequest("POST", "/pets", []byte(`{}`), "application/json"); err == nil {
		t.Error("expected missing-field error, got nil")
	}

	// Wrong type → error.
	if err := s.ValidateRequest("POST", "/pets", []byte(`{"name":123}`), "application/json"); err == nil {
		t.Error("expected type error, got nil")
	}

	// Empty body on required → error.
	if err := s.ValidateRequest("POST", "/pets", nil, "application/json"); err == nil {
		t.Error("expected required-body error, got nil")
	}

	// No matching op → nil (validation is best-effort).
	if err := s.ValidateRequest("POST", "/unknown", []byte(`{}`), "application/json"); err != nil {
		t.Errorf("unknown path should be silent: %v", err)
	}
}

func TestValidateRequest_PathTemplate(t *testing.T) {
	const spec = `
openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /pets/{id}/tags:
    post:
      parameters:
        - {name: id, in: path, required: true, schema: {type: integer}}
      requestBody:
        required: true
        content:
          application/json:
            schema:
              type: object
              required: [tag]
              properties:
                tag: {type: string}
      responses:
        "200":
          description: ok
          content:
            application/json:
              example: {}
`
	s, err := LoadOpenAPI(writeSpec(t, spec))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Concrete path should match the {id} template.
	if err := s.ValidateRequest("POST", "/pets/42/tags", []byte(`{"tag":"cute"}`), "application/json"); err != nil {
		t.Errorf("template-match should validate: %v", err)
	}
	if err := s.ValidateRequest("POST", "/pets/42/tags", []byte(`{}`), "application/json"); err == nil {
		t.Error("expected missing-tag error")
	}
}

func TestLoadOpenAPI_RejectsHTTPRef(t *testing.T) {
	const withHTTPRef = `
openapi: 3.0.3
info:
  title: t
  version: "1"
paths:
  /x:
    $ref: "http://evil.example.com/path.yaml"
`
	_, err := LoadOpenAPI(writeSpec(t, withHTTPRef))
	if err == nil {
		t.Fatal("expected http:// $ref to be rejected, got nil error")
	}
	if !strings.Contains(err.Error(), "not allowed") && !strings.Contains(err.Error(), "http") {
		t.Errorf("error should mention http rejection, got: %v", err)
	}
}
