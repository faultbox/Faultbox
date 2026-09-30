package star

import (
	"strings"
	"testing"
)

func TestParseEphemeralPortRange(t *testing.T) {
	for _, input := range []string{"32768\t60999\n", "32768\n60999\n"} {
		low, high, err := parseEphemeralPortRange(input)
		if err != nil || low != 32768 || high != 60999 {
			t.Fatalf("%q: %d %d %v", input, low, high, err)
		}
	}
	for _, input := range []string{"", "0 65535", "100 99", "1 65536", "1 2 3", "first last"} {
		if _, _, err := parseEphemeralPortRange(input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestFixedPortDiagnosticsExcludeRemoteAndAutomaticPorts(t *testing.T) {
	rt := New(testLogger())
	iface := func(port int) map[string]*InterfaceDef {
		return map[string]*InterfaceDef{"main": {Name: "main", Port: port, HostPort: port}}
	}
	rt.services = map[string]*ServiceDef{
		"binary":    {Name: "binary", Binary: "/bin/true", Interfaces: iface(40000)},
		"safe":      {Name: "safe", Binary: "/bin/true", Interfaces: iface(17000)},
		"remote":    {Name: "remote", Remote: "localhost", Interfaces: iface(40000)},
		"automatic": {Name: "automatic", Image: "redis:7", Interfaces: iface(40000)},
		"explicit":  {Name: "explicit", Image: "redis:7", Interfaces: iface(6379), Ports: map[int]int{6379: 40001}},
		"random":    {Name: "random", Interfaces: iface(0)},
	}
	rt.order = []string{"binary", "safe", "remote", "automatic", "explicit", "random"}
	findings := rt.FixedPortDiagnostics(32768, 60999)
	if len(findings) != 2 {
		t.Fatalf("findings: %+v", findings)
	}
	for i, name := range []string{"binary", "explicit"} {
		if findings[i].Code != "PORT_IN_EPHEMERAL_RANGE" || !strings.Contains(findings[i].Message, name) {
			t.Errorf("finding %d = %+v", i, findings[i])
		}
	}
}
