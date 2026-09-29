package container

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dockertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
)

func TestIndependentClientsOwnNamesNetworksAndCleanup(t *testing.T) {
	var containers []dockertypes.Summary
	var networks []network.Summary
	var removed []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1.45")
		switch {
		case path == "/_ping":
			w.Header().Set("API-Version", "1.45")
			fmt.Fprint(w, "OK")
		case path == "/containers/create":
			var cfg dockertypes.Config
			json.NewDecoder(r.Body).Decode(&cfg)
			if cfg.Hostname != "db" {
				t.Errorf("DNS alias changed: %s", cfg.Hostname)
			}
			id := fmt.Sprintf("container-%04d", len(containers))
			containers = append(containers, dockertypes.Summary{ID: id, Names: []string{"/" + r.URL.Query().Get("name")}, Labels: cfg.Labels})
			json.NewEncoder(w).Encode(map[string]string{"Id": id})
		case path == "/containers/json":
			json.NewEncoder(w).Encode(containers)
		case path == "/networks" && r.Method == "GET":
			json.NewEncoder(w).Encode(networks)
		case path == "/networks/create":
			var cfg struct {
				Name   string
				Labels map[string]string
			}
			json.NewDecoder(r.Body).Decode(&cfg)
			id := fmt.Sprintf("network-%06d", len(networks))
			networks = append(networks, network.Summary{ID: id, Name: cfg.Name, Labels: cfg.Labels})
			json.NewEncoder(w).Encode(map[string]string{"Id": id})
		case r.Method == "DELETE":
			removed = append(removed, path)
			w.WriteHeader(204)
		case strings.HasSuffix(path, "/stop"):
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected Docker request: %s %s", r.Method, path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	t.Setenv("DOCKER_HOST", server.URL)
	t.Setenv("DOCKER_API_VERSION", "1.45")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	first, err := NewClient(ctx, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := NewClient(ctx, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	for _, c := range []*Client{first, second} {
		if _, err := c.EnsureNetwork(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := c.CreateContainer(ctx, CreateOpts{Name: "faultbox-db", Image: "mysql"}); err != nil {
			t.Fatal(err)
		}
	}
	if containers[0].Names[0] == containers[1].Names[0] || networks[0].Name == networks[1].Name || first.SocketDir() == second.SocketDir() {
		t.Fatal("resources collide")
	}
	if len(removed) != 0 {
		t.Fatalf("startup deleted resources: %v", removed)
	}
	if err := second.RemoveContainerByName(ctx, "faultbox-db"); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "/containers/"+containers[1].ID {
		t.Fatalf("removed other run: %v", removed)
	}
	// A legacy or foreign container with a matching name is not ours to remove.
	containers[1].Labels = map[string]string{"io.faultbox.run": "foreign"}
	if err := second.RemoveContainerByName(ctx, "faultbox-db"); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 {
		t.Fatalf("removed foreign resource: %v", removed)
	}
}
