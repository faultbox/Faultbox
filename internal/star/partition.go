package star

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"go.starlark.net/starlark"
)

// partition() on the packet gateway (v0.14.0, RFC-054 mesh amendment).
//
// The previous implementation denied `connect()` filtered by destination.
// That only blocks connection *setup*, and every consensus protocol worth
// testing pools long-lived connections: once the cluster forms, peers stop
// calling connect entirely. Measured on a 3-node hashicorp/raft cluster,
// `partition_applied` fired while **zero** connect syscalls were intercepted,
// and the leader went on committing as if nothing had happened.
//
// A packet drop is what a partition actually is. From netfault/rule.go:
//
//	No RST is sent — the packet simply never existed.
//
// That is the difference between a partition and a refusal: a refusal is
// information the peer acts on, a partition is silence it has to time out.

// partitionDirection selects which legs of a peer pair are cut.
type partitionDirection string

const (
	partitionBoth partitionDirection = "both"
	partitionAtoB partitionDirection = "a_to_b"
	partitionBtoA partitionDirection = "b_to_a"
)

func parsePartitionDirection(s string) (partitionDirection, error) {
	switch partitionDirection(s) {
	case partitionBoth, partitionAtoB, partitionBtoA:
		return partitionDirection(s), nil
	}
	return "", fmt.Errorf(
		"direction must be \"both\", \"a_to_b\" or \"b_to_a\", got %q", s)
}

// activePartition is one installed partition, tracked so partition_stop can
// undo exactly what partition_start did.
type activePartition struct {
	a, b      string
	direction partitionDirection
	cleanups  []func()
}

type partitionRegistry struct {
	mu     sync.Mutex
	active map[string]*activePartition
}

func newPartitionRegistry() *partitionRegistry {
	return &partitionRegistry{active: make(map[string]*activePartition)}
}

// pairKey is order-independent so partition_stop(b, a) undoes
// partition_start(a, b).
func pairKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "\x00" + b
}

// legs returns the (consumer, target) pairs a partition should cut.
func (d partitionDirection) legs(a, b string) [][2]string {
	switch d {
	case partitionAtoB:
		return [][2]string{{a, b}}
	case partitionBtoA:
		return [][2]string{{b, a}}
	default:
		return [][2]string{{a, b}, {b, a}}
	}
}

// installPartition drops every packet on the selected legs.
//
// Each leg is one (consumer, target-service, interface) triple, which is
// exactly the scope the gateway's per-triple addressing already provides — so
// a one-way partition needs no packet inspection, just rules on one side.
func (rt *Runtime) installPartition(thread *starlark.Thread, a, b string, dir partitionDirection, label string) (*activePartition, error) {
	if err := rt.requirePartitionRuntime(); err != nil {
		return nil, err
	}
	rt.mu.Lock()
	svcA, okA := rt.services[a]
	svcB, okB := rt.services[b]
	rt.mu.Unlock()
	if !okA {
		return nil, fmt.Errorf("partition(): no such service %q", a)
	}
	if !okB {
		return nil, fmt.Errorf("partition(): no such service %q", b)
	}
	if a == b {
		return nil, fmt.Errorf("partition(): cannot partition %q from itself", a)
	}

	ap := &activePartition{a: a, b: b, direction: dir}
	byName := map[string]*ServiceDef{a: svcA, b: svcB}

	var installed int
	for _, leg := range dir.legs(a, b) {
		consumer, target := leg[0], leg[1]
		targetSvc := byName[target]
		if targetSvc == nil {
			continue
		}
		for _, ifaceName := range sortedInterfaceNames(targetSvc) {
			def := &PacketFaultDef{
				Action: "drop",
				Label:  label,
			}
			cleanup, err := rt.applyPacketFaults(thread, consumer, target, ifaceName, []*PacketFaultDef{def})
			if err != nil {
				// Undo whatever already went in — a half-applied partition is
				// worse than none, because the spec would believe it holds.
				for _, c := range ap.cleanups {
					c()
				}
				return nil, fmt.Errorf("partition(%s, %s): %w", a, b, err)
			}
			ap.cleanups = append(ap.cleanups, cleanup)
			installed++
		}
	}
	if installed == 0 {
		return nil, fmt.Errorf("partition(%s, %s): neither service declares an interface to cut", a, b)
	}

	rt.events.Emit("partition_applied", a, map[string]string{
		"peer":      b,
		"direction": string(dir),
		"legs":      fmt.Sprintf("%d", installed),
	})
	return ap, nil
}

func (ap *activePartition) remove(rt *Runtime) {
	for _, c := range ap.cleanups {
		c()
	}
	rt.events.Emit("partition_removed", ap.a, map[string]string{
		"peer":      ap.b,
		"direction": string(ap.direction),
	})
}

// requirePartitionRuntime refuses to fall back to the old connect-deny.
//
// Against any service that pools connections — which is most real
// infrastructure — connect-deny silently does nothing. A primitive that
// quietly no-ops is worse than one that refuses, because the test still
// passes.
func (rt *Runtime) requirePartitionRuntime() error {
	rt.mu.Lock()
	runtimeName := rt.detRuntime
	rt.mu.Unlock()
	if runtimeSupportsPacketFaults(runtimeName) {
		return nil
	}
	return fmt.Errorf(
		"partition() needs the packet gateway, but this spec runs on runtime=%q; "+
			"add determinism(runtime=%q) at the top of the spec.\n"+
			"It is not silently downgraded to a connect() deny: that only blocks connection "+
			"*setup*, so against any service that pools connections (Raft, etcd, Cassandra, "+
			"every gossip protocol) it does nothing at all while the test still passes",
		runtimeName, DeterminismRuntimeGVisor)
}

