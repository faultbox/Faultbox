package protocol

import (
	"strings"
	"testing"
)

// The MySQL half of the bug that RFC-056's corpus pattern found.
//
// buildMySQLDSN emitted a bare "root@tcp(host:port)/" — no password, no
// database — while the runtime was busy computing both from the service's env
// and handing them over. Against a stock mysql:8 (which requires
// MYSQL_ROOT_PASSWORD) every step failed with "Access denied for user 'root'
// (using password: NO)", reaching the spec as "invalid connection". Even
// against a passwordless server, the missing database meant any real statement
// returned "Error 1046: No database selected".
//
// Same shape as the Postgres bug fixed in v0.16.0, same reason it survived:
// no spec asserted on the result of a MySQL step.
func TestBuildMySQLDSNCarriesCredentials(t *testing.T) {
	got := buildMySQLDSN("db:3306", "app", "s3cret", "shop")
	want := "app:s3cret@tcp(db:3306)/shop"
	if got != want {
		t.Errorf("DSN = %q, want %q", got, want)
	}
}

// The regression guard: a DSN that names no password cannot reach a server
// that requires one.
func TestBuildMySQLDSNIncludesThePassword(t *testing.T) {
	got := buildMySQLDSN("db:3306", "root", "faultbox", "app")
	if !strings.Contains(got, "faultbox") {
		t.Errorf("DSN %q omits the password; this is the bug", got)
	}
	if strings.HasSuffix(got, "/") {
		t.Errorf("DSN %q selects no database; real statements fail with error 1046", got)
	}
}

func TestBuildMySQLDSNDefaultsToRoot(t *testing.T) {
	if got := buildMySQLDSN("db:3306", "", "", ""); got != "root@tcp(db:3306)/" {
		t.Errorf("DSN = %q, want the previous credential-free form", got)
	}
}

func TestBuildMySQLDSNDefaultsThePort(t *testing.T) {
	if got := buildMySQLDSN("db", "root", "", ""); got != "root@tcp(db:3306)/" {
		t.Errorf("DSN = %q, want the default port applied", got)
	}
}

// The healthcheck path parses a URL; the step path is handed a bare address.
// Both must produce the same DSN, or ready() and the steps disagree about who
// they are connecting as — which is how a service reports ready and then fails
// every statement.
func TestHealthcheckAndStepAgreeOnCredentials(t *testing.T) {
	a := ParseAddr("mysql://app:s3cret@db:3306/shop")
	fromURL := buildMySQLDSN(a.HostPort, a.User, a.Password, a.Database)
	fromKwargs := buildMySQLDSN("db:3306", "app", "s3cret", "shop")
	if fromURL != fromKwargs {
		t.Errorf("healthcheck DSN %q != step DSN %q", fromURL, fromKwargs)
	}
}
