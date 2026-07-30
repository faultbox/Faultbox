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

// Healthcheck reports whether MySQL is ready to serve queries.
//
// addr may be a bare "host:port" or a full "mysql://user:pass@host:port/db".
// With credentials this runs a real query rather than Ping: Ping is satisfied
// by a connection the server has accepted but not yet made usable, which is
// exactly the state a booting MySQL passes through for several seconds.
func (p *mysqlProtocol) Healthcheck(ctx context.Context, addr string, timeout time.Duration) error {
	a := ParseAddr(addr)
	db, err := sql.Open("mysql", buildMySQLDSN(a.HostPort, a.User, a.Password, a.Database))
	if err != nil {
		return fmt.Errorf("mysql open: %w", err)
	}
	defer db.Close()

	return ReadyAfterTCP(ctx, "mysql", a.HostPort, timeout,
		func(attemptCtx context.Context) error {
			var one int
			return db.QueryRowContext(attemptCtx, "SELECT 1").Scan(&one)
		})
}

func (p *mysqlProtocol) ExecuteStep(ctx context.Context, addr, method string, kwargs map[string]any) (*StepResult, error) {
	// user=/password=/database= are accepted per step; the runtime fills them
	// from the service's own env when the spec does not, so a stock mysql image
	// works without the spec restating what it already declared. dsn= remains
	// the full escape hatch.
	a := ParseAddr(addr)
	dsn := buildMySQLDSN(a.HostPort,
		getStringKwarg(kwargs, "user", a.User),
		getStringKwarg(kwargs, "password", a.Password),
		getStringKwarg(kwargs, "database", a.Database))
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

// buildMySQLDSN renders a go-sql-driver DSN including credentials.
//
// This used to emit a bare "root@tcp(host:port)/" — no password, no database.
// The runtime computed user/password/database from the service's env and then
// dropped them on the floor here, so `db.exec()` could not authenticate to any
// MySQL configured the way the image requires. Against a stock mysql:8 with
// MYSQL_ROOT_PASSWORD set, every step failed with "Access denied for user
// 'root' (using password: NO)", surfaced to the spec as the far less legible
// "invalid connection". And with no database in the DSN, even a passwordless
// server answered "Error 1046: No database selected" to any real statement.
//
// It survived because no spec asserted on the result of a MySQL step — the
// same reason the equivalent Postgres bug survived to v0.16.0.
func buildMySQLDSN(addr, user, password, database string) string {
	host := addr
	port := "3306"
	if idx := strings.LastIndex(addr, ":"); idx > 0 {
		host = addr[:idx]
		port = addr[idx+1:]
	}
	if user == "" {
		user = "root"
	}
	cred := user
	if password != "" {
		cred = user + ":" + password
	}
	return fmt.Sprintf("%s@tcp(%s:%s)/%s", cred, host, port, database)
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
