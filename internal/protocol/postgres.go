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
	connStr := buildConnStr(addr)
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
	connStr := buildConnStr(addr)
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

// buildConnStr builds a postgres connection string from addr (host:port).
func buildConnStr(addr string) string {
	host := addr
	port := "5432"
	if idx := strings.LastIndex(addr, ":"); idx > 0 {
		host = addr[:idx]
		port = addr[idx+1:]
	}
	return fmt.Sprintf("host=%s port=%s sslmode=disable", host, port)
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
