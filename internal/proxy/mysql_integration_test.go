package proxy

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Opt-in real-server acceptance, also run against the isolated local MySQL
// container during gap verification. The regular suite covers wire sequences.
func TestMySQLProxyRealPreparedRoundTrip(t *testing.T) {
	addr := os.Getenv("FAULTBOX_TEST_MYSQL_ADDR")
	if addr == "" {
		t.Skip("set FAULTBOX_TEST_MYSQL_ADDR for real MySQL acceptance")
	}
	p := newMySQLProxy(nil, "mysql")
	listen, err := p.Start(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	cfg := mysql.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = listen
	cfg.User = "root"
	cfg.Passwd = os.Getenv("FAULTBOX_TEST_MYSQL_PASSWORD")
	cfg.Timeout = 3 * time.Second
	cfg.ReadTimeout = 3 * time.Second
	cfg.MultiStatements = true
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for i := 0; i < 3; i++ {
		stmt, err := db.Prepare("SELECT ?, '', ?")
		if err != nil {
			t.Fatal(err)
		}
		var n int
		var empty, value string
		if err := stmt.QueryRow(42, "binary-row").Scan(&n, &empty, &value); err != nil {
			t.Fatal(err)
		}
		if n != 42 || empty != "" || value != "binary-row" {
			t.Fatalf("corrupt row: %d %q %q", n, empty, value)
		}
		if err := stmt.Close(); err != nil {
			t.Fatal(err)
		}
		if err := db.Ping(); err != nil {
			t.Fatalf("pool stalled after statement close: %v", err)
		}
	}
	rows, err := db.Query("SELECT ''; SELECT 2")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("missing empty-string row: %v", rows.Err())
	}
	var first string
	if err := rows.Scan(&first); err != nil || first != "" {
		t.Fatalf("bad empty row: %q %v", first, err)
	}
	if !rows.NextResultSet() || !rows.Next() {
		t.Fatalf("missing second result: %v", rows.Err())
	}
	var second int
	if err := rows.Scan(&second); err != nil || second != 2 {
		t.Fatalf("bad second result: %d %v", second, err)
	}
}
