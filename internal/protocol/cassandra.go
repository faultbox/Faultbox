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

	// gocql's package logger prints every failed control-connection dial to
	// stderr. A Cassandra cold start takes ~60s, and ready() now retries for
	// the whole of it, so a perfectly healthy run emitted a dozen lines of
	// "unable to dial control conn ... connection reset by peer" before
	// passing. That reads as a failure when it is the readiness check doing
	// exactly its job.
	//
	// Same treatment the MySQL driver gets in mysql.go: drop the known
	// retry-time noise, pass everything else through. Real failures surface
	// through Session/Query return values, not this logger.
	gocql.Logger = &cassandraFilterLogger{inner: gocql.Logger}
}

// cassandraNoisePatterns are emitted by gocql while a node is still starting.
var cassandraNoisePatterns = []string{
	"unable to dial control conn",
	"connection reset by peer",
	"connection refused",
	"unable to fetch peer host info",
	"gocql: no response received from cassandra within timeout period",
}

type cassandraFilterLogger struct{ inner gocql.StdLogger }

func (l *cassandraFilterLogger) filtered(msg string) bool {
	for _, pat := range cassandraNoisePatterns {
		if strings.Contains(msg, pat) {
			return true
		}
	}
	return false
}

func (l *cassandraFilterLogger) Print(v ...any) {
	if !l.filtered(fmt.Sprint(v...)) {
		l.inner.Print(v...)
	}
}

func (l *cassandraFilterLogger) Printf(format string, v ...any) {
	if !l.filtered(fmt.Sprintf(format, v...)) {
		l.inner.Printf(format, v...)
	}
}

func (l *cassandraFilterLogger) Println(v ...any) {
	if !l.filtered(fmt.Sprintln(v...)) {
		l.inner.Println(v...)
	}
}

type cassandraProtocol struct{}

func (p *cassandraProtocol) Name() string { return "cassandra" }

func (p *cassandraProtocol) Methods() []string {
	return []string{"query", "exec"}
}

func (p *cassandraProtocol) Healthcheck(ctx context.Context, addr string, timeout time.Duration) error {
	// Cassandra binds its CQL port well before it will accept a session, and a
	// cold single-node start routinely takes over a minute — so a single
	// attempt at the moment the port opens could never succeed.
	a := ParseAddr(addr)
	return ReadyAfterTCP(ctx, "cassandra", a.HostPort, timeout,
		func(context.Context) error {
			session, err := p.newSession(a, "ONE", 3*time.Second)
			if err != nil {
				return fmt.Errorf("cassandra session: %w", err)
			}
			session.Close()
			return nil
		})
}

func (p *cassandraProtocol) ExecuteStep(ctx context.Context, addr, method string, kwargs map[string]any) (*StepResult, error) {
	cql := getStringKwarg(kwargs, "cql", "")
	if cql == "" {
		return nil, fmt.Errorf("cassandra.%s requires cql= argument", method)
	}
	consistency := getStringKwarg(kwargs, "consistency", "ONE")

	session, err := p.newSession(CredentialsFor(addr, kwargs), consistency, 10*time.Second)
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

// newSession dials Cassandra, carrying any credentials the address supplies.
//
// addr may be a bare "host:port" or "cassandra://user:pass@host:port/keyspace".
// The default image uses AllowAllAuthenticator, so credentials are optional;
// a cluster configured with PasswordAuthenticator needs them, and without this
// every step failed at session setup.
func (p *cassandraProtocol) newSession(a Addr, consistency string, timeout time.Duration) (*gocql.Session, error) {
	host, port := splitHostPort(a.HostPort, 9042)
	cluster := gocql.NewCluster(host)
	cluster.Port = port
	cluster.Consistency = parseConsistency(consistency)
	cluster.Timeout = timeout
	cluster.ConnectTimeout = timeout
	cluster.ProtoVersion = 4
	cluster.DisableInitialHostLookup = true
	if a.Database != "" { // Cassandra calls it a keyspace
		cluster.Keyspace = a.Database
	}
	if a.User != "" {
		cluster.Authenticator = gocql.PasswordAuthenticator{
			Username: a.User,
			Password: a.Password,
		}
	}
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
