package protocol

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "github.com/lib/pq" // Postgres driver
)

func init() {
	Register(&postgresProtocol{})
}

type postgresProtocol struct{}

func (p *postgresProtocol) Name() string { return "postgres" }

func (p *postgresProtocol) Methods() []string {
	return []string{"query", "exec"}
}

func (p *postgresProtocol) Healthcheck(ctx context.Context, addr string, timeout time.Duration) error {
	// Try TCP first (faster), then Postgres ping.
	if err := TCPHealthcheck(ctx, addr, timeout); err != nil {
		return err
	}
	// Verify it's actually Postgres by opening a connection.
	//
	// No credentials: this ping is unauthenticated, so on a password-protected
	// server it proves the port speaks Postgres and nothing more. That is the
	// same limit the tcp() healthcheck has, and closing it properly belongs
	// with the readiness-gate work rather than here.
	connStr := buildConnStr(addr, "", "", "")
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return fmt.Errorf("postgres open: %w", err)
	}
	defer db.Close()

	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return db.PingContext(pingCtx)
}

func (p *postgresProtocol) ExecuteStep(ctx context.Context, addr, method string, kwargs map[string]any) (*StepResult, error) {
	// user=/password=/database= are accepted per step; the runtime fills them
	// from the service's own env when the spec does not, so the standard
	// postgres image works without the spec restating what it already
	// declared. connstr= remains the full escape hatch.
	connStr := buildConnStr(addr,
		getStringKwarg(kwargs, "user", ""),
		getStringKwarg(kwargs, "password", ""),
		getStringKwarg(kwargs, "database", ""),
	)
	if cs, ok := kwargs["connstr"].(string); ok && cs != "" {
		connStr = cs
	}

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return &StepResult{Success: false, Error: fmt.Sprintf("open: %v", err)}, nil
	}
	defer db.Close()

	sqlStr := getStringKwarg(kwargs, "sql", "")
	if sqlStr == "" {
		return nil, fmt.Errorf("postgres.%s requires sql= argument", method)
	}

	start := time.Now()

	switch method {
	case "query":
		return p.executeQuery(ctx, db, sqlStr, start)
	case "exec":
		return p.executeExec(ctx, db, sqlStr, start)
	default:
		return nil, fmt.Errorf("unsupported postgres method %q", method)
	}
}

func (p *postgresProtocol) executeQuery(ctx context.Context, db *sql.DB, sqlStr string, start time.Time) (*StepResult, error) {
	rows, err := db.QueryContext(ctx, sqlStr)
	if err != nil {
		return &StepResult{
			Success:    false,
			Error:      err.Error(),
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return &StepResult{
			Success:    false,
			Error:      fmt.Sprintf("columns: %v", err),
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	var result []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return &StepResult{
				Success:    false,
				Error:      fmt.Sprintf("scan: %v", err),
				DurationMs: time.Since(start).Milliseconds(),
			}, nil
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			row[col] = normalizeValue(vals[i])
		}
		result = append(result, row)
	}

	body, _ := json.Marshal(result)
	return &StepResult{
		StatusCode: 0,
		Body:       string(body),
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
		Fields:     map[string]string{"rows": fmt.Sprintf("%d", len(result))},
	}, nil
}

func (p *postgresProtocol) executeExec(ctx context.Context, db *sql.DB, sqlStr string, start time.Time) (*StepResult, error) {
	res, err := db.ExecContext(ctx, sqlStr)
	if err != nil {
		return &StepResult{
			Success:    false,
			Error:      err.Error(),
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	affected, _ := res.RowsAffected()
	body, _ := json.Marshal(map[string]any{"rows_affected": affected})
	return &StepResult{
		StatusCode: 0,
		Body:       string(body),
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
		Fields:     map[string]string{"rows_affected": fmt.Sprintf("%d", affected)},
	}, nil
}

// buildConnStr builds a postgres connection string from addr (host:port) plus
// credentials.
//
// The credentials are the whole point. This function used to emit only
// host/port/sslmode, which meant lib/pq fell back to the OS user running
// Faultbox — `root` under sudo, which exists in no Postgres installation. The
// symptom depended on the server's auth method and neither was legible:
//
//	trust auth     pq: role "root" does not exist
//	password auth  read: connection reset by peer, after 60 seconds
//
// So `pg.sql.exec()` could not authenticate against any realistically
// configured Postgres, and the failure was a timeout rather than a message
// naming the missing credential. Present since the plugin was written; it
// survived because no spec asserted on the result of a postgres step.
func buildConnStr(addr, user, password, database string) string {
	host := addr
	port := "5432"
	if idx := strings.LastIndex(addr, ":"); idx > 0 {
		host = addr[:idx]
		port = addr[idx+1:]
	}
	parts := []string{
		"host=" + host,
		"port=" + port,
		"sslmode=disable",
	}
	if user != "" {
		parts = append(parts, "user="+user)
	}
	if password != "" {
		parts = append(parts, "password="+password)
	}
	if database != "" {
		parts = append(parts, "dbname="+database)
	}
	return strings.Join(parts, " ")
}

// normalizeValue converts sql driver values to JSON-friendly types.
func normalizeValue(v any) any {
	switch val := v.(type) {
	case []byte:
		return string(val)
	case nil:
		return nil
	default:
		return val
	}
}
