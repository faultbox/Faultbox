package netfault

import (
	"runtime"
	"strings"
	"testing"
)

func TestAllocatorAssignsOneAddressPerTriple(t *testing.T) {
	g, err := NewGateway(GatewayConfig{})
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}

	a, err := g.Allocate("api", "db", "main", 5432, "127.0.0.1:5432")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	b, err := g.Allocate("worker", "db", "main", 5432, "127.0.0.1:5432")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}

	// Different consumers of the SAME interface must get different addresses.
	// That is what makes fault(db.main, source=worker) resolvable without the
	// packet's source IP, which Docker masquerades away.
	if a.IP == b.IP {
		t.Errorf("api and worker share address %s; source= targeting is unresolvable", a.IP)
	}
	// Port mirrors the upstream so only the host changes.
	if a.Port != 5432 || b.Port != 5432 {
		t.Errorf("ports = %d/%d, want both 5432", a.Port, b.Port)
	}
}

func TestAllocatorIsStableAndIdempotent(t *testing.T) {
	g, _ := NewGateway(GatewayConfig{})
	first, _ := g.Allocate("api", "db", "main", 5432, "u")
	again, _ := g.Allocate("api", "db", "main", 5432, "u")
	if first.IP != again.IP {
		t.Errorf("re-allocating the same triple moved the address: %s -> %s", first.IP, again.IP)
	}

	// Same triples, same order, different gateway → same assignment. The
	// address reaches the SUT's env and therefore the event log and the .fb
	// bundle; a bundle that replays with different addresses is confusing.
	g2, _ := NewGateway(GatewayConfig{})
	x, _ := g2.Allocate("api", "db", "main", 5432, "u")
	y, _ := g2.Allocate("worker", "db", "main", 5432, "u")
	g3, _ := NewGateway(GatewayConfig{})
	x3, _ := g3.Allocate("api", "db", "main", 5432, "u")
	y3, _ := g3.Allocate("worker", "db", "main", 5432, "u")
	if x.IP != x3.IP || y.IP != y3.IP {
		t.Errorf("allocation is not deterministic: (%s,%s) vs (%s,%s)", x.IP, y.IP, x3.IP, y3.IP)
	}
}

func TestAllocatorAvoidsHostAddress(t *testing.T) {
	g, _ := NewGateway(GatewayConfig{})
	host := g.HostIP()
	for i := 0; i < 20; i++ {
		ga, err := g.Allocate("c", "s", string(rune('a'+i)), 80, "u")
		if err != nil {
			t.Fatalf("allocate: %v", err)
		}
		if ga.IP == host {
			t.Fatalf("allocated the host TUN address %s to a service", host)
		}
	}
}

func TestAllocatorLookupAndForIP(t *testing.T) {
	g, _ := NewGateway(GatewayConfig{})
	ga, _ := g.Allocate("api", "db", "main", 5432, "127.0.0.1:5432")

	got, ok := g.Lookup("api", "db", "main")
	if !ok || got.IP != ga.IP {
		t.Errorf("Lookup returned (%v, %v), want %s", got, ok, ga.IP)
	}
	if _, ok := g.Lookup("nobody", "db", "main"); ok {
		t.Error("Lookup found an unallocated triple")
	}

	back, ok := g.alloc.forIP(ga.IP)
	if !ok || back.Service != "db" || back.Interface != "main" || back.Consumer != "api" {
		t.Errorf("forIP(%s) = %+v, want the api/db/main triple", ga.IP, back)
	}
}

func TestAllocatorRejectsBadSubnet(t *testing.T) {
	for _, cidr := range []string{"not-a-cidr", "10.99.0.0/30", "::1/64"} {
		if _, err := NewGateway(GatewayConfig{Subnet: cidr}); err == nil {
			t.Errorf("subnet %q was accepted", cidr)
		}
	}
}

func TestAllocatorExhaustionIsAnError(t *testing.T) {
	g, err := NewGateway(GatewayConfig{Subnet: "10.99.0.0/24"})
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	var lastErr error
	for i := 0; i < 300; i++ {
		if _, lastErr = g.Allocate("c", "s", string(rune(i)), 80, "u"); lastErr != nil {
			break
		}
	}
	if lastErr == nil {
		t.Fatal("a /24 never exhausted; the bound is not enforced")
	}
	if !strings.Contains(lastErr.Error(), "exhausted") {
		t.Errorf("error = %v, want it to mention exhaustion", lastErr)
	}
}

