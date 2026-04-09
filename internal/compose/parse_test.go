package compose

import (
	"os"
	"strings"
	"testing"
)

const testCompose = `services:
  api:
    image: myapp-api:latest
    ports:
      - "8080:8080"
    depends_on:
      - db
      - redis
    environment:
      DATABASE_URL: postgres://db:5432/mydb
      REDIS_URL: redis://redis:6379

  db:
    image: postgres:16
    ports:
      - "5432:5432"
    environment:
      POSTGRES_DB: mydb
      POSTGRES_PASSWORD: secret

  redis:
    image: redis:7
    ports:
      - "6379:6379"
`

func TestParse(t *testing.T) {
	tmpFile := t.TempDir() + "/docker-compose.yml"
	if err := os.WriteFile(tmpFile, []byte(testCompose), 0644); err != nil {
		t.Fatal(err)
	}

	services, err := Parse(tmpFile)
	if err != nil {
		t.Fatal(err)
	}

	if len(services) != 3 {
		t.Fatalf("expected 3 services, got %d", len(services))
	}

	// Should be topo-sorted: db and redis before api.
	names := make([]string, len(services))
	for i, s := range services {
		names[i] = s.Name
	}
	apiIdx := -1
	for i, n := range names {
		if n == "api" {
			apiIdx = i
		}
	}
	if apiIdx < 2 {
		t.Errorf("api should come after db and redis, got order: %v", names)
	}

	// Check protocol detection.
	for _, svc := range services {
		switch svc.Name {
		case "db":
			if svc.Protocol != "postgres" {
				t.Errorf("db protocol: got %q, want postgres", svc.Protocol)
			}
			if svc.Port != 5432 {
				t.Errorf("db port: got %d, want 5432", svc.Port)
			}
		case "redis":
			if svc.Protocol != "redis" {
				t.Errorf("redis protocol: got %q, want redis", svc.Protocol)
			}
		case "api":
			if svc.Protocol != "http" {
				t.Errorf("api protocol: got %q, want http", svc.Protocol)
			}
			if len(svc.DependsOn) != 2 {
				t.Errorf("api depends_on: got %d, want 2", len(svc.DependsOn))
			}
		}
	}
}

func TestGenerateSpec(t *testing.T) {
	services := []Service{
		{Name: "db", Image: "postgres:16", Protocol: "postgres", Port: 5432,
			Healthcheck: `tcp("localhost:5432")`},
		{Name: "redis", Image: "redis:7", Protocol: "redis", Port: 6379,
			Healthcheck: `tcp("localhost:6379")`},
		{Name: "api", Image: "myapp:latest", Protocol: "http", Port: 8080,
			DependsOn: []string{"db", "redis"},
			Env:       map[string]string{"DATABASE_URL": "postgres://db:5432/mydb"},
			Healthcheck: `http("localhost:8080/health")`},
	}

	spec := GenerateSpec(services)

	// Check service declarations present.
	if !strings.Contains(spec, `db = service("db"`) {
		t.Error("missing db service declaration")
	}
	if !strings.Contains(spec, `api = service("api"`) {
		t.Error("missing api service declaration")
	}
	if !strings.Contains(spec, "depends_on=[db, redis]") {
		t.Error("missing depends_on")
	}
	if !strings.Contains(spec, "scenario(test_happy_path)") {
		t.Error("missing scenario registration")
	}
	if !strings.Contains(spec, `write=deny("EIO")`) {
		t.Error("missing fault example")
	}
}
