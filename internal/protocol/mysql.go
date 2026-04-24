package protocol

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

func init() {
	Register(&mysqlProtocol{})

	// The go-sql-driver/mysql default logger prints every bad-connection
	// error to stderr — during healthcheck/seed poll that means dozens of
	// "[mysql] packets.go:58 unexpected EOF" lines per cold start, which
	// drowns real signal. We wrap the default with a filter that drops
	// the well-known retry-time noise and passes everything else
	// through. Real failures surface via Query/Exec return values, not
	// via the driver's package logger, so dropping these is safe.
	inner := log.New(os.Stderr, "[mysql] ", log.Ldate|log.Ltime|log.Lshortfile)
	_ = mysql.SetLogger(&mysqlFilterLogger{inner: inner})
}

// mysqlNoisePatterns captures substrings emitted by go-sql-driver/mysql
// during connection-retry loops. These are expected during healthcheck
// polling and don't indicate a real fault.
var mysqlNoisePatterns = []string{
	"unexpected EOF",
	"invalid connection",
	"bad connection",
	"broken pipe",
	"connection refused",
}

type mysqlFilterLogger struct {
	inner mysql.Logger
}

func (l *mysqlFilterLogger) Print(v ...any) {
	msg := fmt.Sprint(v...)
	for _, pat := range mysqlNoisePatterns {
		if strings.Contains(msg, pat) {
			return
		}
	}
	l.inner.Print(v...)
}

type mysqlProtocol struct{}

func (p *mysqlProtocol) Name() string { return "mysql" }

func (p *mysqlProtocol) Methods() []string {
	return []string{"query", "exec"}
}

func (p *mysqlProtocol) Healthcheck(ctx context.Context, addr string, timeout time.Duration) error {
	if err := TCPHealthcheck(ctx, addr, timeout); err != nil {
		return err
	}
	dsn := buildMySQLDSN(addr)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return fmt.Errorf("mysql open: %w", err)
	}
	defer db.Close()
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return db.PingContext(pingCtx)
}

func (p *mysqlProtocol) ExecuteStep(ctx context.Context, addr, method string, kwargs map[string]any) (*StepResult, error) {
	dsn := buildMySQLDSN(addr)
	if cs, ok := kwargs["dsn"].(string); ok && cs != "" {
		dsn = cs
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return &StepResult{Success: false, Error: fmt.Sprintf("open: %v", err)}, nil
	}
	defer db.Close()

	sqlStr := getStringKwarg(kwargs, "sql", "")
	if sqlStr == "" {
		return nil, fmt.Errorf("mysql.%s requires sql= argument", method)
	}

	start := time.Now()
	switch method {
	case "query":
		return executeGenericQuery(ctx, db, sqlStr, start)
	case "exec":
		return executeGenericExec(ctx, db, sqlStr, start)
	default:
		return nil, fmt.Errorf("unsupported mysql method %q", method)
	}
}

func buildMySQLDSN(addr string) string {
	host := addr
	port := "3306"
	if idx := strings.LastIndex(addr, ":"); idx > 0 {
		host = addr[:idx]
		port = addr[idx+1:]
	}
	return fmt.Sprintf("root@tcp(%s:%s)/", host, port)
}

// Generic SQL helpers shared between Postgres and MySQL.

func executeGenericQuery(ctx context.Context, db *sql.DB, sqlStr string, start time.Time) (*StepResult, error) {
	rows, err := db.QueryContext(ctx, sqlStr)
	if err != nil {
		return &StepResult{
			Success:    false,
			Error:      err.Error(),
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	var result []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			continue
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			row[col] = normalizeValue(vals[i])
		}
		result = append(result, row)
	}

	body, _ := json.Marshal(result)
	return &StepResult{
		Body:       string(body),
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
		Fields:     map[string]string{"rows": fmt.Sprintf("%d", len(result))},
	}, nil
}

func executeGenericExec(ctx context.Context, db *sql.DB, sqlStr string, start time.Time) (*StepResult, error) {
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
		Body:       string(body),
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
		Fields:     map[string]string{"rows_affected": fmt.Sprintf("%d", affected)},
	}, nil
}
