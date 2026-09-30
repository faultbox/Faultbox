package star

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/faultbox/Faultbox/internal/protocol"
	"github.com/faultbox/Faultbox/internal/proxy"
)

func TestContainerTCPReadinessUsesMatchedProtocolAndCredentials(t *testing.T) {
	rt := New(testLogger())
	svc := &ServiceDef{
		Name: "db", Image: "mysql:8", Env: map[string]string{"MYSQL_ROOT_PASSWORD": "secret", "MYSQL_DATABASE": "app"},
		Interfaces: map[string]*InterfaceDef{
			"metrics": {Name: "metrics", Protocol: "http", Port: 9000, HostPort: 49000},
			"sql":     {Name: "sql", Protocol: "mysql", Port: 3306, HostPort: 43306},
		},
		Healthcheck: &HealthcheckDef{Test: "tcp://127.0.0.1:3306"},
	}
	check := rt.readinessCheckForStart(svc)
	addr := protocol.ParseAddr(check)
	if !strings.HasPrefix(check, "mysql://") || addr.HostPort != "127.0.0.1:43306" || addr.Password != "secret" || addr.Database != "app" {
		t.Fatalf("check = %q, parsed = %+v", check, addr)
	}
	events := rt.events.Events()
	if len(events) != 1 || events[0].Type != "service_readiness_upgraded" || events[0].Service != "db" {
		t.Fatalf("upgrade explanation missing: %+v", events)
	}
	if strings.Contains(fmt.Sprint(events[0].Fields), "secret") {
		t.Fatal("readiness event disclosed credentials")
	}
}

func TestContainerTCPReadinessDoesNotChangeUnrelatedChecks(t *testing.T) {
	rt := New(testLogger())
	svc := &ServiceDef{Name: "cache", Image: "redis:7", Interfaces: map[string]*InterfaceDef{
		"main": {Name: "main", Protocol: "redis", Port: 6379, HostPort: 46379},
	}}
	for _, check := range []string{"tcp://other-host:6379", "tcp://127.0.0.1:6380", "tcp://127.0.0.1:6379/path", "http://localhost:6379/health", readyScheme} {
		if got, upgraded := rt.containerProtocolCheck(svc, check); upgraded || got != check {
			t.Errorf("unrelated check %q changed to %q (%v)", check, got, upgraded)
		}
	}
	svc.Image = ""
	if _, upgraded := rt.containerProtocolCheck(svc, "tcp://localhost:6379"); upgraded {
		t.Fatal("binary TCP probe was changed")
	}
}

func TestContainerReadinessWaitsForRedisProtocolAfterTCPAccept(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var ready atomic.Bool
	pingSeen := make(chan struct{}, 1)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.SetDeadline(time.Now().Add(time.Second))
			reader := bufio.NewReader(conn)
			for i := 0; i < 3; i++ {
				_, _ = reader.ReadString('\n')
			}
			select {
			case pingSeen <- struct{}{}:
			default:
			}
			if ready.Load() {
				_, _ = fmt.Fprint(conn, "+PONG\r\n")
			}
			conn.Close()
		}
	}()
	rt := New(testLogger())
	svc := &ServiceDef{Name: "cache", Image: "redis:7", Interfaces: map[string]*InterfaceDef{
		"main": {Name: "main", Protocol: "redis", Port: 6379, HostPort: listener.Addr().(*net.TCPAddr).Port},
	}, Healthcheck: &HealthcheckDef{Test: "tcp://127.0.0.1:6379"}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- waitReady(ctx, rt.readinessCheckForStart(svc), 3*time.Second) }()
	select {
	case <-pingSeen:
	case err := <-result:
		t.Fatalf("probe returned before PING: %v", err)
	case <-ctx.Done():
		t.Fatal("protocol probe never arrived")
	}
	select {
	case err := <-result:
		t.Fatalf("port accepted TCP but protocol not ready: %v", err)
	default:
	}
	ready.Store(true)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	listener.Close()
	<-serverDone
}

func TestStartupPortConflictIsDistinctFromProcessExit(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	svc := &ServiceDef{Name: "sut", Binary: "/bin/true", Interfaces: map[string]*InterfaceDef{"main": {Port: port}}}
	err = checkFixedServicePorts(svc)
	if d := DiagnosticFor(err); d == nil || d.Code != string(CodePortInUse) {
		t.Fatalf("port collision = %v", err)
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("lost bind cause: %v", err)
	}
	listener.Close()
	if err := checkFixedServicePorts(svc); err != nil {
		t.Fatal(err)
	}
	for _, cause := range []error{nil, errors.New("crashed")} {
		err := startupFailure("sut", 42, cause)
		if d := DiagnosticFor(err); d == nil || d.Code != string(CodeServiceExitedBeforeReady) || !strings.Contains(d.Message, "42") {
			t.Fatalf("exit = %v", err)
		}
	}
	err = startupFailure("sut", 1, fmt.Errorf("listen: %w", syscall.EADDRINUSE))
	if d := DiagnosticFor(err); d == nil || d.Code != string(CodePortInUse) {
		t.Fatalf("bind exit = %v", err)
	}
}

func TestFixedPortPreflightUsesTransportAndMockBindAddress(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	svc := &ServiceDef{Name: "udp", Binary: "/bin/true", Interfaces: map[string]*InterfaceDef{"main": {Protocol: "udp", Port: port}}}
	if err := checkFixedServicePorts(svc); err != nil {
		t.Fatalf("UDP preflight mistook TCP listener for a collision: %v", err)
	}
	packet, err := net.ListenPacket("udp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer packet.Close()
	if d := DiagnosticFor(checkFixedServicePorts(svc)); d == nil || d.Code != string(CodePortInUse) {
		t.Fatalf("UDP collision not detected: %+v", d)
	}
	svc.Mock = &MockConfig{}
	svc.Interfaces["main"].Protocol = "tcp"
	t.Setenv(proxy.FaultboxProxyBindEnv, "127.0.0.1")
	if d := DiagnosticFor(checkFixedServicePorts(svc)); d == nil || d.Code != string(CodePortInUse) {
		t.Fatalf("mock collision not detected: %+v", d)
	}
}