func TestGatewayDefaults(t *testing.T) {
	g, _ := NewGateway(GatewayConfig{})
	if g.Device() != "faultbox0" {
		t.Errorf("Device = %q, want faultbox0", g.Device())
	}
	if g.Subnet() != DefaultSubnet {
		t.Errorf("Subnet = %q, want %q", g.Subnet(), DefaultSubnet)
	}
	if g.HostIP() != "10.99.0.1" {
		t.Errorf("HostIP = %q, want 10.99.0.1", g.HostIP())
	}
}

// TestRulePublishOrderIsDeterministic: rule precedence is first-match-wins, so
// it must not depend on Go map iteration order.
func TestRulePublishOrderIsDeterministic(t *testing.T) {
	build := func() []*Rule {
		g, _ := NewGateway(GatewayConfig{})
		lower := &recordEndpoint{}
		ep := New(lower, Options{})
		ep.Attach(&recordDispatcher{})
		g.endpoint = ep
		defer ep.Close()

		_ = g.InstallRules("zeta", "main", []*Rule{{Action: ActionDrop, Label: "z"}})
		_ = g.InstallRules("alpha", "main", []*Rule{{Action: ActionDelay, Delay: 1, Label: "a"}})
		_ = g.InstallRules("mid", "main", []*Rule{{Action: ActionPass, Label: "m"}})
		return ep.Rules().Rules()
	}
	first := build()
	for i := 0; i < 20; i++ {
		got := build()
		if len(got) != len(first) {
			t.Fatalf("rule count varies: %d vs %d", len(got), len(first))
		}
		for j := range got {
			if got[j].Label != first[j].Label {
				t.Fatalf("rule order varies between runs: %v vs %v", labels(got), labels(first))
			}
		}
	}
}

func labels(rs []*Rule) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Label
	}
	return out
}

func TestClearRulesRemovesOnlyThatInterface(t *testing.T) {
	g, _ := NewGateway(GatewayConfig{})
	lower := &recordEndpoint{}
	ep := New(lower, Options{})
	ep.Attach(&recordDispatcher{})
	defer ep.Close()
	g.endpoint = ep

	_ = g.InstallRules("db", "main", []*Rule{{Action: ActionDrop, Label: "db"}})
	_ = g.InstallRules("cache", "main", []*Rule{{Action: ActionDrop, Label: "cache"}})
	if got := len(ep.Rules().Rules()); got != 2 {
		t.Fatalf("installed %d rules, want 2", got)
	}

	_ = g.ClearRules("db", "main")
	rules := ep.Rules().Rules()
	if len(rules) != 1 || rules[0].Label != "cache" {
		t.Errorf("after clearing db.main, rules = %v, want [cache]", labels(rules))
	}

	_ = g.ClearRules("cache", "main")
	if rs := ep.Rules(); rs != nil && len(rs.Rules()) != 0 {
		t.Errorf("rules remain after clearing everything: %v", labels(rs.Rules()))
	}
}

// TestPreflightOnNonLinuxNamesTheFix: on a host without a TUN device the error
// must say what to do, not merely that something failed.
func TestPreflightMessageIsActionable(t *testing.T) {
	g, _ := NewGateway(GatewayConfig{})
	err := g.Preflight()
	if runtime.GOOS != "linux" {
		if err == nil {
			t.Fatal("preflight passed on a non-Linux host")
		}
		for _, want := range []string{"Linux", runtime.GOOS} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should mention %q, got: %v", want, err)
			}
		}
		return
	}
	// On Linux the result depends on privileges; either way it must be
	// actionable rather than a bare errno.
	if err != nil {
		for _, want := range []string{"tun", "TUN", "CAP_NET_ADMIN", "ip_forward"} {
			if strings.Contains(err.Error(), want) {
				return
			}
		}
		t.Errorf("preflight failure is not actionable: %v", err)
	}
}

func TestStartFailsClosedOnUnsupportedPlatform(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("this asserts the non-Linux stub; the Linux path is exercised in the Lima VM (M4 acceptance)")
	}
	g, _ := NewGateway(GatewayConfig{})
	if err := g.Start(); err == nil {
		t.Fatal("Start succeeded without a TUN device")
	}
	if _, err := g.Listen(&GatewayAddr{IP: "10.99.0.2", Port: 80}); err == nil {
		t.Fatal("Listen succeeded without a started gateway")
	}
	if err := g.Close(); err != nil {
		t.Errorf("Close on an unstarted gateway: %v", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	g, _ := NewGateway(GatewayConfig{})
	if err := g.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := g.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if err := g.Start(); err == nil {
		t.Error("Start succeeded after Close")
	}
}
