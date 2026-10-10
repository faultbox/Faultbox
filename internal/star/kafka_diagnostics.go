package star

import (
	"sort"
	"strings"
)

type kafkaSnapshotSource struct{ snapshot func() []map[string]string }

func (rt *Runtime) kafkaSnapshotRegistration(service, iface string) func(func() []map[string]string) {
	key := service + "\x00" + iface
	source := &kafkaSnapshotSource{}
	return func(fn func() []map[string]string) {
		rt.kafkaSnapshotMu.Lock()
		defer rt.kafkaSnapshotMu.Unlock()
		if rt.kafkaSnapshots == nil {
			rt.kafkaSnapshots = make(map[string]*kafkaSnapshotSource)
		}
		if fn == nil {
			if rt.kafkaSnapshots[key] == source {
				delete(rt.kafkaSnapshots, key)
			}
		} else {
			source.snapshot = fn
			rt.kafkaSnapshots[key] = source
		}
	}
}

// Failure-only observation; never invokes Starlark subscriber callbacks or
// changes the verdict/readiness. Sources are copied before entering protocol locks.
func (rt *Runtime) recordKafkaDiagnostics(service, iface, group, consumer, reason string, topics []string) {
	rt.kafkaSnapshotMu.Lock()
	sources := map[string]func() []map[string]string{}
	for key, s := range rt.kafkaSnapshots {
		if service == "" || key == service+"\x00"+iface {
			sources[key] = s.snapshot
		}
	}
	rt.kafkaSnapshotMu.Unlock()
	wanted := map[string]bool{}
	for _, topic := range topics {
		wanted[topic] = true
	}
	keys := make([]string, 0, len(sources))
	for key := range sources {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fn := sources[key]
		broker, iface, _ := strings.Cut(key, "\x00")
		for _, f := range fn() {
			if f["ready"] == "true" || (group != "" && f["group"] != group) || (consumer != "" && f["source_service"] != consumer) {
				continue
			}
			if len(wanted) > 0 && f["topic"] != "" && !wanted[f["topic"]] {
				continue
			}
			f["interface"], f["trigger"] = iface, reason
			rt.events.recordOnly("mock.kafka.readiness_diagnostic", broker, f)
		}
	}
}
