package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gocql/gocql"
)

func init() {
	Register(&cassandraProtocol{})
}

type cassandraProtocol struct{}

func (p *cassandraProtocol) Name() string { return "cassandra" }

func (p *cassandraProtocol) Methods() []string {
	return []string{"query", "exec"}
}

func (p *cassandraProtocol) Healthcheck(ctx context.Context, addr string, timeout time.Duration) error {
	if err := TCPHealthcheck(ctx, addr, timeout); err != nil {
		return err
	}
	session, err := p.newSession(addr, "ONE", 3*time.Second)
	if err != nil {
		return fmt.Errorf("cassandra session: %w", err)
	}
	session.Close()
	return nil
}

func (p *cassandraProtocol) ExecuteStep(ctx context.Context, addr, method string, kwargs map[string]any) (*StepResult, error) {
	cql := getStringKwarg(kwargs, "cql", "")
	if cql == "" {
		return nil, fmt.Errorf("cassandra.%s requires cql= argument", method)
	}
	consistency := getStringKwarg(kwargs, "consistency", "ONE")

	session, err := p.newSession(addr, consistency, 10*time.Second)
	if err != nil {
		return &StepResult{Success: false, Error: fmt.Sprintf("session: %v", err)}, nil
	}
	defer session.Close()

	start := time.Now()
	switch method {
	case "query":
		return p.executeQuery(ctx, session, cql, start)
	case "exec":
		return p.executeExec(ctx, session, cql, start)
	default:
		return nil, fmt.Errorf("unsupported cassandra method %q", method)
	}
}

func (p *cassandraProtocol) newSession(addr, consistency string, timeout time.Duration) (*gocql.Session, error) {
	host, port := splitHostPort(addr, 9042)
	cluster := gocql.NewCluster(host)
	cluster.Port = port
	cluster.Consistency = parseConsistency(consistency)
	cluster.Timeout = timeout
	cluster.ConnectTimeout = timeout
	cluster.ProtoVersion = 4
	cluster.DisableInitialHostLookup = true
	return cluster.CreateSession()
}

func (p *cassandraProtocol) executeQuery(ctx context.Context, session *gocql.Session, cql string, start time.Time) (*StepResult, error) {
	iter := session.Query(cql).WithContext(ctx).Iter()
	cols := iter.Columns()

	var rows []map[string]any
	for {
		row := make(map[string]any, len(cols))
		if !iter.MapScan(row) {
			break
		}
		rows = append(rows, normalizeCassandraRow(row))
	}
	if err := iter.Close(); err != nil {
		return &StepResult{Success: false, Error: err.Error(), DurationMs: time.Since(start).Milliseconds()}, nil
	}

	body, _ := json.Marshal(rows)
	return &StepResult{
		Body:       string(body),
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
		Fields:     map[string]string{"rows": fmt.Sprintf("%d", len(rows))},
	}, nil
}

func (p *cassandraProtocol) executeExec(ctx context.Context, session *gocql.Session, cql string, start time.Time) (*StepResult, error) {
	if err := session.Query(cql).WithContext(ctx).Exec(); err != nil {
		return &StepResult{Success: false, Error: err.Error(), DurationMs: time.Since(start).Milliseconds()}, nil
	}
	body, _ := json.Marshal(map[string]any{"ok": true})
	return &StepResult{
		Body:       string(body),
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

// parseConsistency maps the Starlark-facing consistency name to gocql's enum.
// Unknown values default to ONE — matches Cassandra's own default.
func parseConsistency(s string) gocql.Consistency {
	switch strings.ToUpper(s) {
	case "ANY":
		return gocql.Any
	case "ONE":
		return gocql.One
	case "TWO":
		return gocql.Two
	case "THREE":
		return gocql.Three
	case "QUORUM":
		return gocql.Quorum
	case "ALL":
		return gocql.All
	case "LOCAL_QUORUM":
		return gocql.LocalQuorum
	case "EACH_QUORUM":
		return gocql.EachQuorum
	case "LOCAL_ONE":
		return gocql.LocalOne
	default:
		return gocql.One
	}
}

// normalizeCassandraRow converts gocql-decoded values into JSON-friendly
// types (UUIDs to strings, []byte to strings, etc.).
func normalizeCassandraRow(row map[string]any) map[string]any {
	out := make(map[string]any, len(row))
	for k, v := range row {
		out[k] = normalizeCassandraValue(v)
	}
	return out
}

func normalizeCassandraValue(v any) any {
	switch val := v.(type) {
	case gocql.UUID:
		return val.String()
	case []byte:
		return string(val)
	case time.Time:
		return val.UTC().Format(time.RFC3339Nano)
	default:
		return v
	}
}

// splitHostPort splits "host:port" into components, falling back to the
// default port if unspecified.
func splitHostPort(addr string, defaultPort int) (string, int) {
	idx := strings.LastIndex(addr, ":")
	if idx < 0 {
		return addr, defaultPort
	}
	host := addr[:idx]
	var port int
	fmt.Sscanf(addr[idx+1:], "%d", &port)
	if port == 0 {
		port = defaultPort
	}
	return host, port
}
