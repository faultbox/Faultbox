// Package connowner attributes TCP connections to explicitly registered managed
// process trees. Source.PID is the registered root's host PID, not necessarily
// the descendant that holds the socket. Instance identifies that registration
// and the root's kernel process start time; it is not merely a service name.
//
// Attribution fails closed: unknown or ambiguous owners return an empty Source.
// Bindings cache proven ownership, while unproven bindings can retry until
// Close. Callers must close bindings when their accepted connections close, and
// check IsActive before using historical ownership to affect a current run.
package connowner

import (
	"crypto/rand"
	"fmt"
	"net"
	"net/netip"
	"sync"
)

type Source struct {
	Service  string
	Instance string
	PID      int
}

func (s Source) Known() bool { return s.Instance != "" }

type endpoint struct {
	addr netip.Addr
	port uint16
}

type tuple struct{ from, to endpoint }

func address(addr net.Addr) (endpoint, bool) {
	if tcp, ok := addr.(*net.TCPAddr); ok {
		ip, ok := netip.AddrFromSlice(tcp.IP)
		if !ok || tcp.Port <= 0 || tcp.Port > 65535 {
			return endpoint{}, false
		}
		// /proc does not encode an IPv6 scope ID. Do not infer ownership
		// where the same link-local address can occur on several interfaces.
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return endpoint{}, false
		}
		return endpoint{ip.Unmap(), uint16(tcp.Port)}, true
	}
	return endpoint{}, false
}

func connectionTuple(from, to net.Addr) (tuple, bool) {
	src, srcOK := address(from)
	dst, dstOK := address(to)
	return tuple{src, dst}, srcOK && dstOK
}

type registration struct {
	source Source
	start  uint64
	token  uint64
}

type Tracker struct {
	mu            sync.Mutex
	identity      string
	next          uint64
	procRoot      string
	registrations map[string]registration
	bridges       map[tuple]map[uint64]*Binding
}

func New() *Tracker { return newTracker("/proc") }

func newTracker(procRoot string) *Tracker {
	return &Tracker{
		identity: rand.Text(), procRoot: procRoot,
		registrations: make(map[string]registration),
		bridges:       make(map[tuple]map[uint64]*Binding),
	}
}

// Register pins the root's /proc start time before its sockets can be used.
// Unregister affects only this registration, even if the PID is later reused.
// Unsupported platforms deliberately accept registration without attribution.
func (t *Tracker) Register(service string, pid int) (func(), error) {
	if !platformSupported {
		return func() {}, nil
	}
	if service == "" || pid <= 0 {
		return nil, fmt.Errorf("connection owner requires a service and positive managed root PID")
	}
	start, err := processStart(t.procRoot, pid)
	if err != nil {
		return nil, fmt.Errorf("register connection owner %q PID %d: %w", service, pid, err)
	}
	t.mu.Lock()
	t.next++
	reg := registration{start: start, token: t.next}
	reg.source = Source{Service: service, PID: pid,
		Instance: fmt.Sprintf("%s/%d/%d/%d", t.identity, pid, start, reg.token)}
	t.registrations[reg.source.Instance] = reg
	t.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			if current, ok := t.registrations[reg.source.Instance]; ok && current.token == reg.token {
				delete(t.registrations, reg.source.Instance)
			}
			t.mu.Unlock()
		})
	}, nil
}

// IsActive verifies both the registration lifetime and the kernel process
// generation. An old Source is never reactivated by a reused PID or service.
func (t *Tracker) IsActive(source Source) bool {
	if !source.Known() || !platformSupported {
		return false
	}
	t.mu.Lock()
	reg, ok := t.registrations[source.Instance]
	t.mu.Unlock()
	if !ok || reg.source != source {
		return false
	}
	start, err := processStart(t.procRoot, source.PID)
	if err != nil || start != reg.start {
		return false
	}
	t.mu.Lock()
	current, ok := t.registrations[source.Instance]
	t.mu.Unlock()
	return ok && current.token == reg.token
}

type Binding struct {
	mu     sync.Mutex
	owner  *Tracker
	tuple  tuple
	valid  bool
	closed bool
	source Source
}

// Bind uses the tuple seen by a server at Accept. Resolution is lazy so a
// managed process can be registered between Accept and its first request.
func (t *Tracker) Bind(peer, local net.Addr) *Binding {
	key, valid := connectionTuple(peer, local)
	return &Binding{owner: t, tuple: key, valid: valid}
}

func (b *Binding) Source() Source { return b.resolve(make(map[*Binding]bool)) }

func (b *Binding) resolve(seen map[*Binding]bool) Source {
	b.mu.Lock()
	if b.source.Known() || b.closed || !b.valid {
		source := b.source
		b.mu.Unlock()
		return source
	}
	b.mu.Unlock()
	if seen[b] {
		return Source{}
	}
	seen[b] = true
	defer delete(seen, b)
	source := b.owner.resolve(b.tuple, seen)
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed && !b.source.Known() && source.Known() {
		b.source = source
	}
	return b.source
}

func (b *Binding) Close() {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
}

func (t *Tracker) resolve(key tuple, seen map[*Binding]bool) Source {
	t.mu.Lock()
	bridges := t.bridges[key]
	var bridge *Binding
	for _, entry := range bridges {
		bridge = entry
	}
	bridgeCount := len(bridges)
	regs := make([]registration, 0, len(t.registrations))
	for _, reg := range t.registrations {
		regs = append(regs, reg)
	}
	t.mu.Unlock()
	if bridgeCount > 1 {
		return Source{}
	}
	if bridgeCount == 1 {
		return bridge.resolve(seen)
	}
	var found Source
	for _, reg := range regs {
		owned, complete := ownsConnection(t.procRoot, reg, key)
		t.mu.Lock()
		current, stillRegistered := t.registrations[reg.source.Instance]
		t.mu.Unlock()
		if !stillRegistered || current.token != reg.token {
			continue
		}
		if !complete {
			return Source{}
		}
		if !t.IsActive(reg.source) {
			continue
		}
		if owned {
			if found.Known() && found != reg.source {
				return Source{}
			}
			found = reg.source
		}
	}
	return found
}

// Forward preserves the original accepted connection's ownership across a
// transparent TCP proxy. Install it immediately after dialing upstream, before
// writing requests. The cleanup function is idempotent and cannot remove a
// newer bridge for a reused tuple. Concurrent conflicting bridges are unknown.
func (t *Tracker) Forward(client, upstream net.Conn) func() {
	if client == nil || upstream == nil {
		return func() {}
	}
	key, valid := connectionTuple(upstream.LocalAddr(), upstream.RemoteAddr())
	if !valid {
		return func() {}
	}
	binding := t.Bind(client.RemoteAddr(), client.LocalAddr())
	t.mu.Lock()
	t.next++
	token := t.next
	if t.bridges[key] == nil {
		t.bridges[key] = make(map[uint64]*Binding)
	}
	t.bridges[key][token] = binding
	t.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			binding.Close()
			t.mu.Lock()
			delete(t.bridges[key], token)
			if len(t.bridges[key]) == 0 {
				delete(t.bridges, key)
			}
			t.mu.Unlock()
		})
	}
}
