package protocol

import "testing"

// The v0.16.0 regression. ready() resolves to "<protocol>://host:port" and
// hands that whole string to the plugin's Healthcheck, but every plugin except
// postgres/http/http2 dialled it verbatim — so it tried to reach a host named
// "redis://localhost". DNS never resolves that, the check burned its entire
// timeout, and the service was reported not ready.
//
// Measured before the fix: redis with ready(timeout="60s") failed all three
// tests in its audit spec at exactly 60s, against a container that was serving
// in under a second.
func TestParseAddrStripsScheme(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"redis://localhost:6379", "localhost:6379"},
		{"mysql://localhost:3306", "localhost:3306"},
		{"mongodb://localhost:27017", "localhost:27017"},
		{"cassandra://localhost:9042", "localhost:9042"},
		{"clickhouse://localhost:8123", "localhost:8123"},
		{"nats://localhost:4222", "localhost:4222"},
		{"grpc://localhost:50051", "localhost:50051"},
		{"udp://localhost:9999", "localhost:9999"},
		{"postgres://localhost:5432", "localhost:5432"},

		// A bare address is what ExecuteStep has always been handed, and it
		// must survive untouched.
		{"localhost:6379", "localhost:6379"},
		{"127.0.0.1:5432", "127.0.0.1:5432"},
	}
	for _, tc := range cases {
		if got := ParseAddr(tc.in).HostPort; got != tc.want {
			t.Errorf("ParseAddr(%q).HostPort = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseAddrExtractsCredentials(t *testing.T) {
	a := ParseAddr("postgres://alice:s3cret@db.local:5432/app")
	if a.HostPort != "db.local:5432" {
		t.Errorf("HostPort = %q", a.HostPort)
	}
	if a.User != "alice" || a.Password != "s3cret" {
		t.Errorf("credentials = %q/%q, want alice/s3cret", a.User, a.Password)
	}
	if a.Database != "app" {
		t.Errorf("Database = %q, want app", a.Database)
	}
}

// A user with no password is legitimate (trust auth, or Redis's default ACL
// user). It must not be mistaken for "no credentials at all".
func TestParseAddrUserWithoutPassword(t *testing.T) {
	a := ParseAddr("mysql://root@localhost:3306/app")
	if a.User != "root" {
		t.Errorf("User = %q, want root", a.User)
	}
	if a.Password != "" {
		t.Errorf("Password = %q, want empty", a.Password)
	}
	if a.Database != "app" {
		t.Errorf("Database = %q, want app", a.Database)
	}
}

// Passwords routinely contain characters that are special in a URL. If they
// don't survive the round trip, the plugin authenticates with the wrong
// secret and reports a credential failure the spec author cannot explain.
func TestAddrRoundTripsAwkwardPasswords(t *testing.T) {
	for _, pw := range []string{
		"p@ssw0rd", "with:colon", "with/slash", "with?query",
		"with#hash", "with@at", "sp ace", "100%sure",
	} {
		url := BuildAddr("postgres", "host:5432", Addr{
			User: "u", Password: pw, Database: "db",
		})
		got := ParseAddr(url)
		if got.Password != pw {
			t.Errorf("password %q round-tripped to %q (via %s)", pw, got.Password, url)
		}
		if got.HostPort != "host:5432" {
			t.Errorf("HostPort = %q for %s", got.HostPort, url)
		}
		if got.Database != "db" {
			t.Errorf("Database = %q for %s", got.Database, url)
		}
	}
}

// Redis's --requirepass sets a password and no username. Emitting only the
// user would drop the secret entirely and the plugin would send no AUTH.
func TestBuildAddrKeepsAPasswordWithNoUser(t *testing.T) {
	got := BuildAddr("redis", "h:6379", Addr{Password: "faultbox"})
	if a := ParseAddr(got); a.Password != "faultbox" {
		t.Errorf("password-only credential became %q (via %s)", a.Password, got)
	}
}

func TestBuildAddrOmitsWhatItDoesNotHave(t *testing.T) {
	if got := BuildAddr("redis", "h:6379", Addr{}); got != "redis://h:6379" {
		t.Errorf("no credentials should give a bare URL, got %q", got)
	}
	if got := BuildAddr("mysql", "h:3306", Addr{User: "root"}); got != "mysql://root@h:3306" {
		t.Errorf("user-only URL = %q", got)
	}
	if got := BuildAddr("postgres", "h:5432", Addr{Database: "app"}); got != "postgres://h:5432/app" {
		t.Errorf("database-only URL = %q", got)
	}
}

// Garbage past the scheme must still yield something dialable rather than an
// empty host, so the user sees a connection error naming the address they
// wrote instead of a silent failure.
func TestParseAddrTolerLatesMalformedInput(t *testing.T) {
	if got := ParseAddr("redis://%zz").HostPort; got == "" {
		t.Error("a malformed URL should still surface an address to dial")
	}
	if got := ParseAddr("").HostPort; got != "" {
		t.Errorf("empty input = %q", got)
	}
	// "://" with nothing before it is not a scheme.
	if got := ParseAddr("://host:1").HostPort; got != "://host:1" {
		t.Errorf("a leading :// is not a scheme, got %q", got)
	}
}
