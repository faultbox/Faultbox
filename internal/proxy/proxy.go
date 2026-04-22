// Package proxy provides protocol-level fault injection via transparent proxies.
// Each proxy intercepts traffic between two services, speaks the real protocol,
// and can inject faults (errors, delays, drops) based on request matching rules.
//
// Proxies are started by the runtime when fault(interface_ref, ...) is used.
// Services connect to the proxy instead of the real target — env wiring is
// rewritten transparently.
package proxy

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Proxy intercepts traffic between two services at the protocol level.
type Proxy interface {
	// Protocol returns the protocol name this proxy handles.
	Protocol() string

	// Start begins listening on a random port. Returns the listen address.
	Start(ctx context.Context, target string) (listenAddr string, err error)

	// AddRule adds a fault injection rule.
	AddRule(rule Rule)

	// ClearRules removes all fault rules.
	ClearRules()

	// Stop shuts down the proxy.
	Stop() error
}

// Rule describes a protocol-level fault to inject.
type Rule struct {
	// Match criteria (protocol-specific, all optional, glob patterns):
	Method  string // HTTP method, gRPC method, Redis command
	Path    string // HTTP path pattern
	Query   string // SQL query pattern (Postgres/MySQL)
	Key     string // Redis key pattern
	Topic   string // Kafka/NATS topic/subject
	Command string // Redis command name

	// Action:
	Action Action

	// Action parameters:
	Status int           // HTTP status code or gRPC status
	Body   string        // Response body (JSON)
	Error  string        // Error message
	Delay  time.Duration // Delay before action
	Prob   float64       // Probability [0,1], 0 = always
}

// Action determines what the proxy does when a rule matches.
type Action int

const (
	ActionRespond  Action = iota // Return custom response (don't forward)
	ActionError                  // Return protocol-specific error
	ActionDelay                  // Delay then forward normally
	ActionDrop                   // Close connection / drop message
	ActionDuplicate              // Forward twice (for message brokers)
)

// MatchRequest checks if a rule matches a request described by the given fields.
func (r *Rule) MatchRequest(method, path, query, key, topic, command string) bool {
	if r.Method != "" && !matchGlob(method, r.Method) {
		return false
	}
	if r.Path != "" && !matchGlob(path, r.Path) {
		return false
	}
	if r.Query != "" && !matchGlob(query, r.Query) {
		return false
	}
	if r.Key != "" && !matchGlob(key, r.Key) {
		return false
	}
	if r.Topic != "" && !matchGlob(topic, r.Topic) {
		return false
	}
	if r.Command != "" && !matchGlob(command, r.Command) {
		return false
	}
	return true
}

// matchGlob checks if actual matches pattern. Supports *, prefix*, *suffix,
// and filepath.Match globs.
func matchGlob(actual, pattern string) bool {
	if pattern == "" {
		return true
	}
	if strings.Contains(pattern, "*") {
		if matched, err := filepath.Match(pattern, actual); err == nil && matched {
			return true
		}
		// Fallback for prefix/suffix.
		if strings.HasSuffix(pattern, "*") {
			return strings.HasPrefix(actual, strings.TrimSuffix(pattern, "*"))
		}
		if strings.HasPrefix(pattern, "*") {
			return strings.HasSuffix(actual, strings.TrimPrefix(pattern, "*"))
		}
		return false
	}
	return strings.EqualFold(actual, pattern)
}

// OnProxyEvent is called when the proxy intercepts and acts on a request.
type OnProxyEvent func(evt ProxyEvent)

// ProxyEvent describes a proxy interception for trace emission.
type ProxyEvent struct {
	Protocol string
	Action   string // "error", "delay", "drop", "respond", "forward"
	From     string // source service
	To       string // target service
	Fields   map[string]string // protocol-specific: method, path, query, etc.
}

// Manager manages proxy lifecycle per service interface.
type Manager struct {
	mu      sync.Mutex
	proxies map[string]*runningProxy // key: "serviceName:interfaceName"
	onEvent OnProxyEvent
}

type runningProxy struct {
	proxy      Proxy
	listenAddr string
	target     string
	cancel     context.CancelFunc
}

// NewManager creates a proxy manager.
func NewManager(onEvent OnProxyEvent) *Manager {
	return &Manager{
		proxies: make(map[string]*runningProxy),
		onEvent: onEvent,
	}
}