// parsePartitionArgs is shared by partition() and partition_start().
func parsePartitionArgs(builtin string, args starlark.Tuple, kwargs []starlark.Tuple) (a, b string, dir partitionDirection, run starlark.Callable, err error) {
	if len(args) != 2 {
		return "", "", "", nil, fmt.Errorf("%s() takes exactly two service arguments", builtin)
	}
	svcA, ok := args[0].(*ServiceDef)
	if !ok {
		return "", "", "", nil, fmt.Errorf("%s() first argument must be a service, got %s", builtin, args[0].Type())
	}
	svcB, ok := args[1].(*ServiceDef)
	if !ok {
		return "", "", "", nil, fmt.Errorf("%s() second argument must be a service, got %s", builtin, args[1].Type())
	}

	dir = partitionBoth
	for _, kv := range kwargs {
		key, _ := starlark.AsString(kv[0])
		switch key {
		case "direction":
			s, _ := starlark.AsString(kv[1])
			d, e := parsePartitionDirection(s)
			if e != nil {
				return "", "", "", nil, fmt.Errorf("%s(): %w", builtin, e)
			}
			dir = d
		case "run":
			cb, ok := kv[1].(starlark.Callable)
			if !ok {
				return "", "", "", nil, fmt.Errorf("%s() run= must be a callable, got %s", builtin, kv[1].Type())
			}
			run = cb
		default:
			return "", "", "", nil, fmt.Errorf("%s(): unknown keyword argument %q; valid: direction, run", builtin, key)
		}
	}
	return svcA.Name, svcB.Name, dir, run, nil
}

// builtinPartition implements partition(a, b, direction=, run=).
func (rt *Runtime) builtinPartition(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	a, b, dir, run, err := parsePartitionArgs("partition", args, kwargs)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("partition() requires run= with a callback; " +
			"use partition_start()/partition_stop() to hold a partition across steps")
	}

	ap, err := rt.installPartition(thread, a, b, dir, fmt.Sprintf("partition %s|%s", a, b))
	if err != nil {
		return nil, err
	}
	defer ap.remove(rt)

	result, err := starlark.Call(thread, run, nil, nil)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return starlark.None, nil
	}
	return result, nil
}

// builtinPartitionStart implements partition_start(a, b, direction=).
//
// The run= form alone makes a multi-step scenario awkward, and the
// leadership-transfer failure this exists to reproduce is reached precisely by
// holding a partition open across several client calls.
func (rt *Runtime) builtinPartitionStart(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	a, b, dir, run, err := parsePartitionArgs("partition_start", args, kwargs)
	if err != nil {
		return nil, err
	}
	if run != nil {
		return nil, fmt.Errorf("partition_start() does not take run=; use partition() for a scoped window")
	}

	key := pairKey(a, b)
	rt.partitions.mu.Lock()
	_, exists := rt.partitions.active[key]
	rt.partitions.mu.Unlock()
	if exists {
		return nil, fmt.Errorf("partition_start(%s, %s): a partition between these services is already active", a, b)
	}

	ap, err := rt.installPartition(thread, a, b, dir, fmt.Sprintf("partition %s|%s", a, b))
	if err != nil {
		return nil, err
	}
	rt.partitions.mu.Lock()
	rt.partitions.active[key] = ap
	rt.partitions.mu.Unlock()
	return starlark.None, nil
}

// builtinPartitionStop implements partition_stop(a, b).
func (rt *Runtime) builtinPartitionStop(thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	if len(args) != 2 {
		return nil, fmt.Errorf("partition_stop() takes exactly two service arguments")
	}
	svcA, ok := args[0].(*ServiceDef)
	if !ok {
		return nil, fmt.Errorf("partition_stop() first argument must be a service, got %s", args[0].Type())
	}
	svcB, ok := args[1].(*ServiceDef)
	if !ok {
		return nil, fmt.Errorf("partition_stop() second argument must be a service, got %s", args[1].Type())
	}
	if len(kwargs) > 0 {
		key, _ := starlark.AsString(kwargs[0][0])
		return nil, fmt.Errorf("partition_stop(): unexpected keyword argument %q", key)
	}

	key := pairKey(svcA.Name, svcB.Name)
	rt.partitions.mu.Lock()
	ap := rt.partitions.active[key]
	delete(rt.partitions.active, key)
	rt.partitions.mu.Unlock()

	if ap == nil {
		return nil, fmt.Errorf("partition_stop(%s, %s): no partition is active between these services",
			svcA.Name, svcB.Name)
	}
	ap.remove(rt)
	return starlark.None, nil
}

// clearPartitions removes any partition left open at test end, so one test's
// partition cannot leak into the next.
func (rt *Runtime) clearPartitions() {
	rt.partitions.mu.Lock()
	active := rt.partitions.active
	rt.partitions.active = make(map[string]*activePartition)
	rt.partitions.mu.Unlock()

	names := make([]string, 0, len(active))
	for _, ap := range active {
		ap.remove(rt)
		names = append(names, ap.a+"|"+ap.b)
	}
	if len(names) > 0 {
		sort.Strings(names)
		rt.events.Emit("partition_leaked", "", map[string]string{
			"pairs":  strings.Join(names, ","),
			"detail": "partition_start() without a matching partition_stop(); removed at test end",
		})
	}
}
