package protocol

import (
	"strings"
	"testing"
)

// buildConnStr emitted only host/port/sslmode, so lib/pq fell back to the OS
// user running Faultbox — "root" under sudo, which exists in no Postgres
// installation. The symptom depended on the server's auth method and neither
// was legible:
//
//	trust auth     pq: role "root" does not exist
//	password auth  read: connection reset by peer, after 60 seconds
func TestBuildConnStrCarriesCredentials(t *testing.T) {
	got := buildConnStr("127.0.0.1:32768", "postgres", "s3cret", "app")

	for _, want := range []string{
		"host=127.0.0.1", "port=32768", "sslmode=disable",
		"user=postgres", "password=s3cret", "dbname=app",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("connection string missing %q: %s", want, got)
		}
	}
}

// Absent credentials must be omitted rather than sent empty: "user=" is not
// the same as saying nothing, and an empty dbname changes which database the
// server picks.
func TestBuildConnStrOmitsEmptyFields(t *testing.T) {
	got := buildConnStr("db:5432", "", "", "")
	for _, unwanted := range []string{"user=", "password=", "dbname="} {
		if strings.Contains(got, unwanted) {
			t.Errorf("empty field should be omitted, found %q in: %s", unwanted, got)
		}
	}
	if !strings.Contains(got, "host=db") || !strings.Contains(got, "port=5432") {
		t.Errorf("host/port lost: %s", got)
	}
}

// An address with no port keeps the protocol default rather than producing
// "port=" and failing obscurely.
func TestBuildConnStrDefaultsPort(t *testing.T) {
	got := buildConnStr("db", "u", "", "")
	if !strings.Contains(got, "port=5432") {
		t.Errorf("expected the default port: %s", got)
	}
}