// EnsureProxy starts a proxy for the given service interface if not already
// running. Returns the proxy's listen address (to rewrite in env).
func (m *Manager) EnsureProxy(ctx context.Context, svcName, ifaceName, protocol, targetAddr string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := svcName + ":" + ifaceName
	if rp, ok := m.proxies[key]; ok {
		return rp.listenAddr, nil // already running
	}

	p, err := newProxy(protocol, m.onEvent, svcName)
	if err != nil {
		return "", fmt.Errorf("create %s proxy for %s: %w", protocol, key, err)
	}

	pCtx, cancel := context.WithCancel(ctx)
	listenAddr, err := p.Start(pCtx, targetAddr)
	if err != nil {
		cancel()
		return "", fmt.Errorf("start %s proxy for %s: %w", protocol, key, err)
	}

	m.proxies[key] = &runningProxy{
		proxy:      p,
		listenAddr: listenAddr,
		target:     targetAddr,
		cancel:     cancel,
	}

	return listenAddr, nil
}

// AddRule adds a fault rule to the proxy for the given service interface.
func (m *Manager) AddRule(svcName, ifaceName string, rule Rule) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := svcName + ":" + ifaceName
	rp, ok := m.proxies[key]
	if !ok {
		return fmt.Errorf("no proxy running for %s", key)
	}
	rp.proxy.AddRule(rule)
	return nil
}

// ClearRules removes all rules from the proxy for the given service interface.
func (m *Manager) ClearRules(svcName, ifaceName string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := svcName + ":" + ifaceName
	if rp, ok := m.proxies[key]; ok {
		rp.proxy.ClearRules()
	}
}

// RegisterListenAddr is a test-only helper that records a proxy listen
// address without starting a real protocol proxy. Production code must go
// through EnsureProxy so the full lifecycle (Start, rule dispatch, Stop)
// is exercised; this shortcut exists for unit tests that want to verify
// buildEnv / GetProxyAddr plumbing without standing up a backend.
func (m *Manager) RegisterListenAddr(svcName, ifaceName, listenAddr string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := svcName + ":" + ifaceName
	m.proxies[key] = &runningProxy{
		proxy:      nil,
		listenAddr: listenAddr,
		target:     "",
		cancel:     func() {},
	}
}

// GetProxyAddr returns the proxy listen address for a service interface,
// or empty string if no proxy is running.
func (m *Manager) GetProxyAddr(svcName, ifaceName string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := svcName + ":" + ifaceName
	if rp, ok := m.proxies[key]; ok {
		return rp.listenAddr
	}
	return ""
}

// StopAll shuts down all running proxies.
func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, rp := range m.proxies {
		rp.cancel()
		if rp.proxy != nil {
			rp.proxy.Stop()
		}
		delete(m.proxies, key)
	}
}

// SupportsProxy reports whether a protocol has a proxy implementation that
// can be started in pass-through mode (no rules installed). Callers that
// want to pre-start proxies for every interface (RFC-024 data-path mode)
// use this to decide which interfaces to skip — tcp has no proxy today,
// so attempting to pre-start one would error at launch. Keep this list in
// sync with newProxy() below.
func SupportsProxy(protocol string) bool {
	switch protocol {
	case "http", "http2", "redis", "postgres", "mysql", "grpc",
		"kafka", "mongodb", "amqp", "nats", "memcached", "udp",
		"cassandra", "clickhouse":
		return true
	}
	return false
}

// newProxy creates a protocol-specific proxy instance.
func newProxy(protocol string, onEvent OnProxyEvent, svcName string) (Proxy, error) {
	switch protocol {
	case "http":
		return newHTTPProxy(onEvent, svcName), nil
	case "http2":
		return newHTTP2Proxy(onEvent, svcName), nil
	case "redis":
		return newRedisProxy(onEvent, svcName), nil
	case "postgres":
		return newPostgresProxy(onEvent, svcName), nil
	case "mysql":
		return newMySQLProxy(onEvent, svcName), nil
	case "grpc":
		return newGRPCProxy(onEvent, svcName), nil
	case "kafka":
		return newKafkaProxy(onEvent, svcName), nil
	case "mongodb":
		return newMongoDBProxy(onEvent, svcName), nil
	case "amqp":
		return newAMQPProxy(onEvent, svcName), nil
	case "nats":
		return newNATSProxy(onEvent, svcName), nil
	case "memcached":
		return newMemcachedProxy(onEvent, svcName), nil
	case "udp":
		return newUDPProxy(onEvent, svcName), nil
	case "cassandra":
		return newCassandraProxy(onEvent, svcName), nil
	case "clickhouse":
		return newClickhouseProxy(onEvent, svcName), nil
	default:
		return nil, fmt.Errorf("protocol %q does not support proxy-level faults", protocol)
	}
}
