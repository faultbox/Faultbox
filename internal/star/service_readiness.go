package star

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"syscall"

	"github.com/faultbox/Faultbox/internal/protocol"
	"github.com/faultbox/Faultbox/internal/proxy"
)

// containerProtocolCheck upgrades a container's own TCP probe to the protocol
// declared for that port. Docker can accept TCP before the server is running.
// Checks of unrelated endpoints retain their explicitly requested semantics.
func (rt *Runtime) containerProtocolCheck(svc *ServiceDef, check string) (string, bool) {
	if !svc.IsContainer() {
		return check, false
	}
	u, err := url.Parse(check)
	if err != nil || u.Scheme != "tcp" || u.User != nil || u.RawQuery != "" || u.Path != "" || u.Fragment != "" {
		return check, false
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return check, false
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return check, false
	}
	var match *InterfaceDef
	for _, iface := range svc.Interfaces {
		if iface.Port == port {
			if match != nil { // Ambiguous declarations must not depend on map order.
				return check, false
			}
			match = iface
		}
	}
	if match == nil || match.Protocol == "tcp" {
		return check, false
	}
	if _, ok := protocol.Get(match.Protocol); !ok {
		return check, false
	}
	// Reuse ready()'s protocol and credential handling for this exact interface,
	// including services where it is not the default interface.
	copy := *svc
	copy.Interfaces = map[string]*InterfaceDef{match.Name: match}
	resolved := rt.resolveReadyCheck(&copy)
	parsed, err := url.Parse(resolved)
	if err != nil {
		return check, false
	}
	mappedPort := match.Port
	if match.HostPort > 0 {
		mappedPort = match.HostPort
	}
	parsed.Host = net.JoinHostPort(host, strconv.Itoa(mappedPort))
	return parsed.String(), true
}

// readinessCheckForStart explains a TCP-to-protocol upgrade without putting
// credentials from the resolved URL in the trace.
func (rt *Runtime) readinessCheckForStart(svc *ServiceDef) string {
	if svc.Healthcheck == nil {
		return ""
	}
	if check, upgraded := rt.containerProtocolCheck(svc, svc.Healthcheck.Test); upgraded {
		u, _ := url.Parse(check)
		rt.events.Emit("service_readiness_upgraded", svc.Name, map[string]string{
			"from": "tcp", "protocol": u.Scheme, "address": u.Host,
			"reason": "Docker may accept TCP before the service can answer protocol requests",
		})
		return check
	}
	return rt.resolveHealthcheck(svc)
}

const (
	CodePortInUse                Code = "PORT_IN_USE"
	CodeServiceExitedBeforeReady Code = "SERVICE_EXITED_BEFORE_READY"
)

func init() {
	suggestions[CodePortInUse] = "A declared fixed host port is already bound. Stop the conflicting listener or choose an available fixed port outside the host's ephemeral range; no service restart is attempted."
	suggestions[CodeServiceExitedBeforeReady] = "The service exited during startup. Inspect its exit code and startup logs; a readiness timeout or automatic startup retry would hide the original failure."
}

// startupFailure preserves a real bind error when available; other exits must
// not be guessed to be port conflicts from their message or exit status.
func startupFailure(service string, exitCode int, cause error) error {
	if errors.Is(cause, syscall.EADDRINUSE) {
		return codedf(CodePortInUse, "service %q could not bind its listener: %w", service, cause)
	}
	if cause != nil {
		return codedf(CodeServiceExitedBeforeReady, "service %q exited before becoming ready (exit code %d): %w", service, exitCode, cause)
	}
	return codedf(CodeServiceExitedBeforeReady, "service %q exited before becoming ready (exit code %d)", service, exitCode)
}

// checkFixedServicePorts detects an already-bound port before a process is
// started, so its healthcheck cannot accidentally succeed against an old
// listener. This is best effort, not a reservation or a startup retry. Binary
// bind addresses are unspecified, so only their normal loopback endpoints are
// probed; the actual process exit remains authoritative.
func checkFixedServicePorts(svc *ServiceDef) error {
	bindings := []struct{ network, host string }{{"tcp4", "127.0.0.1"}, {"tcp6", "::1"}}
	if svc.IsMock() {
		bindings = []struct{ network, host string }{{"tcp", proxy.BindHost()}}
	} else if svc.IsContainer() {
		// Keep consistent with container.Client.CreateContainer's HostIP.
		bindings = []struct{ network, host string }{{"tcp4", "0.0.0.0"}}
	}
	for _, port := range fixedHostPorts(svc) {
		transports := map[string]bool{}
		for _, iface := range svc.Interfaces {
			declared := iface.Port
			if svc.IsContainer() {
				declared = svc.Ports[iface.Port]
			}
			if declared != port {
				continue
			}
			transport := "tcp"
			if iface.Protocol == "udp" && !svc.IsContainer() {
				transport = "udp"
			}
			transports[transport] = true
		}
		for _, transport := range []string{"tcp", "udp"} {
			if !transports[transport] {
				continue
			}
			for _, binding := range bindings {
				var listener interface{ Close() error }
				var err error
				addr := net.JoinHostPort(binding.host, strconv.Itoa(port))
				if transport == "udp" {
					listener, err = net.ListenPacket("udp"+binding.network[3:], addr)
				} else {
					listener, err = net.Listen(binding.network, addr)
				}
				if err == nil {
					listener.Close()
					continue
				}
				if binding.network == "tcp6" && (errors.Is(err, syscall.EAFNOSUPPORT) || errors.Is(err, syscall.EADDRNOTAVAIL)) {
					continue
				}
				if errors.Is(err, syscall.EADDRINUSE) {
					return codedf(CodePortInUse, "service %q fixed host port %d is already in use: %w", svc.Name, port, err)
				}
				return fmt.Errorf("check service %q fixed host port %d: %w", svc.Name, port, err)
			}
		}
	}
	return nil
}
