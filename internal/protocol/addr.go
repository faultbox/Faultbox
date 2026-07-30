package protocol

import (
	"net/url"
	"strings"
)

// Addr is an address a plugin was handed, split into the parts a plugin
// actually needs to dial.
type Addr struct {
	HostPort string // "host:port", always free of any scheme
	User     string
	Password string
	Database string
}

// ParseAddr splits an address that may carry a scheme and credentials.
//
// Plugins receive an address from two places, and until v0.16.1 the two did
// not agree. ExecuteStep gets a bare "host:port". Healthcheck gets whatever
// the spec's healthcheck resolved to — and ready() resolves to
// "<protocol>://host:port", so every plugin whose Healthcheck dialled `addr`
// directly was trying to open a TCP connection to a host literally named
// "redis://localhost". That never resolves, so the check burned its whole
// timeout and the service was reported not ready. It made ready() a guaranteed
// failure for cassandra, clickhouse, grpc, mongodb, mysql, nats, redis and
// udp — every protocol except the three that happened to parse a URL already
// (postgres, http, http2) and the two waitReady special-cases (tcp, kafka).
//
// So every plugin normalises through here, and gets any credentials the URL
// carried for free. A bare "host:port" yields empty credentials, which is what
// callers with none to give have always passed.
func ParseAddr(addr string) Addr {
	i := strings.Index(addr, "://")
	if i <= 0 {
		return Addr{HostPort: addr}
	}
	u, err := url.Parse(addr)
	if err != nil {
		// Malformed past the scheme: strip what we can and let the dial fail
		// with a real connection error rather than a parse error, which is the
		// more useful of the two for someone reading a spec.
		return Addr{HostPort: addr[i+3:]}
	}
	a := Addr{HostPort: u.Host, Database: strings.TrimPrefix(u.Path, "/")}
	if u.User != nil {
		a.User = u.User.Username()
		a.Password, _ = u.User.Password()
	}
	return a
}

// BuildAddr renders the URL form ParseAddr consumes. resolveReadyCheck uses it
// so a spec's declared credentials reach the plugin's own readiness check —
// otherwise "ready" can only ever mean "the port is open", which is the guess
// ready() exists to replace.
func BuildAddr(scheme, hostPort string, a Addr) string {
	u := &url.URL{Scheme: scheme, Host: hostPort}
	if a.Database != "" {
		u.Path = "/" + a.Database
	}
	// A password with no username is not a mistake — it is how Redis's
	// --requirepass works, and how most single-secret servers are configured.
	// Emitting only the user would silently drop the secret, and the plugin
	// would authenticate with nothing.
	switch {
	case a.Password != "":
		u.User = url.UserPassword(a.User, a.Password)
	case a.User != "":
		u.User = url.User(a.User)
	}
	return u.String()
}
