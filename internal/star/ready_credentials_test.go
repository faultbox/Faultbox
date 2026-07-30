package star

import (
	"strings"
	"testing"

	"github.com/faultbox/Faultbox/internal/protocol"
)

// resolveReadyCheck builds the address ready() hands to a plugin. Every
// protocol with a registered plugin must come back as a URL that plugin can
// parse — the v0.16.0 shape was right, but the plugins couldn't read it.
func TestReadyCheckIsParseableByPlugins(t *testing.T) {
	for _, proto := range []string{
		"postgres", "mysql", "redis", "mongodb",
		"cassandra", "clickhouse", "nats", "kafka", "grpc", "udp", "tcp",
	} {
		svc := &ServiceDef{
			Name:       "svc",
			Image:      "img",
			Interfaces: map[string]*InterfaceDef{"main": {Name: "main", Protocol: proto, Port: 1234, HostPort: 32768}},
		}
		rt := New(testLogger())
		check := rt.resolveReadyCheck(svc)
		if check == "" {
			t.Errorf("%s: resolveReadyCheck returned nothing", proto)
			continue
		}
		if got := protocol.ParseAddr(check).HostPort; got != "localhost:32768" {
			t.Errorf("%s: ready() resolved to %q, which a plugin parses as host %q "+
				"— it would dial that literally and time out", proto, check, got)
		}
	}
}

// Credentials the spec already declared must reach the readiness check.
// Without them "ready" can only mean "the port is open", which is the guess
// ready() exists to replace.
func TestReadyCheckCarriesDeclaredCredentials(t *testing.T) {
	cases := []struct {
		proto          string
		env            map[string]string
		user, pass, db string
	}{
		{"postgres", map[string]string{"POSTGRES_PASSWORD": "pw", "POSTGRES_DB": "app"}, "postgres", "pw", "app"},
		{"mysql", map[string]string{"MYSQL_ROOT_PASSWORD": "pw", "MYSQL_DATABASE": "app"}, "root", "pw", "app"},
		{"mysql", map[string]string{"MYSQL_USER": "app", "MYSQL_PASSWORD": "pw", "MYSQL_DATABASE": "shop"}, "app", "pw", "shop"},
		{"redis", map[string]string{"REDIS_PASSWORD": "pw"}, "", "pw", ""},
		{"mongodb", map[string]string{"MONGO_INITDB_ROOT_USERNAME": "root", "MONGO_INITDB_ROOT_PASSWORD": "pw"}, "root", "pw", ""},
		{"clickhouse", map[string]string{"CLICKHOUSE_USER": "ch", "CLICKHOUSE_PASSWORD": "pw", "CLICKHOUSE_DB": "logs"}, "ch", "pw", "logs"},
		{"cassandra", map[string]string{"CASSANDRA_USER": "cas", "CASSANDRA_PASSWORD": "pw"}, "cas", "pw", ""},
	}
	for _, tc := range cases {
		svc := &ServiceDef{
			Name:       "svc",
			Image:      "img",
			Env:        tc.env,
			Interfaces: map[string]*InterfaceDef{"main": {Name: "main", Protocol: tc.proto, Port: 1234, HostPort: 32768}},
		}
		rt := New(testLogger())
		a := protocol.ParseAddr(rt.resolveReadyCheck(svc))
		if a.User != tc.user || a.Password != tc.pass || a.Database != tc.db {
			t.Errorf("%s %v: ready() carried %q/%q/%q, want %q/%q/%q",
				tc.proto, tc.env, a.User, a.Password, a.Database, tc.user, tc.pass, tc.db)
		}
	}
}

// A password with URL metacharacters must survive the trip through the URL
// form, or the plugin authenticates with a different secret than the spec
// declared and the failure looks like a wrong password.
func TestReadyCheckSurvivesAwkwardPasswords(t *testing.T) {
	for _, pw := range []string{"p@ss:w/rd", "100%sure", "a#b?c"} {
		svc := &ServiceDef{
			Name:       "svc",
			Image:      "img",
			Env:        map[string]string{"POSTGRES_PASSWORD": pw},
			Interfaces: map[string]*InterfaceDef{"main": {Name: "main", Protocol: "postgres", Port: 5432, HostPort: 32768}},
		}
		rt := New(testLogger())
		check := rt.resolveReadyCheck(svc)
		if got := protocol.ParseAddr(check).Password; got != pw {
			t.Errorf("password %q became %q (via %s)", pw, got, check)
		}
	}
}

// HTTP is the one protocol whose readiness is a path to fetch rather than a
// credential URL, and it must stay that way.
func TestReadyCheckForHTTPIsAFetchableURL(t *testing.T) {
	svc := &ServiceDef{
		Name:       "api",
		Image:      "img",
		Interfaces: map[string]*InterfaceDef{"main": {Name: "main", Protocol: "http", Port: 8080, HostPort: 32768}},
	}
	rt := New(testLogger())
	if got := rt.resolveReadyCheck(svc); !strings.HasPrefix(got, "http://localhost:32768/") {
		t.Errorf("http ready() = %q, want a fetchable URL", got)
	}
}

// MySQL's two credential conventions. MYSQL_USER creates an additional account
// and is what MYSQL_DATABASE grants; MYSQL_ROOT_PASSWORD is the root account.
// Mixing them up authenticates as a user that has no rights to the database.
func TestMySQLPrefersTheNamedUserOverRoot(t *testing.T) {
	args := map[string]any{}
	applyServiceCredentials(args, &ServiceDef{Env: map[string]string{
		"MYSQL_ROOT_PASSWORD": "rootpw",
		"MYSQL_USER":          "app",
		"MYSQL_PASSWORD":      "apppw",
	}}, "mysql")
	if args["user"] != "app" || args["password"] != "apppw" {
		t.Errorf("with MYSQL_USER set, got %v/%v, want app/apppw", args["user"], args["password"])
	}

	args = map[string]any{}
	applyServiceCredentials(args, &ServiceDef{Env: map[string]string{
		"MYSQL_ROOT_PASSWORD": "rootpw",
	}}, "mysql")
	if args["user"] != "root" || args["password"] != "rootpw" {
		t.Errorf("with only a root password, got %v/%v, want root/rootpw", args["user"], args["password"])
	}
}

// The official Mongo image only enables authentication when both the username
// and password are set. Sending credentials to a server that has auth disabled
// is an error, so an unset username must produce none.
func TestMongoCredentialsOnlyWhenAuthIsEnabled(t *testing.T) {
	args := map[string]any{}
	applyServiceCredentials(args, &ServiceDef{Env: map[string]string{
		"MONGO_INITDB_DATABASE": "app",
	}}, "mongodb")
	if _, ok := args["user"]; ok {
		t.Errorf("no root username declared, but credentials were sent: %v", args)
	}
}

// An explicit step kwarg always wins over the service's env.
func TestStepKwargsOverrideServiceEnv(t *testing.T) {
	args := map[string]any{"user": "explicit", "password": "explicitpw"}
	applyServiceCredentials(args, &ServiceDef{Env: map[string]string{
		"POSTGRES_USER": "fromenv", "POSTGRES_PASSWORD": "envpw",
	}}, "postgres")
	if args["user"] != "explicit" || args["password"] != "explicitpw" {
		t.Errorf("env overrode an explicit kwarg: %v", args)
	}
}
